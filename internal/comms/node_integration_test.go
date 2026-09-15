package comms

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests use the production Unix HTTP server, scheduler, broker reader and
// sender. Only the final agent adapter is replaced by a receiver that explicitly
// reports handoff. The broker uses its real HTTP and SQLite implementation.
type nodeIntegrationFixture struct {
	node      *Node
	client    *http.Client
	transport *http.Transport
	trace     *nodeIntegrationTransport
	cfg       Config
}

type nodeIntegrationTransport struct {
	base        http.RoundTripper
	mu          sync.Mutex
	paths       []string
	denyGrant   atomic.Bool
	denyReceipt atomic.Bool
	blocked     *nodeIntegrationBlock
}

type nodeIntegrationBlock struct {
	id      string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (tr *nodeIntegrationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	tr.paths = append(tr.paths, req.Method+" "+req.URL.Path)
	blocked := tr.blocked
	tr.mu.Unlock()
	if tr.denyGrant.Load() && req.Method == "PUT" && strings.HasPrefix(req.URL.Path, "/v1/grants/") {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"test_sync_partition","error":"grant sync temporarily unavailable"}`)), Request: req}, nil
	}
	if tr.denyReceipt.Load() && req.Method == "POST" && req.URL.Path == "/v1/receipts" {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"test_receipt_partition","error":"receipt path temporarily unavailable"}`)), Request: req}, nil
	}
	if blocked != nil && req.Method == "POST" && req.URL.Path == "/v1/messages" {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(data))
		var env Envelope
		if err = json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		if env.ID == blocked.id {
			blocked.once.Do(func() { close(blocked.started) })
			select {
			case <-blocked.release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
	}
	return tr.base.RoundTrip(req)
}

func (tr *nodeIntegrationTransport) dataRequests() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	count := 0
	for _, path := range tr.paths {
		if strings.Contains(path, "/v1/messages") || strings.Contains(path, "/v1/receipts") || strings.Contains(path, "/v1/history") || path == "GET /v1/who" {
			count++
		}
	}
	return count
}

func newNodeIntegration(t *testing.T, change func(*Config)) *nodeIntegrationFixture {
	t.Helper()
	// macOS Unix socket paths must fit sockaddr_un; t.TempDir's test-name path
	// can exceed that limit even when the production path fits comfortably.
	dir, err := os.MkdirTemp("/tmp", "comms-it-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Heartbeat = 40 * time.Millisecond
	cfg.Lease = 3 * time.Second
	cfg.Drain = 100 * time.Millisecond
	if change != nil {
		change(&cfg)
	}
	tr := &nodeIntegrationTransport{base: http.DefaultTransport}
	n, err := NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.httpClient.Transport = tr
	if err = n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	local := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "node.sock"))
	}}
	f := &nodeIntegrationFixture{node: n, cfg: cfg, transport: local, trace: tr, client: &http.Client{Transport: local, Timeout: 5 * time.Second}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := f.node.Close(ctx); err != nil {
			t.Error(err)
		}
		local.CloseIdleConnections()
	})
	return f
}

func (f *nodeIntegrationFixture) request(method, path string, body any) (int, []byte, error) {
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(marshal(body))
	}
	req, err := http.NewRequest(method, "http://comms"+path, input)
	if err != nil {
		return 0, nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (f *nodeIntegrationFixture) call(t *testing.T, method, path string, body any, status int, out any) []byte {
	t.Helper()
	actual, data, err := f.request(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if actual != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, actual, status, data)
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func nodeIntegrationEventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", description)
}

func (f *nodeIntegrationFixture) open(t *testing.T, alias, scope string, persistent bool) OpenResponse {
	t.Helper()
	var out OpenResponse
	f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: alias, Scope: scope, Persistent: persistent, Harness: "service", HarnessID: NewID("test_session_")}, 200, &out)
	return out
}

func (f *nodeIntegrationFixture) send(t *testing.T, session, to, body, id string) Message {
	t.Helper()
	var out Message
	f.call(t, "POST", "/v1/messages", SendRequest{SessionID: session, To: to, Body: body, ID: id}, 202, &out)
	return out
}

func (f *nodeIntegrationFixture) waitState(t *testing.T, sender, id, state string) Message {
	t.Helper()
	var got Message
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		got, lastErr = f.node.Store.Message(sender, id)
		if lastErr == nil && got.State == state {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("message %s/%s: wanted %s, got state=%s failure=%s err=%v", sender, id, state, got.State, got.Failure, lastErr)
	return got
}

func pairNodeIntegration(t *testing.T, mac, pi *nodeIntegrationFixture) *brokerFixture {
	t.Helper()
	broker := testBroker(t, nil)
	mac.call(t, "PUT", "/v1/name", map[string]string{"name": "mac"}, 200, nil)
	pi.call(t, "PUT", "/v1/name", map[string]string{"name": "pi"}, 200, nil)
	for _, pair := range []struct {
		from, to *nodeIntegrationFixture
		alias    string
	}{{mac, pi, "pi"}, {pi, mac, "mac"}} {
		identity := pair.to.node.Store.Identity
		pair.from.call(t, "PUT", "/v1/peers/"+identity.MachineID, Peer{MachineID: identity.MachineID, Alias: pair.alias, PublicKey: identity.PublicKey}, 200, nil)
		pair.from.call(t, "PUT", "/v1/grants/"+identity.MachineID, map[string]bool{"allow_messages": true, "allow_history": false}, 200, nil)
		pair.from.call(t, "PUT", "/v1/broker", map[string]string{"url": broker.server.URL}, 200, nil)
	}
	nodeIntegrationEventually(t, "both real nodes connected and grants published", func() bool {
		a, e1 := broker.b.hasGrant(context.Background(), pi.node.Store.Identity.MachineID, mac.node.Store.Identity.MachineID, false)
		b, e2 := broker.b.hasGrant(context.Background(), mac.node.Store.Identity.MachineID, pi.node.Store.Identity.MachineID, false)
		return e1 == nil && e2 == nil && a && b && mac.node.brokerOnline.Load() && pi.node.brokerOnline.Load()
	})
	return broker
}

type nodeIntegrationReceiver struct {
	fixture  *nodeIntegrationFixture
	session  Session
	messages chan Message
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
}

func (f *nodeIntegrationFixture) receiver(t *testing.T, session Session, renewLease ...bool) *nodeIntegrationReceiver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", "http://comms/v1/sessions/"+session.ID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: f.transport}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("receiver status %d: %s", resp.StatusCode, data)
	}
	receiver := &nodeIntegrationReceiver{fixture: f, session: session, messages: make(chan Message, 32), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(receiver.done)
		defer cancel()
		defer resp.Body.Close()
		decoder := json.NewDecoder(resp.Body)
		for {
			var event Event
			if err := decoder.Decode(&event); err != nil {
				return
			}
			if event.Type == "message" && event.Message != nil {
				select {
				case receiver.messages <- *event.Message:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	// The production receiver renews attachment leases independently of model turns.
	if len(renewLease) == 0 || renewLease[0] {
		go func() {
			ticker := time.NewTicker(f.cfg.Heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					r, _ := http.NewRequestWithContext(ctx, "PUT", "http://comms/v1/sessions/"+session.ID+"/lease", nil)
					response, err := f.client.Do(r)
					if err == nil {
						io.Copy(io.Discard, response.Body)
						response.Body.Close()
					}
				}
			}
		}()
	}
	t.Cleanup(func() { cancel(); <-receiver.done })
	return receiver
}

func (r *nodeIntegrationReceiver) next(t *testing.T) Message {
	t.Helper()
	select {
	case m := <-r.messages:
		return m
	case <-r.done:
		t.Fatal("receiver disconnected before message")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for local handoff")
	}
	return Message{}
}

func (r *nodeIntegrationReceiver) handoff(t *testing.T, m Message, status string) {
	t.Helper()
	r.fixture.call(t, "POST", "/v1/sessions/"+r.session.ID+"/handoffs", Handoff{SenderMachine: m.SenderMachine, MessageID: m.ID, AttemptID: m.AttemptID, Status: status}, 200, nil)
}

func (r *nodeIntegrationReceiver) empty(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.messages:
		t.Fatalf("unexpected handoff %s in state %s", m.ID, m.State)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestNodeIntegrationEncryptedRoundTripAndReceipts(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	mr, pr := mac.receiver(t, notes.Session), pi.receiver(t, brain.Session)
	nodeIntegrationEventually(t, "remote brain alias discovery", func() bool {
		_, data, err := mac.request("GET", "/v1/who", nil)
		if err != nil {
			return false
		}
		var who []Presence
		if json.Unmarshal(data, &who) != nil {
			return false
		}
		for _, p := range who {
			if p.PeerAlias == "pi" && p.Alias == "brain" && p.Online {
				return true
			}
		}
		return false
	})
	sent := mac.send(t, notes.Session.ID, "pi:brain", "hello over encrypted transport", "roundtrip_one")
	got := pr.next(t)
	if got.Body != "hello over encrypted transport" || got.ID != sent.ID || got.SenderMachine != mac.node.Store.Identity.MachineID || got.SenderAgent != notes.Agent.ID || got.RecipientAgent != brain.Agent.ID {
		t.Fatalf("incorrect receiver message: %+v", got)
	}
	mac.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "received")
	pr.handoff(t, got, "handed_off")
	pi.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "handed_off")
	// Retrying the same adapter outcome is safe if its HTTP acknowledgment was lost.
	pr.handoff(t, got, "handed_off")
	reply := pi.send(t, brain.Session.ID, "mac:"+notes.Agent.ID, "reply from brain", "roundtrip_reply")
	back := mr.next(t)
	if back.ID != reply.ID || back.Body != "reply from brain" {
		t.Fatalf("incorrect reply: %+v", back)
	}
	mr.handoff(t, back, "handed_off")
	pi.waitState(t, pi.node.Store.Identity.MachineID, reply.ID, "handed_off")
	var history HistoryPage
	mac.call(t, "GET", "/v1/messages?agent_id="+notes.Agent.ID, nil, 200, &history)
	if len(history.Messages) != 2 || history.Messages[0].Body != "hello over encrypted transport" {
		t.Fatalf("ciphertext-only outbound history unreadable: %+v", history)
	}
}

func TestNodeIntegrationLocalContainmentAndSocketPermissions(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	pairNodeIntegration(t, mac, pi)
	sender := mac.open(t, "local-sender", "local", false)
	target := mac.open(t, "local-target", "global", false)
	receiver := mac.receiver(t, target.Session)
	for _, path := range []string{mac.cfg.DataDir, filepath.Join(mac.cfg.DataDir, "node.sock"), filepath.Join(mac.cfg.DataDir, "node.db")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("private local state has group/other access: %s %o", path, info.Mode().Perm())
		}
	}
	before := mac.trace.dataRequests()
	sent := mac.send(t, sender.Session.ID, "mac:local-target", "same machine stays here", "local_containment")
	message := receiver.next(t)
	receiver.handoff(t, message, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "handed_off")
	mac.call(t, "POST", "/v1/messages", SendRequest{SessionID: sender.Session.ID, To: "pi:a_any", Body: "must not leave"}, 403, nil)
	if after := mac.trace.dataRequests(); after != before {
		t.Fatalf("local send/denial caused broker data requests: before %d, after %d", before, after)
	}
	// Once broker use is disabled, local communication continues unchanged.
	mac.call(t, "PUT", "/v1/broker", map[string]string{"url": ""}, 200, nil)
	mac.send(t, sender.Session.ID, "local-target", "works without broker", "local_no_broker")
	message = receiver.next(t)
	receiver.handoff(t, message, "handed_off")
}

func TestNodeIntegrationPersistentOfflineResumeAndEphemeralEnd(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	pairNodeIntegration(t, mac, pi)
	sender := mac.open(t, "notes", "global", false)
	brain := pi.open(t, "brain", "global", true)
	queued := mac.send(t, sender.Session.ID, "pi:"+brain.Agent.ID, "wait for the same brain", "persistent_wait")
	pi.waitState(t, mac.node.Store.Identity.MachineID, queued.ID, "received")
	mac.waitState(t, mac.node.Store.Identity.MachineID, queued.ID, "received")
	pi.call(t, "DELETE", "/v1/sessions/"+brain.Session.ID, nil, 200, nil)
	resumed := pi.open(t, "brain", "global", true)
	if resumed.Agent.ID != brain.Agent.ID || resumed.Session.ID == brain.Session.ID {
		t.Fatal("persistent resume did not preserve identity with a fresh attachment")
	}
	r := pi.receiver(t, resumed.Session)
	message := r.next(t)
	if message.ID != queued.ID {
		t.Fatal("persistent inbox did not resume")
	}
	r.handoff(t, message, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, queued.ID, "handed_off")
	r.cancel()
	<-r.done
	second := mac.send(t, sender.Session.ID, "pi:"+brain.Agent.ID, "receiver disconnected only", "persistent_reconnect")
	pi.waitState(t, mac.node.Store.Identity.MachineID, second.ID, "received")
	r = pi.receiver(t, resumed.Session)
	message = r.next(t)
	if message.ID != second.ID {
		t.Fatal("receiver reconnect did not rescan pending mail")
	}
	r.handoff(t, message, "handed_off")
	ephemeral := pi.open(t, "worker", "global", false)
	undelivered := mac.send(t, sender.Session.ID, "pi:"+ephemeral.Agent.ID, "agent will end", "ephemeral_pending")
	pi.waitState(t, mac.node.Store.Identity.MachineID, undelivered.ID, "received")
	pi.call(t, "DELETE", "/v1/sessions/"+ephemeral.Session.ID, nil, 200, nil)
	failed := mac.waitState(t, mac.node.Store.Identity.MachineID, undelivered.ID, "undeliverable")
	if failed.Failure != "agent_ended" {
		t.Fatalf("wrong termination receipt: %+v", failed)
	}
	replacement := pi.open(t, "worker", "global", false)
	if replacement.Agent.ID == ephemeral.Agent.ID {
		t.Fatal("ephemeral alias reuse inherited old identity")
	}
	pi.receiver(t, replacement.Session).empty(t)
}

func TestNodeIntegrationHistoryPermissionAndLocalExclusion(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	broker := pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	r := pi.receiver(t, brain.Session)
	remote := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "remote history record", "history_remote")
	m := r.next(t)
	r.handoff(t, m, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, remote.ID, "handed_off")
	localSender := pi.open(t, "private-work", "local", false)
	local := pi.send(t, localSender.Session.ID, "brain", "local confidential record", "history_local")
	m = r.next(t)
	r.handoff(t, m, "handed_off")
	pi.waitState(t, pi.node.Store.Identity.MachineID, local.ID, "handed_off")
	query := HistoryRequest{SessionID: notes.Session.ID, Target: "pi:" + brain.Agent.ID, Limit: 50}
	mac.call(t, "POST", "/v1/history", query, 403, nil)
	pi.call(t, "PUT", "/v1/grants/"+mac.node.Store.Identity.MachineID, map[string]bool{"allow_messages": true, "allow_history": true}, 200, nil)
	nodeIntegrationEventually(t, "history grant published", func() bool {
		allowed, err := broker.b.hasGrant(context.Background(), pi.node.Store.Identity.MachineID, mac.node.Store.Identity.MachineID, true)
		return err == nil && allowed
	})
	var page HistoryPage
	mac.call(t, "POST", "/v1/history", query, 200, &page)
	if len(page.Messages) != 1 || page.Messages[0].ID != remote.ID || page.Messages[0].Body != "remote history record" {
		t.Fatalf("remote history leaked local traffic or lost remote record: %+v", page)
	}
	pi.call(t, "GET", "/v1/messages?agent_id="+brain.Agent.ID, nil, 200, &page)
	if len(page.Messages) != 2 {
		t.Fatalf("local cross-reading lacks full history: %+v", page)
	}
	localReader := mac.open(t, "local-reader", "local", false)
	query.SessionID = localReader.Session.ID
	mac.call(t, "POST", "/v1/history", query, 403, nil)
	query.SessionID = notes.Session.ID
	pi.trace.denyGrant.Store(true)
	pi.call(t, "PUT", "/v1/grants/"+mac.node.Store.Identity.MachineID, map[string]bool{"allow_messages": true, "allow_history": false}, 200, nil)
	mac.call(t, "POST", "/v1/history", query, 403, nil) // Node enforces revocation despite stale broker grant.
	pi.waitState(t, pi.node.Store.Identity.MachineID, local.ID, "handed_off")
}

func TestNodeIntegrationLocalRevocationOverridesStaleBroker(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	broker := pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	queued := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "pending at recipient", "revoke_pending")
	pi.waitState(t, mac.node.Store.Identity.MachineID, queued.ID, "received")
	pi.trace.denyGrant.Store(true)
	pi.call(t, "PUT", "/v1/grants/"+mac.node.Store.Identity.MachineID, map[string]bool{"allow_messages": false, "allow_history": false}, 200, nil)
	failed := mac.waitState(t, mac.node.Store.Identity.MachineID, queued.ID, "undeliverable")
	if failed.Failure != "grant_revoked" {
		t.Fatalf("wrong revocation receipt: %+v", failed)
	}
	allowed, err := broker.b.hasGrant(context.Background(), pi.node.Store.Identity.MachineID, mac.node.Store.Identity.MachineID, false)
	if err != nil || !allowed {
		t.Fatal("test did not preserve stale broker permission")
	}
	later := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "broker still permits this", "revoke_new")
	failed = mac.waitState(t, mac.node.Store.Identity.MachineID, later.ID, "undeliverable")
	if failed.Failure != "grant_revoked" {
		t.Fatalf("receiving node failed independent grant enforcement: %+v", failed)
	}
	pi.receiver(t, brain.Session).empty(t)
}

func TestNodeIntegrationIndependentRecipientsAndSerialization(t *testing.T) {
	f := newNodeIntegration(t, nil)
	sender, a, b := f.open(t, "sender", "local", false), f.open(t, "a", "local", false), f.open(t, "b", "local", false)
	ra, rb := f.receiver(t, a.Session), f.receiver(t, b.Session)
	first := f.send(t, sender.Session.ID, "a", "first in flight", "serial_first")
	firstHandoff := ra.next(t)
	second := f.send(t, sender.Session.ID, "a", "second waits for same agent", "serial_second")
	other := f.send(t, sender.Session.ID, "b", "other agent proceeds", "serial_other")
	otherHandoff := rb.next(t)
	if otherHandoff.ID != other.ID {
		t.Fatal("other agent did not proceed independently")
	}
	ra.empty(t)
	rb.handoff(t, otherHandoff, "handed_off")
	f.waitState(t, f.node.Store.Identity.MachineID, other.ID, "handed_off")
	if firstHandoff.ID != first.ID {
		t.Fatal("unexpected initial handoff")
	}
	ra.handoff(t, firstHandoff, "handed_off")
	secondHandoff := ra.next(t)
	if secondHandoff.ID != second.ID {
		t.Fatal("next same-agent message did not proceed after completion")
	}
	ra.handoff(t, secondHandoff, "handed_off")
}

func TestNodeIntegrationLargeOfflineBacklogCannotHideReadyAgent(t *testing.T) {
	f := newNodeIntegration(t, nil)
	sender := f.open(t, "sender", "local", false)
	f.open(t, "offline", "local", true)
	ready := f.open(t, "ready", "local", false)
	r := f.receiver(t, ready.Session)
	for i := 0; i < 300; i++ {
		f.send(t, sender.Session.ID, "offline", "older offline backlog", NewID("backlog_"))
	}
	last := f.send(t, sender.Session.ID, "ready", "ready agent behind old rows", "ready_after_backlog")
	m := r.next(t)
	if m.ID != last.ID {
		t.Fatal("wrong message after large offline backlog")
	}
	r.handoff(t, m, "handed_off")
	f.waitState(t, f.node.Store.Identity.MachineID, last.ID, "handed_off")
}

func TestNodeIntegrationRecipientBackpressureDoesNotBlockOtherAgent(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, func(c *Config) { c.LocalCount = 1 })
	broker := pairNodeIntegration(t, mac, pi)
	notes, brain, worker := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true), pi.open(t, "worker", "global", false)
	rw := pi.receiver(t, worker.Session)
	first := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "fills recipient quota", "quota_first")
	pi.waitState(t, mac.node.Store.Identity.MachineID, first.ID, "received")
	second := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "waits at broker", "quota_second")
	nodeIntegrationEventually(t, "full recipient leaves ciphertext unacknowledged", func() bool {
		var count int
		err := broker.b.db.QueryRow(`SELECT count(*) FROM pending_messages WHERE sender_machine_id=? AND id=?`, mac.node.Store.Identity.MachineID, second.ID).Scan(&count)
		return err == nil && count == 1
	})
	third := mac.send(t, notes.Session.ID, "pi:"+worker.Agent.ID, "other queue stays usable", "quota_other")
	m := rw.next(t)
	if m.ID != third.ID {
		t.Fatal("full brain queue blocked another agent")
	}
	rw.handoff(t, m, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, third.ID, "handed_off")
	rb := pi.receiver(t, brain.Session)
	m = rb.next(t)
	if m.ID != first.ID {
		t.Fatal("queued first message lost")
	}
	rb.handoff(t, m, "handed_off")
	pi.waitState(t, mac.node.Store.Identity.MachineID, first.ID, "handed_off")
	// A real stream reconnect retries unacknowledged broker work immediately.
	pi.call(t, "PUT", "/v1/broker", map[string]string{"url": broker.server.URL}, 200, nil)
	m = rb.next(t)
	if m.ID != second.ID {
		t.Fatal("backpressured ciphertext did not survive reconnect")
	}
	rb.handoff(t, m, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, second.ID, "handed_off")
}

func TestNodeIntegrationPruneRetainsReplayProtection(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	broker := pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	r := pi.receiver(t, brain.Session)
	sent := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "erase body but remember delivery", "pruned_replay")
	m := r.next(t)
	r.handoff(t, m, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "handed_off")
	var original Envelope
	stored, err := mac.node.Store.Message(mac.node.Store.Identity.MachineID, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(stored.Wire, &original); err != nil {
		t.Fatal(err)
	}
	pi.call(t, "POST", "/v1/prune", map[string]int64{"before": Now() + 1}, 200, nil)
	pruned, err := pi.node.Store.Message(mac.node.Store.Identity.MachineID, sent.ID)
	if err != nil || pruned.PrunedAt == nil || pruned.Body != "" {
		t.Fatalf("prune did not keep an empty replay record: %+v %v", pruned, err)
	}
	if err = mac.node.brokerRequest(context.Background(), "POST", "/v1/messages", original, nil); err != nil {
		t.Fatal(err)
	}
	nodeIntegrationEventually(t, "replay acknowledged without repeating handoff", func() bool {
		var count int
		err := broker.b.db.QueryRow(`SELECT count(*) FROM pending_messages WHERE sender_machine_id=? AND id=?`, original.Sender, original.ID).Scan(&count)
		return err == nil && count == 0
	})
	r.empty(t)
	var history HistoryPage
	pi.call(t, "GET", "/v1/messages?agent_id="+brain.Agent.ID, nil, 200, &history)
	if len(history.Messages) != 0 {
		t.Fatal("pruned content returned in history")
	}
	var stats map[string]int64
	pi.call(t, "GET", "/v1/stats", nil, 200, &stats)
	if stats["messages"] != 0 {
		t.Fatal("pruned messages retained in derived statistics")
	}
}

func TestNodeIntegrationSendDedupQuotaExpiryAndReadOnlyInbox(t *testing.T) {
	f := newNodeIntegration(t, func(c *Config) { c.LocalCount = 1 })
	sender, target := f.open(t, "sender", "local", false), f.open(t, "target", "local", false)
	first := f.send(t, sender.Session.ID, "target", "one retained record", "stable_send_id")
	again := f.send(t, sender.Session.ID, "target", "one retained record", "stable_send_id")
	if again.ID != first.ID {
		t.Fatal("idempotent send made a new message")
	}
	f.call(t, "POST", "/v1/messages", SendRequest{SessionID: sender.Session.ID, To: "target", Body: "different", ID: first.ID}, 409, nil)
	f.call(t, "POST", "/v1/messages", SendRequest{SessionID: sender.Session.ID, To: "target", Body: "queue full", ID: "over_quota"}, 429, nil)
	var page HistoryPage
	for i := 0; i < 2; i++ {
		f.call(t, "GET", "/v1/messages?pending=true&agent_id="+target.Agent.ID, nil, 200, &page)
		if len(page.Messages) != 1 || page.Messages[0].State != "received" {
			t.Fatalf("inspection consumed or changed mail: %+v", page)
		}
	}
	if _, err := f.node.Store.DB.Exec(`UPDATE messages SET expires_at=? WHERE id=?`, Now()-1, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.node.Store.Expire(); err != nil {
		t.Fatal(err)
	}
	f.waitState(t, f.node.Store.Identity.MachineID, first.ID, "expired")
	r := f.receiver(t, target.Session)
	r.empty(t)
	next := f.send(t, sender.Session.ID, "target", "expired work frees quota", "after_expiry")
	m := r.next(t)
	if m.ID != next.ID {
		t.Fatal("expired message was handed off")
	}
	r.handoff(t, m, "handed_off")
}

func TestNodeIntegrationDrainRestartPreservesIdentityAndUncertainty(t *testing.T) {
	f := newNodeIntegration(t, nil)
	sender, target := f.open(t, "sender", "local", false), f.open(t, "target", "local", false)
	r := f.receiver(t, target.Session)
	first := f.send(t, sender.Session.ID, "target", "possibly already emitted", "drain_uncertain")
	m := r.next(t)
	if m.ID != first.ID {
		t.Fatal("wrong in-flight message")
	}
	second := f.send(t, sender.Session.ID, "target", "still safely queued", "drain_pending")
	identity := f.node.Store.Identity
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := f.node.Close(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	<-r.done
	f.transport.CloseIdleConnections()
	restarted, err := NewNode(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.httpClient.Transport = f.trace
	if err = restarted.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.node = restarted
	if restarted.Store.Identity.MachineID != identity.MachineID || !SameKey(restarted.Store.Identity.PublicKey, identity.PublicKey) {
		t.Fatal("restart changed machine identity")
	}
	saved, err := restarted.Store.Agent(target.Agent.ID)
	if err != nil || saved.RetiredAt != nil {
		t.Fatalf("node restart retired a surviving ephemeral agent: %+v %v", saved, err)
	}
	f.waitState(t, identity.MachineID, first.ID, "uncertain")
	r = f.receiver(t, target.Session)
	m = r.next(t)
	if m.ID != second.ID {
		t.Fatalf("restart retried uncertain handoff instead of pending message: %+v", m)
	}
	r.handoff(t, m, "handed_off")
	f.waitState(t, identity.MachineID, second.ID, "handed_off")
	r.empty(t)
}

func TestNodeIntegrationAgentNamespaceAndTakeover(t *testing.T) {
	f := newNodeIntegration(t, nil)
	persistent := f.open(t, "brain", "global", true)
	request := OpenRequest{Alias: "brain", Scope: "local", Persistent: true, Harness: "service", HarnessID: "new-session"}
	f.call(t, "POST", "/v1/sessions", request, 409, nil)
	oldReceiver := f.receiver(t, persistent.Session)
	request.Takeover = true
	var replacement OpenResponse
	f.call(t, "POST", "/v1/sessions", request, 200, &replacement)
	if replacement.Agent.ID != persistent.Agent.ID || replacement.Session.ID == persistent.Session.ID || replacement.Session.Scope != "local" {
		t.Fatal("takeover failed identity or explicit scope semantics")
	}
	select {
	case <-oldReceiver.done:
	case <-time.After(time.Second):
		t.Fatal("takeover did not fence old receiver")
	}
	f.call(t, "GET", "/v1/sessions/"+persistent.Session.ID+"/stream", nil, 410, nil)
	var saved []Agent
	f.call(t, "GET", "/v1/agents?persistent=true", nil, 200, &saved)
	if len(saved) != 1 || saved[0].ID != persistent.Agent.ID {
		t.Fatal("persistent identity not discoverable by name/list")
	}
	// A different agent with the same friendly name must not inherit its ID.
	f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: "brain", Harness: "service", HarnessID: "ephemeral"}, 409, nil)
}

func TestNodeIntegrationReceiptsDoNotNeedReverseGrant(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	broker := pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	r := pi.receiver(t, brain.Session)
	mac.call(t, "PUT", "/v1/grants/"+pi.node.Store.Identity.MachineID, map[string]bool{"allow_messages": false, "allow_history": false}, 200, nil)
	nodeIntegrationEventually(t, "reverse messaging permission revoked", func() bool {
		allowed, err := broker.b.hasGrant(context.Background(), mac.node.Store.Identity.MachineID, pi.node.Store.Identity.MachineID, false)
		return err == nil && !allowed
	})
	m := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "one-directional request", "one_way_receipts")
	handoff := r.next(t)
	mac.waitState(t, mac.node.Store.Identity.MachineID, m.ID, "received")
	r.handoff(t, handoff, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, m.ID, "handed_off")
}

func TestNodeIntegrationHistoryScopesAgentIDsToTheirMachine(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	pairNodeIntegration(t, mac, pi)
	brain, other := pi.open(t, "brain", "global", true), pi.open(t, "other", "global", true)
	// A remote machine controls its own agent labels. Its sender-agent string may
	// equal a local identity without making it that local identity's history.
	payload := Payload{Routing: Routing{Version: ProtocolVersion, ID: "remote_agent_id_collision", Sender: mac.node.Store.Identity.MachineID, Recipient: pi.node.Store.Identity.MachineID, Kind: "message", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, SenderAgent: brain.Agent.ID, RecipientAgent: other.Agent.ID, CreatedAt: Now(), Body: "belongs to other, not local brain"}
	env, err := Encrypt(mac.node.Store.Identity, Peer{MachineID: pi.node.Store.Identity.MachineID, PublicKey: pi.node.Store.Identity.PublicKey}, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = mac.node.brokerRequest(context.Background(), "POST", "/v1/messages", env, nil); err != nil {
		t.Fatal(err)
	}
	pi.waitState(t, mac.node.Store.Identity.MachineID, payload.ID, "received")
	var page HistoryPage
	pi.call(t, "GET", "/v1/messages?agent_id="+brain.Agent.ID, nil, 200, &page)
	if len(page.Messages) != 0 {
		t.Fatalf("remote sender string polluted local agent history: %+v", page)
	}
	pi.call(t, "GET", "/v1/messages?agent_id="+other.Agent.ID, nil, 200, &page)
	if len(page.Messages) != 1 || page.Messages[0].ID != payload.ID {
		t.Fatal("legitimate local recipient history was lost")
	}
}

func TestNodeIntegrationFullReceiptReservePreservesCompletedHandoff(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, func(c *Config) { c.ReceiptCount = 1 })
	pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	r := pi.receiver(t, brain.Session)
	pi.trace.denyReceipt.Store(true)
	sent := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "handoff survives full receipt reserve", "receipt_reserve_full")
	m := r.next(t)
	r.handoff(t, m, "handed_off")
	pi.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "handed_off")
	var pending int
	if err := pi.node.Store.DB.QueryRow(`SELECT receipt_pending FROM messages WHERE sender_machine_id=? AND id=?`, mac.node.Store.Identity.MachineID, sent.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("full receipt reserve lost durable receipt obligation: pending=%d err=%v", pending, err)
	}
	pi.call(t, "POST", "/v1/prune", map[string]int64{"before": Now() + 1}, 200, nil)
	var preserved int
	if err := pi.node.Store.DB.QueryRow(`SELECT count(*) FROM messages WHERE kind='receipt' AND pruned_at IS NULL`).Scan(&preserved); err != nil || preserved != 1 {
		t.Fatalf("ordinary prune damaged outstanding protocol receipt: %d %v", preserved, err)
	}
	pi.trace.denyReceipt.Store(false)
	pi.node.notify()
	mac.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "handed_off")
	r.empty(t)
}

func TestNodeIntegrationSlowOutgoingRequestDoesNotBlockNewMessages(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	pairNodeIntegration(t, mac, pi)
	notes, slow, ready := mac.open(t, "notes", "global", false), pi.open(t, "slow", "global", true), pi.open(t, "ready", "global", false)
	r := pi.receiver(t, ready.Session)
	block := &nodeIntegrationBlock{id: "blocked_first_send", started: make(chan struct{}), release: make(chan struct{})}
	mac.trace.mu.Lock()
	mac.trace.blocked = block
	mac.trace.mu.Unlock()
	defer close(block.release)
	mac.send(t, notes.Session.ID, "pi:"+slow.Agent.ID, "network request stays blocked", block.id)
	select {
	case <-block.started:
	case <-time.After(3 * time.Second):
		t.Fatal("first outgoing request never started")
	}
	second := mac.send(t, notes.Session.ID, "pi:"+ready.Agent.ID, "new work uses another sender slot", "new_send_after_blocked")
	m := r.next(t)
	if m.ID != second.ID {
		t.Fatal("wrong message while another network request was blocked")
	}
	r.handoff(t, m, "handed_off")
	mac.waitState(t, mac.node.Store.Identity.MachineID, second.ID, "handed_off")
}

func TestNodeIntegrationBrokerGrantDenialIsTerminalTransportFailure(t *testing.T) {
	mac, pi := newNodeIntegration(t, nil), newNodeIntegration(t, nil)
	broker := pairNodeIntegration(t, mac, pi)
	notes, brain := mac.open(t, "notes", "global", false), pi.open(t, "brain", "global", true)
	pi.call(t, "PUT", "/v1/grants/"+mac.node.Store.Identity.MachineID, map[string]bool{"allow_messages": false, "allow_history": false}, 200, nil)
	nodeIntegrationEventually(t, "broker messaging denial published", func() bool {
		allowed, err := broker.b.hasGrant(context.Background(), pi.node.Store.Identity.MachineID, mac.node.Store.Identity.MachineID, false)
		return err == nil && !allowed
	})
	sent := mac.send(t, notes.Session.ID, "pi:"+brain.Agent.ID, "denied before queue admission", "terminal_broker_denial")
	failed := mac.waitState(t, mac.node.Store.Identity.MachineID, sent.ID, "undeliverable")
	if failed.Failure != "broker_rejected:message_denied" || failed.ReceivedAt != nil {
		t.Fatalf("broker error misrepresented as recipient-authenticated delivery: %+v", failed)
	}
	var count int
	if err := pi.node.Store.DB.QueryRow(`SELECT count(*) FROM messages WHERE sender_machine_id=? AND id=?`, mac.node.Store.Identity.MachineID, sent.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("denied message reached recipient")
	}
}

func TestNodeIntegrationLeaseExpiryDoesNotRetireEphemeralIdentity(t *testing.T) {
	f := newNodeIntegration(t, func(c *Config) { c.Lease = 150 * time.Millisecond })
	sender, target := f.open(t, "sender", "local", false), f.open(t, "target", "local", false)
	r := f.receiver(t, target.Session, false)
	nodeIntegrationEventually(t, "unrenewed receiver becomes offline", func() bool { return !f.node.ready(target.Agent.ID) })
	select {
	case <-r.done:
	case <-time.After(time.Second):
		t.Fatal("expired receiver remained connected")
	}
	agent, err := f.node.Store.Agent(target.Agent.ID)
	if err != nil || agent.RetiredAt != nil {
		t.Fatalf("lease timeout permanently retired identity: %+v %v", agent, err)
	}
	sent := f.send(t, sender.Session.ID, "target", "wait through lost contact", "lease_reconnect")
	r = f.receiver(t, target.Session)
	m := r.next(t)
	if m.ID != sent.ID {
		t.Fatal("same attachment did not recover its queue")
	}
	r.handoff(t, m, "handed_off")
	f.waitState(t, f.node.Store.Identity.MachineID, sent.ID, "handed_off")
}

func TestNodeIntegrationConfirmedHarnessExitRetiresEphemeralIdentity(t *testing.T) {
	f := newNodeIntegration(t, nil)
	process := exec.Command("sleep", "20")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	stamp := ProcessStamp(process.Process.Pid)
	if stamp == "" {
		t.Fatal("could not establish real harness process identity")
	}
	var target OpenResponse
	f.call(t, "POST", "/v1/sessions", OpenRequest{Alias: "tracked", Scope: "local", Harness: "service", HarnessID: "tracked-harness", ProcessID: process.Process.Pid, ProcessStarted: stamp}, 200, &target)
	sender := f.open(t, "sender", "local", false)
	sent := f.send(t, sender.Session.ID, "tracked", "fails when actual harness exits", "real_process_exit")
	if err := process.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = process.Wait()
	nodeIntegrationEventually(t, "actual process exit retires ephemeral identity", func() bool { a, e := f.node.Store.Agent(target.Agent.ID); return e == nil && a.RetiredAt != nil })
	failed := f.waitState(t, f.node.Store.Identity.MachineID, sent.ID, "undeliverable")
	if failed.Failure != "agent_ended" {
		t.Fatalf("wrong process-end outcome: %+v", failed)
	}
}
