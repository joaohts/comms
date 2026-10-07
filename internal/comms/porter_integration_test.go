package comms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
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

type porterNode struct {
	*nodeIntegrationFixture
	push   *fakePusher
	state  porter.Store
	subs   porter.Subscribers
	trusts string
	config string
}

type porterFixture struct {
	mac   *porterNode
	pi    *porterNode // a second porter machine
	phone *nodeIntegrationFixture
	app   OpenResponse
	recv  *nodeIntegrationReceiver
}

func newPorterNode(t *testing.T, change func(*Config)) *porterNode {
	t.Helper()
	push := &fakePusher{}
	cfgPath := filepath.Join(t.TempDir(), "porter", "config.toml")
	f := newNodeIntegration(t, func(c *Config) {
		c.Porter, c.PorterPush, c.PorterConfig = true, push, cfgPath
		c.PorterStatusInterval = time.Hour
		c.PorterResubscribe = 2 * time.Second
		if change != nil {
			change(c)
		}
	})
	dir := filepath.Join(f.cfg.DataDir, "porter")
	return &porterNode{nodeIntegrationFixture: f, push: push, state: porter.Store{Path: filepath.Join(dir, "state.json")},
		subs: porter.Subscribers{Path: filepath.Join(dir, "subscribers.json")}, trusts: filepath.Join(dir, "trusts.json"), config: cfgPath}
}

// pairAll pairs and mutually grants every node through one broker.
func pairAll(t *testing.T, nodes map[string]*nodeIntegrationFixture) {
	t.Helper()
	broker := testBroker(t, nil)
	for name, n := range nodes {
		n.call(t, "PUT", "/v1/name", map[string]string{"name": name}, 200, nil)
	}
	for _, from := range nodes {
		for alias, to := range nodes {
			if from == to {
				continue
			}
			id := to.node.Store.Identity
			from.call(t, "PUT", "/v1/peers/"+id.MachineID, Peer{MachineID: id.MachineID, Alias: alias, PublicKey: id.PublicKey}, 200, nil)
			from.call(t, "PUT", "/v1/grants/"+id.MachineID, map[string]bool{"allow_messages": true}, 200, nil)
		}
		from.call(t, "PUT", "/v1/broker", map[string]string{"url": broker.server.URL}, 200, nil)
	}
	nodeIntegrationEventually(t, "all nodes connected and grants published", func() bool {
		for _, a := range nodes {
			if !a.node.brokerOnline.Load() {
				return false
			}
			for _, b := range nodes {
				if a == b {
					continue
				}
				ok, e := broker.b.hasGrant(context.Background(), a.node.Store.Identity.MachineID, b.node.Store.Identity.MachineID, false)
				if e != nil || !ok {
					return false
				}
			}
		}
		return true
	})
}

func discoverable(t *testing.T, from *nodeIntegrationFixture, peer, alias string) {
	t.Helper()
	nodeIntegrationEventually(t, peer+":"+alias+" discoverable", func() bool {
		var who []Presence
		_, data, err := from.request("GET", "/v1/who", nil)
		if err != nil || json.Unmarshal(data, &who) != nil {
			return false
		}
		for _, p := range who {
			if p.PeerAlias == peer && p.Alias == alias && p.Online {
				return true
			}
		}
		return false
	})
}

// newPorterFixture pairs porter machines "mac" and "pi" with a plain "phone"
// node running the app agent. The phone is the approver on both machines.
func newPorterFixture(t *testing.T) *porterFixture {
	t.Helper()
	f := &porterFixture{mac: newPorterNode(t, nil), pi: newPorterNode(t, nil), phone: newNodeIntegration(t, nil)}
	pairAll(t, map[string]*nodeIntegrationFixture{"mac": f.mac.nodeIntegrationFixture, "pi": f.pi.nodeIntegrationFixture, "phone": f.phone})
	for _, n := range []*porterNode{f.mac, f.pi} {
		c := porter.DefaultConfig()
		c.Approvers = []string{f.phone.node.Store.Identity.MachineID}
		if err := porter.SaveConfig(n.config, c); err != nil {
			t.Fatal(err)
		}
	}
	f.app = f.phone.open(t, AppAlias, "global", true)
	f.recv = f.phone.receiver(t, f.app.Session)
	discoverable(t, f.phone, "mac", PorterAlias)
	discoverable(t, f.mac.nodeIntegrationFixture, "phone", AppAlias)
	return f
}

func (n *porterNode) event(t *testing.T, e porter.Event) {
	t.Helper()
	if err := n.state.Update(func(s *porter.State) error { _, err := s.Apply(e); return err }); err != nil {
		t.Fatal(err)
	}
}

func (f *porterFixture) control(t *testing.T, machine, body string) {
	t.Helper()
	m := f.phone.send(t, f.app.Session.ID, machine+":"+PorterAlias, body, NewID("ctl_"))
	waitHandedOff(t, f.phone, m.ID)
}

// waitHandedOff is waitState with room for -race on three nodes.
func waitHandedOff(t *testing.T, n *nodeIntegrationFixture, id string) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < 20*time.Second {
		if got, err := n.node.Store.Message(n.node.Store.Identity.MachineID, id); err == nil && got.State == "handed_off" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("message %s not handed off", id)
}

// next returns the next porter message to the phone, acknowledging it.
func (f *porterFixture) next(t *testing.T, out any) (string, string) {
	t.Helper()
	m := f.recv.next(t)
	f.recv.handoff(t, m, "handed_off")
	var head struct {
		Type  string `json:"type"`
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal([]byte(m.Body), &head); err != nil {
		t.Fatalf("porter body: %v: %s", err, m.Body)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(m.Body), out); err != nil {
			t.Fatal(err)
		}
	}
	return head.Type, head.Topic
}

// expect skips status traffic and returns the next message of type/topic.
func (f *porterFixture) expect(t *testing.T, typ, topic string, out any) {
	t.Helper()
	for i := 0; i < 20; i++ {
		var raw json.RawMessage
		gotType, gotTopic := f.next(t, &raw)
		if gotTopic == porter.TopicStatus && topic != porter.TopicStatus {
			continue
		}
		if gotType != typ || gotTopic != topic {
			t.Fatalf("got %s/%s, want %s/%s: %s", gotType, gotTopic, typ, topic, raw)
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	t.Fatalf("no %s/%s", typ, topic)
}

func (n *porterNode) subscribers(t *testing.T) map[string]porter.Subscriber {
	t.Helper()
	m, err := n.subs.Load()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func openBox(t *testing.T, data map[string]string, sender, recipient Identity) map[string]any {
	t.Helper()
	if data["from"] != sender.MachineID || data["porter"] != "1" {
		t.Fatalf("push data: %v", data)
	}
	sealed, _ := base64.StdEncoding.DecodeString(data["box"])
	var nonce [24]byte
	copy(nonce[:], sealed[:24])
	pub, _ := key32(sender.PublicKey)
	priv, _ := key32(recipient.PrivateKey)
	plain, ok := box.Open(nil, sealed[24:], &nonce, pub, priv)
	if !ok {
		t.Fatal("phone cannot open push with its key and the sender's pinned key")
	}
	var v map[string]any
	if err := json.Unmarshal(plain, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPorterSubscribeTopicsPublishAndEncryptedPush(t *testing.T) {
	f := newPorterFixture(t)
	mac := f.mac
	mac.event(t, porter.Event{Agent: "s1", Harness: "claude", Kind: porter.KindStart, Title: "notes"})
	f.control(t, "mac", `{"type":"porter.subscribe","v":1,"topics":["agents","status","trusts","location"],"push":{"provider":"expo","token":"`+testPushToken+`"}}`)
	var sub porter.Subscribed
	if typ, _ := f.next(t, &sub); typ != porter.TypeSubscribed || sub.Machine != "mac" || !sub.Approver || !sub.Push || sub.Version != Version ||
		strings.Join(sub.Topics, ",") != "agents,status,trusts" || strings.Join(sub.Refused, ",") != "location" {
		t.Fatalf("subscribed: %+v", sub)
	}
	var snap struct {
		Machine string         `json:"machine"`
		Items   []porter.Agent `json:"items"`
	}
	f.expect(t, porter.TypeSnapshot, porter.TopicAgents, &snap)
	if snap.Machine != "mac" || len(snap.Items) != 1 || snap.Items[0].Title != "notes" {
		t.Fatalf("agents snapshot: %+v", snap)
	}
	var st struct {
		Value porter.Status `json:"value"`
	}
	f.expect(t, porter.TypeSnapshot, porter.TopicStatus, &st)
	if st.Value.Host != "mac" || st.Value.CommsVersion != Version || st.Value.DiskPct <= 0 {
		t.Fatalf("status snapshot: %+v", st.Value)
	}
	f.expect(t, porter.TypeSnapshot, porter.TopicTrusts, nil)
	phoneID := f.phone.node.Store.Identity.MachineID
	if s := mac.subscribers(t)[phoneID]; s.Agent != f.app.Agent.ID || s.Push == nil || len(s.Topics) != 4 {
		t.Fatalf("subscriber not stored by peer id: %+v", mac.subscribers(t))
	}

	mac.event(t, porter.Event{Agent: "s1", Kind: porter.KindNeeds, Needs: &porter.Needs{Kind: "permission", Text: "Bash: ls"}})
	var up struct {
		Machine string       `json:"machine"`
		Item    porter.Agent `json:"item"`
	}
	f.expect(t, porter.TypeUpdate, porter.TopicAgents, &up)
	if up.Item.Status != porter.StatusNeedsYou || up.Machine != "mac" {
		t.Fatalf("update: %+v", up)
	}
	nodeIntegrationEventually(t, "needs_you push", func() bool { return mac.push.count() == 1 })
	notice := openBox(t, mac.push.call(0), mac.node.Store.Identity, f.phone.node.Store.Identity)
	if notice["kind"] != "agent" || notice["agent"] != "s1" || notice["status"] != porter.StatusNeedsYou || notice["machine"] != "mac" {
		t.Fatalf("notice: %+v", notice)
	}

	// running → status topic update on agents_running change.
	mac.event(t, porter.Event{Agent: "s1", Kind: porter.KindPrompt})
	f.expect(t, porter.TypeUpdate, porter.TopicAgents, nil)
	var stUp struct {
		Value porter.Status `json:"value"`
	}
	f.expect(t, porter.TypeUpdate, porter.TopicStatus, &stUp)
	if stUp.Value.AgentsRunning != 1 {
		t.Fatalf("status update: %+v", stUp.Value)
	}
	// Ending moves the agent to the graveyard: an update, not a remove.
	mac.event(t, porter.Event{Agent: "s1", Kind: porter.KindEnd})
	f.expect(t, porter.TypeUpdate, porter.TopicAgents, &up)
	if up.Item.Status != porter.StatusEnded || up.Item.EndReason != porter.EndClosed || up.Item.EndedAt == nil {
		t.Fatalf("graveyard: %+v", up.Item)
	}
	// Purged agents are removed.
	if err := mac.state.Update(func(s *porter.State) error { delete(s.Agents, "s1"); return nil }); err != nil {
		t.Fatal(err)
	}
	var rm porter.Remove
	f.expect(t, porter.TypeRemove, porter.TopicAgents, &rm)
	if rm.ID != "s1" || rm.V != 1 {
		t.Fatalf("remove: %+v", rm)
	}
	if mac.push.count() != 1 {
		t.Fatalf("unexpected pushes: %d", mac.push.count())
	}

	// Unshared topics are refused; a re-subscribe replaces the previous one.
	c := porter.DefaultConfig()
	c.Approvers = []string{phoneID}
	c.Share = map[string][]string{porter.TopicAgents: {"m_someone_else"}}
	porter.SaveConfig(mac.config, c)
	f.control(t, "mac", `{"type":"porter.subscribe","v":1,"topics":["agents","status"]}`)
	f.next(t, &sub)
	if len(sub.Topics) != 0 || len(sub.Refused) != 2 || sub.Push {
		t.Fatalf("refused: %+v", sub)
	}
	if mac.subscribers(t)[phoneID].Push != nil {
		t.Fatal("token-less subscribe kept the old push token")
	}
	mac.event(t, porter.Event{Agent: "s2", Kind: porter.KindPrompt})
	time.Sleep(200 * time.Millisecond)
	f.recv.empty(t)
	f.control(t, "mac", `{"type":"porter.unsubscribe","v":1}`)
	if _, ok := mac.subscribers(t)[phoneID]; ok {
		t.Fatal("unsubscribe kept subscriber")
	}
}

func TestPorterRejectsLocalAndRevokedPeersAndDropsDeadTokens(t *testing.T) {
	f := newPorterFixture(t)
	mac := f.mac
	mac.push.fail(porter.ErrDeviceNotRegistered)
	macID, phoneID := mac.node.Store.Identity.MachineID, f.phone.node.Store.Identity.MachineID

	// Local agents read state.json directly; they cannot subscribe.
	local := mac.open(t, "local", "local", false)
	m := mac.send(t, local.Session.ID, PorterAlias, `{"type":"porter.subscribe"}`, "local_sub")
	mac.waitState(t, macID, m.ID, "handed_off")
	if _, ok := mac.subscribers(t)[macID]; ok {
		t.Fatal("local sender subscribed")
	}
	// Malformed controls are consumed and ignored.
	f.control(t, "mac", `{"type":"porter.subscribe","push":{"provider":"expo","token":"not-a-token"}}`)
	f.control(t, "mac", `not json`)
	if _, ok := mac.subscribers(t)[phoneID]; ok {
		t.Fatal("invalid token subscribed")
	}

	f.control(t, "mac", `{"type":"porter.subscribe","topics":["agents"],"push":{"provider":"expo","token":"`+testPushToken+`"}}`)
	f.expect(t, porter.TypeSubscribed, "", nil)
	f.expect(t, porter.TypeSnapshot, porter.TopicAgents, nil)
	mac.event(t, porter.Event{Agent: "s1", Kind: porter.KindNeeds})
	f.expect(t, porter.TypeUpdate, porter.TopicAgents, nil)
	nodeIntegrationEventually(t, "dead token forgotten", func() bool {
		s, ok := mac.subscribers(t)[phoneID]
		return ok && s.Push == nil && mac.push.count() == 1
	})

	// Revoking the grant drops the subscriber at the next publish.
	mac.call(t, "PUT", "/v1/grants/"+phoneID, map[string]bool{"allow_messages": false}, 200, nil)
	mac.event(t, porter.Event{Agent: "s1", Kind: porter.KindPrompt})
	nodeIntegrationEventually(t, "revoked subscriber dropped", func() bool { _, ok := mac.subscribers(t)[phoneID]; return !ok })
	f.recv.empty(t)
}

func TestPorterControlIsApproverOnly(t *testing.T) {
	f := newPorterFixture(t)
	mac := f.mac
	mac.event(t, porter.Event{Agent: "s1", Kind: porter.KindPrompt})
	porter.UpdateJSON(mac.trusts, func(ts *porter.Trusts) (bool, error) {
		ts.Put(porter.Trust{ID: "tr_x", ReceiverID: "a_r", SenderID: "m:a", Scope: porter.TrustAlways})
		return true, nil
	})
	// The pi is granted but not an approver: ignored.
	piApp := f.pi.open(t, "operator", "global", false)
	for _, body := range []string{`{"type":"porter.set","v":1,"agent":"s1","special":true}`, `{"type":"porter.revoke","v":1,"trust":"tr_x"}`} {
		m := f.pi.send(t, piApp.Session.ID, "mac:"+PorterAlias, body, NewID("ctl_"))
		waitHandedOff(t, f.pi.nodeIntegrationFixture, m.ID)
	}
	st, _ := mac.state.Load()
	ts, _ := porter.LoadJSON[porter.Trusts](mac.trusts)
	if st.Agents["s1"].Special || len(ts.Trusts) != 1 {
		t.Fatal("non-approver changed state")
	}
	// The phone is: applied, and visible as topic updates.
	f.control(t, "mac", `{"type":"porter.subscribe","v":1,"topics":["agents","trusts"]}`)
	f.expect(t, porter.TypeSubscribed, "", nil)
	f.expect(t, porter.TypeSnapshot, porter.TopicAgents, nil)
	f.expect(t, porter.TypeSnapshot, porter.TopicTrusts, nil)
	f.control(t, "mac", `{"type":"porter.set","v":1,"agent":"s1","special":true}`)
	var up struct {
		Item porter.Agent `json:"item"`
	}
	f.expect(t, porter.TypeUpdate, porter.TopicAgents, &up)
	if !up.Item.Special {
		t.Fatalf("set: %+v", up.Item)
	}
	f.control(t, "mac", `{"type":"porter.revoke","v":1,"trust":"tr_x"}`)
	var rm porter.Remove
	f.expect(t, porter.TypeRemove, porter.TopicTrusts, &rm)
	if rm.ID != "tr_x" {
		t.Fatalf("revoke: %+v", rm)
	}
	// A special agent's stop is pushed... only with a token; none here.
	if mac.push.count() != 0 {
		t.Fatal("push without token")
	}
}

func TestPorterNotifyContextPushAndForwardedAnswer(t *testing.T) {
	f := newPorterFixture(t)
	mac := f.mac
	f.control(t, "mac", `{"type":"porter.subscribe","v":1,"topics":[],"push":{"provider":"expo","token":"`+testPushToken+`"}}`)
	f.expect(t, porter.TypeSubscribed, "", nil)
	mcp := mac.open(t, "mcp", "local", true)
	agentRecv := mac.receiver(t, mcp.Session)

	// A reason is required.
	mac.call(t, "POST", "/v1/porter/notify", PorterNotifyRequest{SessionID: mcp.Session.ID, To: "phone", Title: "Deploy"}, 400, nil)
	var out map[string]string
	mac.call(t, "POST", "/v1/porter/notify", PorterNotifyRequest{SessionID: mcp.Session.ID, To: "phone", Title: "Deploy finished", Body: "pager-api v2 is live", Reason: "you asked to be told", Ask: []string{"Nice", "Roll back"}}, 202, &out)
	var nt porter.Notify
	if typ, _ := f.next(t, &nt); typ != porter.TypeNotify {
		t.Fatalf("type %s", typ)
	}
	if nt.ID != out["id"] || nt.Kind != porter.NotifyMessage || nt.Reason != "you asked to be told" || nt.Priority != "normal" || nt.ExpiresAt == nil ||
		nt.Context.Machine != "mac" || nt.Context.Agent != "mac:mcp" || nt.Context.Harness != "service" || nt.Context.Title != "mcp" {
		t.Fatalf("notify: %+v", nt)
	}
	nodeIntegrationEventually(t, "notify push", func() bool { return mac.push.count() == 1 })
	plain := openBox(t, mac.push.call(0), mac.node.Store.Identity, f.phone.node.Store.Identity)
	if plain["kind"] != "notify" || plain["notify"].(map[string]any)["id"] != nt.ID {
		t.Fatalf("push plaintext: %+v", plain)
	}
	// A non-approver's answer does not count; the approver's is forwarded.
	piSender := f.pi.open(t, "joana", "global", false)
	m := f.pi.send(t, piSender.Session.ID, "mac:"+PorterAlias, `{"type":"porter.answer","v":1,"id":"`+nt.ID+`","choice":"Nice"}`, NewID("ans_"))
	waitHandedOff(t, f.pi.nodeIntegrationFixture, m.ID)
	f.control(t, "mac", `{"type":"porter.answer","v":1,"id":"`+nt.ID+`","choice":"Not a choice"}`)
	agentRecv.empty(t)
	f.control(t, "mac", `{"type":"porter.answer","v":1,"id":"`+nt.ID+`","choice":"Roll back"}`)
	got := agentRecv.next(t)
	agentRecv.handoff(t, got, "handed_off")
	var ans porter.Answer
	json.Unmarshal([]byte(got.Body), &ans)
	if ans.Type != porter.TypeAnswer || ans.ID != nt.ID || ans.Choice != "Roll back" {
		t.Fatalf("forwarded answer: %s", got.Body)
	}
	// A second answer to a settled question is ignored.
	f.control(t, "mac", `{"type":"porter.answer","v":1,"id":"`+nt.ID+`","choice":"Nice"}`)
	agentRecv.empty(t)
	// --ask needs an identity to answer to.
	mac.call(t, "POST", "/v1/porter/notify", PorterNotifyRequest{To: "phone", Title: "x", Reason: "y", Ask: []string{"A"}}, 400, nil)
	mac.call(t, "POST", "/v1/porter/notify", PorterNotifyRequest{To: "phone", Title: "plain", Reason: "cron"}, 202, nil)
	nt = porter.Notify{}
	f.next(t, &nt)
	if nt.Title != "plain" || nt.Context.Machine != "mac" || nt.Context.Agent != "" {
		t.Fatalf("sessionless context: %s %+v", nt.Title, nt.Context)
	}
}

func TestPorterAskAnswerTimeoutAndTerminal(t *testing.T) {
	f := newPorterFixture(t)
	mac := f.mac
	mac.event(t, porter.Event{Agent: "claude-1", Harness: "claude", Kind: porter.KindNeeds, Project: "notes", Title: "pager"})
	var q PorterQuestion
	mac.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "permission", Text: "Bash: npm install", Agent: "claude-1", TimeoutMS: 5000}, 202, &q)
	var nt porter.Notify
	f.next(t, &nt)
	if nt.ID != q.ID || nt.Kind != porter.NotifyPermission || strings.Join(nt.Ask, ",") != "Allow,Deny" || nt.Body != "Bash: npm install" ||
		nt.Context.Session != "claude-1" || nt.Context.Project != "notes" || nt.Context.Title != "pager" || nt.Context.Harness != "claude" || nt.Priority != "high" {
		t.Fatalf("permission notify: %+v", nt)
	}
	var pending PorterQuestion
	mac.call(t, "GET", "/v1/porter/questions/"+q.ID+"?wait_ms=10", nil, 200, &pending)
	if pending.State != "pending" {
		t.Fatalf("pending: %+v", pending)
	}
	go f.control(t, "mac", `{"type":"porter.answer","v":1,"id":"`+q.ID+`","choice":"Allow"}`)
	var done PorterQuestion
	mac.call(t, "GET", "/v1/porter/questions/"+q.ID+"?wait_ms=4000", nil, 200, &done)
	if done.State != "answered" || done.Choice != "Allow" || done.By != f.phone.node.Store.Identity.MachineID {
		t.Fatalf("answered: %+v", done)
	}

	// Timeout: the asker is told, the phone gets a cancel.
	mac.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "permission", Text: "x", TimeoutMS: 300}, 202, &q)
	f.next(t, &nt)
	mac.call(t, "GET", "/v1/porter/questions/"+q.ID+"?wait_ms=2000", nil, 200, &done)
	if done.State != "timeout" {
		t.Fatalf("timeout: %+v", done)
	}
	var cancel porter.Cancel
	if typ, _ := f.next(t, &cancel); typ != porter.TypeCancel || cancel.ID != q.ID || cancel.Resolution != porter.ResolvedTimeout {
		t.Fatalf("cancel: %+v", cancel)
	}
	// An answer after expiry does not count.
	f.control(t, "mac", `{"type":"porter.answer","v":1,"id":"`+q.ID+`","choice":"Allow"}`)
	mac.call(t, "GET", "/v1/porter/questions/"+q.ID, nil, 200, &done)
	if done.State != "timeout" {
		t.Fatalf("late answer counted: %+v", done)
	}

	// Terminal: the hook gave up; the phone's question is cancelled.
	mac.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "permission", Text: "y"}, 202, &q)
	f.next(t, &nt)
	mac.call(t, "DELETE", "/v1/porter/questions/"+q.ID, nil, 200, &done)
	if done.State != "cancelled" {
		t.Fatalf("terminal: %+v", done)
	}
	if typ, _ := f.next(t, &cancel); typ != porter.TypeCancel || cancel.Resolution != porter.ResolvedTerminal {
		t.Fatalf("terminal cancel: %+v", cancel)
	}

	// Without approvers there is nobody to ask.
	porter.SaveConfig(mac.config, porter.DefaultConfig())
	mac.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "permission", Text: "z"}, 409, nil)
	// Porter off: the API says so.
	plain := newNodeIntegration(t, nil)
	plain.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "permission", Text: "z"}, 409, nil)
}

func TestPorterTrustQuestionStoreAndLabels(t *testing.T) {
	f := newPorterFixture(t)
	mac := f.mac
	joana := mac.open(t, "joana", "global", true)
	joanaRecv := mac.receiver(t, joana.Session)
	secretary := f.pi.open(t, "secretary", "global", false)
	f.pi.receiver(t, secretary.Session)
	stranger := f.pi.open(t, "stranger", "global", false)
	discoverable(t, f.pi.nodeIntegrationFixture, "mac", "joana")
	discoverable(t, mac.nodeIntegrationFixture, "pi", "secretary")
	sender := f.pi.node.Store.Identity.MachineID + ":" + secretary.Agent.ID

	first := f.pi.send(t, secretary.Session.ID, "mac:joana", "deploy please", NewID("o_"))
	got := joanaRecv.next(t)
	joanaRecv.handoff(t, got, "handed_off")
	if got.ID != first.ID || got.Trust != "untrusted" || ContentForPeer(got).Trust != "untrusted" {
		t.Fatalf("label: %+v", got)
	}

	mac.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "trust", SessionID: joana.Session.ID, Sender: "pi:nonsense"}, 400, nil)
	var q PorterQuestion
	mac.call(t, "POST", "/v1/porter/questions", PorterQuestionRequest{Kind: "trust", SessionID: joana.Session.ID, Sender: sender, Text: "deploy please"}, 202, &q)
	var nt porter.Notify
	f.next(t, &nt)
	if nt.Kind != porter.NotifyTrust || nt.Trust == nil || nt.Trust.Receiver != "mac:joana" || nt.Trust.Sender != "pi:secretary" || len(nt.Ask) != 4 {
		t.Fatalf("trust notify: %+v %+v", nt, nt.Trust)
	}
	f.control(t, "mac", `{"type":"porter.answer","v":1,"id":"`+q.ID+`","choice":"This session"}`)
	var done PorterQuestion
	mac.call(t, "GET", "/v1/porter/questions/"+q.ID+"?wait_ms=4000", nil, 200, &done)
	if done.Choice != "This session" {
		t.Fatalf("answer: %+v", done)
	}
	ts, _ := porter.LoadJSON[porter.Trusts](mac.trusts)
	l := ts.List()
	if len(l) != 1 || l[0].Scope != porter.TrustSession || l[0].SenderID != sender || l[0].ReceiverID != joana.Agent.ID || l[0].Receiver != "mac:joana" {
		t.Fatalf("stored: %+v", l)
	}

	f.pi.send(t, secretary.Session.ID, "mac:joana", "now trusted", NewID("o_"))
	got = joanaRecv.next(t)
	joanaRecv.handoff(t, got, "handed_off")
	if got.Trust != "trusted: session" {
		t.Fatalf("trusted label: %+v", got.Trust)
	}
	f.pi.send(t, stranger.Session.ID, "mac:joana", "me too", NewID("o_"))
	got = joanaRecv.next(t)
	joanaRecv.handoff(t, got, "handed_off")
	if got.Trust != "untrusted" {
		t.Fatalf("stranger label: %+v", got.Trust)
	}

	// The session trust dies with the sender's ephemeral identity.
	f.pi.call(t, "DELETE", "/v1/sessions/"+secretary.Session.ID, nil, 200, nil)
	b := mac.node.porter
	nodeIntegrationEventually(t, "sender gone from directory", func() bool {
		var who []Presence
		mac.node.brokerRequest(context.Background(), "GET", "/v1/who", nil, &who)
		for _, p := range who {
			if p.AgentID == secretary.Agent.ID {
				return false
			}
		}
		return true
	})
	b.n.cfg.PorterTrustGrace = time.Millisecond
	b.pruneSessionTrusts(time.Now())
	b.pruneSessionTrusts(time.Now().Add(time.Second))
	ts, _ = porter.LoadJSON[porter.Trusts](mac.trusts)
	if len(ts.Trusts) != 0 {
		t.Fatalf("session trust survived its sender: %+v", ts.Trusts)
	}
}

func TestPorterLinksIdentityAndCachesPeers(t *testing.T) {
	f := newPorterFixture(t)
	mac, pi := f.mac, f.pi
	// A Claude session whose comms identity is open links to it.
	var claude OpenResponse
	mac.call(t, "POST", "/v1/sessions", OpenRequest{Alias: "pager-refactor", Scope: "global", Persistent: true, Harness: "claude", HarnessID: "claude-sess-1"}, 200, &claude)
	mac.event(t, porter.Event{Agent: "claude-sess-1", Harness: "claude", Kind: porter.KindPrompt, Project: "notes"})
	mac.event(t, porter.Event{Agent: "sub-1", Parent: "claude-sess-1", AgentType: "Explore", Harness: "claude", Kind: porter.KindPrompt})
	nodeIntegrationEventually(t, "identity linked", func() bool {
		st, _ := mac.state.Load()
		a := st.Agents["claude-sess-1"]
		return a != nil && a.Identity != nil && a.Identity.Address == "mac:pager-refactor" && a.Identity.Persistent
	})
	// Each porter machine subscribes to the other and caches its topics.
	pi.event(t, porter.Event{Agent: "joana-sess", Harness: "claude", Kind: porter.KindNeeds})
	cache := porter.Cache{Dir: filepath.Join(pi.cfg.DataDir, "porter", "cache")}
	macID := mac.node.Store.Identity.MachineID
	nodeIntegrationEventually(t, "pi caches mac agents", func() bool {
		e, _ := cache.Load(macID, porter.TopicAgents)
		if e == nil || len(e.Items) != 2 || e.Machine != "mac" {
			return false
		}
		for _, raw := range e.Items {
			var a porter.Agent
			json.Unmarshal(raw, &a)
			if a.ID == "claude-sess-1" && a.Title == "pager-refactor" {
				return true
			}
		}
		return false
	})
	nodeIntegrationEventually(t, "pi caches mac status", func() bool {
		e, _ := cache.Load(macID, porter.TopicStatus)
		return e != nil && len(e.Value) > 0
	})
	if e, _ := cache.Load(macID, porter.TopicTrusts); e != nil {
		t.Fatal("trusts cached by a non-approver")
	}
	macCache := porter.Cache{Dir: filepath.Join(mac.cfg.DataDir, "porter", "cache")}
	nodeIntegrationEventually(t, "mac caches pi agents", func() bool {
		e, _ := macCache.Load(pi.node.Store.Identity.MachineID, porter.TopicAgents)
		return e != nil && len(e.Items) == 1
	})
	// Updates and removes keep the cache current.
	mac.event(t, porter.Event{Agent: "sub-1", Kind: porter.KindStop})
	mac.state.Update(func(s *porter.State) error { delete(s.Agents, "claude-sess-1"); return nil })
	nodeIntegrationEventually(t, "cache follows updates", func() bool {
		e, _ := cache.Load(macID, porter.TopicAgents)
		if e == nil || len(e.Items) != 1 {
			return false
		}
		var a porter.Agent
		json.Unmarshal(e.Items[0], &a)
		return a.ID == "sub-1" && a.Status == porter.StatusDone && a.Parent == "claude-sess-1"
	})
}
