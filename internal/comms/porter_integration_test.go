package comms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joaohts/comms/internal/porter"
	"golang.org/x/crypto/nacl/box"
)

const testPushToken = "ExponentPushToken[porter-test]"

// fakePusher records pushes instead of calling Expo.
type fakePusher struct {
	mu    sync.Mutex
	calls []map[string]string
	err   error
}

func (f *fakePusher) Push(_ context.Context, token string, data map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if token != testPushToken {
		return nil
	}
	f.calls = append(f.calls, data)
	return f.err
}

func (f *fakePusher) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }
func (f *fakePusher) call(i int) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}
func (f *fakePusher) fail(err error) { f.mu.Lock(); defer f.mu.Unlock(); f.err = err }

type porterFixture struct {
	mac, pi *nodeIntegrationFixture
	push    *fakePusher
	state   porter.Store
	subs    porter.Subscribers
	phone   OpenResponse
	recv    *nodeIntegrationReceiver
}

// newPorterFixture pairs a porter-enabled "mac" with a "pi" node standing in
// for the phone, and waits until mac:porter is discoverable.
func newPorterFixture(t *testing.T) *porterFixture {
	t.Helper()
	push := &fakePusher{}
	mac := newNodeIntegration(t, func(c *Config) { c.Porter = true; c.PorterPush = push })
	pi := newNodeIntegration(t, nil)
	pairNodeIntegration(t, mac, pi)
	dir := filepath.Join(mac.cfg.DataDir, "porter")
	f := &porterFixture{mac: mac, pi: pi, push: push, state: porter.Store{Path: filepath.Join(dir, "state.json")}, subs: porter.Subscribers{Path: filepath.Join(dir, "subscribers.json")}}
	f.phone = pi.open(t, "phone", "global", true)
	f.recv = pi.receiver(t, f.phone.Session)
	nodeIntegrationEventually(t, "mac:porter discoverable", func() bool {
		var who []Presence
		_, data, err := pi.request("GET", "/v1/who", nil)
		if err != nil || json.Unmarshal(data, &who) != nil {
			return false
		}
		for _, p := range who {
			if p.PeerAlias == "mac" && p.Alias == PorterAlias && p.Online {
				return true
			}
		}
		return false
	})
	return f
}

func (f *porterFixture) event(t *testing.T, e porter.Event) {
	t.Helper()
	if err := f.state.Update(func(s *porter.State) error { _, err := s.Apply(e); return err }); err != nil {
		t.Fatal(err)
	}
}

func (f *porterFixture) control(t *testing.T, body string) {
	t.Helper()
	m := f.pi.send(t, f.phone.Session.ID, "mac:"+PorterAlias, body, NewID("ctl_"))
	f.mac.waitState(t, f.pi.node.Store.Identity.MachineID, m.ID, "handed_off")
}

// next returns the next porter message body, acknowledging it.
func (f *porterFixture) next(t *testing.T, out any) string {
	t.Helper()
	m := f.recv.next(t)
	f.recv.handoff(t, m, "handed_off")
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(m.Body), &head); err != nil {
		t.Fatalf("porter body: %v: %s", err, m.Body)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(m.Body), out); err != nil {
			t.Fatal(err)
		}
	}
	return head.Type
}

func (f *porterFixture) subscribers(t *testing.T) map[string]porter.Subscriber {
	t.Helper()
	m, err := f.subs.Load()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPorterSubscribePublishAndEncryptedPush(t *testing.T) {
	f := newPorterFixture(t)
	f.event(t, porter.Event{Agent: "s1", Harness: "claude", Kind: porter.KindStart, Title: "notes"})
	f.control(t, `{"type":"porter.subscribe","push":{"provider":"expo","token":"`+testPushToken+`"}}`)
	var snap porter.Snapshot
	if typ := f.next(t, &snap); typ != porter.TypeSnapshot || snap.Machine != "mac" || len(snap.Agents) != 1 || snap.Agents[0].Title != "notes" {
		t.Fatalf("snapshot %s: %+v", typ, snap)
	}
	piID := f.pi.node.Store.Identity.MachineID
	if s := f.subscribers(t)[piID]; s.Agent != f.phone.Agent.ID || s.Push == nil || s.Push.Token != testPushToken {
		t.Fatalf("subscriber not stored by peer id: %+v", f.subscribers(t))
	}

	f.event(t, porter.Event{Agent: "s1", Kind: porter.KindNeeds, Needs: &porter.Needs{Kind: "permission", Text: "Bash: ls"}})
	var up porter.AgentUpdate
	if typ := f.next(t, &up); typ != porter.TypeAgent || up.Agent.Status != porter.StatusNeedsYou || up.Machine != "mac" {
		t.Fatalf("update %s: %+v", typ, up)
	}
	nodeIntegrationEventually(t, "needs_you push", func() bool { return f.push.count() == 1 })
	data := f.push.call(0)
	if data["from"] != f.mac.node.Store.Identity.MachineID {
		t.Fatalf("push from: %v", data)
	}
	sealed, _ := base64.StdEncoding.DecodeString(data["box"])
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	macPub, _ := key32(f.mac.node.Store.Identity.PublicKey)
	piPriv, _ := key32(f.pi.node.Store.Identity.PrivateKey)
	plain, ok := box.Open(nil, sealed[24:], &nonce, macPub, piPriv)
	if !ok {
		t.Fatal("phone cannot open push with its key and mac's pinned key")
	}
	var notice porter.Notice
	if err := json.Unmarshal(plain, &notice); err != nil || notice.Agent != "s1" || notice.Status != porter.StatusNeedsYou || notice.Needs.Text != "Bash: ls" || notice.Machine != "mac" {
		t.Fatalf("notice: %+v %v", notice, err)
	}

	// A normal agent's stop is published but not pushed.
	f.event(t, porter.Event{Agent: "s1", Kind: porter.KindStop})
	if typ := f.next(t, &up); typ != porter.TypeAgent || up.Agent.Status != porter.StatusDone {
		t.Fatalf("stop %s: %+v", typ, up)
	}
	f.event(t, porter.Event{Agent: "s1", Kind: porter.KindEnd})
	var rm porter.Remove
	if typ := f.next(t, &rm); typ != porter.TypeRemove || rm.ID != "s1" {
		t.Fatalf("remove %s: %+v", typ, rm)
	}
	if f.push.count() != 1 {
		t.Fatalf("unexpected pushes: %d", f.push.count())
	}

	// Re-subscribing (app reconnect) yields a fresh snapshot.
	f.control(t, `{"type":"porter.subscribe"}`)
	if typ := f.next(t, &snap); typ != porter.TypeSnapshot || len(snap.Agents) != 0 {
		t.Fatalf("resubscribe %s: %+v", typ, snap)
	}
	if f.subscribers(t)[piID].Push != nil {
		t.Fatal("token-less subscribe kept the old push token")
	}
	f.control(t, `{"type":"porter.unsubscribe"}`)
	if len(f.subscribers(t)) != 0 {
		t.Fatal("unsubscribe kept subscriber")
	}
	f.event(t, porter.Event{Agent: "s2", Kind: porter.KindPrompt})
	time.Sleep(200 * time.Millisecond)
	f.recv.empty(t)
}

func TestPorterRejectsLocalAndRevokedPeersAndDropsDeadTokens(t *testing.T) {
	f := newPorterFixture(t)
	f.push.fail(porter.ErrDeviceNotRegistered)
	macID, piID := f.mac.node.Store.Identity.MachineID, f.pi.node.Store.Identity.MachineID

	// Local agents read state.json directly; they cannot subscribe.
	local := f.mac.open(t, "local", "local", false)
	m := f.mac.send(t, local.Session.ID, PorterAlias, `{"type":"porter.subscribe"}`, "local_sub")
	f.mac.waitState(t, macID, m.ID, "handed_off")
	if len(f.subscribers(t)) != 0 {
		t.Fatal("local sender subscribed")
	}
	// Malformed controls are consumed and ignored.
	f.control(t, `{"type":"porter.subscribe","push":{"provider":"expo","token":"not-a-token"}}`)
	if len(f.subscribers(t)) != 0 {
		t.Fatal("invalid token subscribed")
	}

	f.control(t, `{"type":"porter.subscribe","push":{"provider":"expo","token":"`+testPushToken+`"}}`)
	f.next(t, nil)
	f.event(t, porter.Event{Agent: "s1", Kind: porter.KindNeeds})
	f.next(t, nil)
	nodeIntegrationEventually(t, "dead token forgotten", func() bool {
		s, ok := f.subscribers(t)[piID]
		return ok && s.Push == nil && f.push.count() == 1
	})

	// Revoking the grant drops the subscriber at the next publish.
	f.mac.call(t, "PUT", "/v1/grants/"+piID, map[string]bool{"allow_messages": false}, 200, nil)
	f.event(t, porter.Event{Agent: "s1", Kind: porter.KindPrompt})
	nodeIntegrationEventually(t, "revoked subscriber dropped", func() bool { return len(f.subscribers(t)) == 0 })
	f.recv.empty(t)
}
