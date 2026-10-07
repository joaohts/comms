package porter

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

const token = "ExponentPushToken[abc_DEF-123]"

func TestParse(t *testing.T) {
	c, err := Parse(`{"type":"porter.subscribe","push":{"provider":"expo","token":"` + token + `"},"future":1}`)
	if err != nil || c.Push == nil || c.Push.Token != token {
		t.Fatalf("subscribe: %+v %v", c, err)
	}
	if c, err = Parse(`{"type":"porter.subscribe"}`); err != nil || c.Push != nil || len(c.Topics) != 2 {
		t.Fatalf("token-less subscribe: %+v %v", c, err)
	}
	if c, err = Parse(`{"type":"porter.subscribe","topics":["trusts"]}`); err != nil || len(c.Topics) != 1 {
		t.Fatalf("topics: %+v %v", c, err)
	}
	if c, err = Parse(`{"type":"porter.future","x":1}`); err != nil || c.Type != "porter.future" {
		t.Fatalf("unknown type must parse: %+v %v", c, err)
	}
	if _, err = Parse(`hello`); err != ErrNotPorter {
		t.Fatalf("non-JSON: %v", err)
	}
	if c, err = Parse(`{"type":"porter.unsubscribe","push":{"provider":"x"}}`); err != nil || c.Push != nil {
		t.Fatalf("unsubscribe: %+v %v", c, err)
	}
	for _, bad := range []string{
		`{"type":"porter.set","agent":"x"}`,
		`{"type":"porter.revoke"}`,
		`{"type":"porter.answer","id":"ntf_1"}`,
		`{"type":"porter.subscribe","push":{"provider":"fcm","token":"` + token + `"}}`,
		`{"type":"porter.subscribe","push":{"provider":"expo","token":"https://evil"}}`,
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestDiffAndPushWanted(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(0)})
	mustApply(t, s, Event{Agent: "b", Kind: KindPrompt, At: at(0)})
	prev := map[string]*Agent{}
	for id, a := range s.Agents {
		c := *a
		prev[id] = &c
	}
	mustApply(t, s, Event{Agent: "a", Kind: KindNeeds, At: at(5), Needs: &Needs{Kind: "permission", Text: "Bash: ls"}})
	delete(s.Agents, "b")
	mustApply(t, s, Event{Agent: "c", Kind: KindStart, At: at(5)})
	changed, removed := Diff(prev, s.Agents)
	if len(changed) != 2 || len(removed) != 1 || removed[0] != "b" {
		t.Fatalf("changed=%+v removed=%v", changed, removed)
	}
	if !PushWanted(prev["a"], *s.Agents["a"]) {
		t.Fatal("needs_you must push")
	}
	if PushWanted(nil, *s.Agents["c"]) {
		t.Fatal("fresh session must not push")
	}
	// Normal agents do not push on stop; special agents do, on every stop.
	before := *s.Agents["a"]
	mustApply(t, s, Event{Agent: "a", Kind: KindStop, At: at(9)})
	if PushWanted(&before, *s.Agents["a"]) {
		t.Fatal("normal stop pushed")
	}
	yes := true
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(10), Special: &yes})
	before = *s.Agents["a"]
	mustApply(t, s, Event{Agent: "a", Kind: KindStop, At: at(12)})
	if !PushWanted(&before, *s.Agents["a"]) {
		t.Fatal("special stop did not push")
	}
	// A prompt+stop missed between two polls still counts as a new stop.
	before = *s.Agents["a"]
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(13)})
	mustApply(t, s, Event{Agent: "a", Kind: KindStop, At: at(14)})
	if !PushWanted(&before, *s.Agents["a"]) {
		t.Fatal("missed stop did not push")
	}
	before = *s.Agents["a"]
	mustApply(t, s, Event{Agent: "a", Kind: KindUpdate, At: at(15), Summary: "x"})
	if PushWanted(&before, *s.Agents["a"]) {
		t.Fatal("metadata update pushed")
	}
}

func TestSnapshotTruncatesToLimit(t *testing.T) {
	s := NewState()
	for i := 0; i < 50; i++ {
		mustApply(t, s, Event{Agent: string(rune('A' + i)), Kind: KindPrompt, At: at(i), Summary: strings.Repeat("x", 200)})
	}
	mustApply(t, s, Event{Agent: "needs", Kind: KindNeeds, At: at(0)})
	snap := NewSnapshot("mac", TopicAgents, AgentItems(s), 4096)
	b, _ := json.Marshal(snap)
	if !snap.Truncated || len(b) > 4096 || snap.Items[0].(Agent).ID != "needs" || snap.Type != TypeSnapshot || snap.Topic != TopicAgents || snap.V != 1 {
		t.Fatalf("truncated=%v size=%d first=%+v", snap.Truncated, len(b), snap.Items[0])
	}
	if full := NewSnapshot("mac", TopicAgents, AgentItems(s), 1<<20); full.Truncated || len(full.Items) != 51 {
		t.Fatal("unexpected truncation")
	}
}

func TestSubscribersRoundTrip(t *testing.T) {
	f := Subscribers{Path: filepath.Join(t.TempDir(), "porter", "subscribers.json")}
	m, err := f.Load()
	if err != nil || len(m) != 0 {
		t.Fatalf("empty load: %v %v", m, err)
	}
	m["m_phone"] = Subscriber{Peer: "m_phone", Agent: "a_1", Topics: []string{TopicAgents}, Push: &Push{"expo", token}, Since: t0}
	if err = f.Save(m); err != nil {
		t.Fatal(err)
	}
	got, err := f.Load()
	if err != nil || got["m_phone"].Push.Token != token || got["m_phone"].Agent != "a_1" {
		t.Fatalf("reload: %+v %v", got, err)
	}
}

func TestPushDataIsAuthenticatedBoxUnderLimit(t *testing.T) {
	phonePub, phonePriv, _ := box.GenerateKey(rand.Reader)
	nodePub, nodePriv, _ := box.GenerateKey(rand.Reader)
	a := Agent{ID: "sess", Title: strings.Repeat("t", 1000), Status: StatusNeedsYou, Summary: strings.Repeat("s", 5000), Needs: &Needs{Kind: "permission", Text: strings.Repeat("n", 5000)}}
	data, err := PushData(NoticeFor("mac", a), "m_mac", phonePub[:], nodePriv[:])
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(data); len(b) > MaxPushData || data["from"] != "m_mac" {
		t.Fatalf("data: %d bytes %v", len(b), data["from"])
	}
	sealed, _ := base64.StdEncoding.DecodeString(data["box"])
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	plain, ok := box.Open(nil, sealed[24:], &nonce, nodePub, phonePriv)
	if !ok {
		t.Fatal("phone cannot open box with the node's pinned key")
	}
	var n Notice
	if err = json.Unmarshal(plain, &n); err != nil || n.Kind != "agent" || n.Machine != "mac" || n.Agent != "sess" || n.Status != StatusNeedsYou || n.Needs == nil {
		t.Fatalf("notice: %+v %v", n, err)
	}
	// Another sender's key must not open it: the phone drops forged pushes.
	otherPub, _, _ := box.GenerateKey(rand.Reader)
	if _, ok := box.Open(nil, sealed[24:], &nonce, otherPub, phonePriv); ok {
		t.Fatal("box opened with the wrong sender key")
	}
}

func TestExpoClient(t *testing.T) {
	reply := `{"data":[{"status":"ok","id":"x"}]}`
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		got = nil
		json.Unmarshal(b, &got)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	e := Expo{URL: srv.URL, HTTP: srv.Client()}
	data := map[string]string{"porter": "1", "from": "m_mac", "box": "AAAA"}
	if err := e.Push(context.Background(), token, data); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["to"] != token || got[0]["priority"] != "high" || got[0]["title"] != nil || got[0]["body"] != nil {
		t.Fatalf("expo message: %+v", got)
	}
	if d := got[0]["data"].(map[string]any); d["box"] != "AAAA" {
		t.Fatalf("data: %+v", d)
	}
	reply = `{"data":[{"status":"error","message":"gone","details":{"error":"DeviceNotRegistered"}}]}`
	if err := e.Push(context.Background(), token, data); !errors.Is(err, ErrDeviceNotRegistered) {
		t.Fatalf("want ErrDeviceNotRegistered, got %v", err)
	}
	reply = `{"data":[{"status":"error","message":"slow down","details":{"error":"MessageRateExceeded"}}]}`
	if err := e.Push(context.Background(), token, data); err == nil || errors.Is(err, ErrDeviceNotRegistered) {
		t.Fatalf("rate error: %v", err)
	}
}
