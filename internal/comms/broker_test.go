package comms

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
)

type brokerFixture struct {
	b      *Broker
	server *httptest.Server
	client *http.Client
}

func testBroker(t *testing.T, configure func(*Config)) *brokerFixture {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Heartbeat = 25 * time.Millisecond
	if configure != nil {
		configure(&cfg)
	}
	b, err := NewBroker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(b.Handler())
	f := &brokerFixture{b, s, &http.Client{Timeout: 5 * time.Second}}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
		s.Close()
	})
	return f
}

func (f *brokerFixture) call(t *testing.T, method, path, token string, body any, status int, out any) []byte {
	t.Helper()
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(marshal(body))
	}
	r, err := http.NewRequest(method, f.server.URL+path, input)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d; %s", method, path, response.StatusCode, status, data)
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

type testChallenge struct {
	Purpose   string `json:"purpose"`
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	PublicKey []byte `json:"public_key"`
	Secret    string `json:"secret"`
	ExpiresAt int64  `json:"expires_at"`
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	i, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func (f *brokerFixture) challengeFor(t *testing.T, identity Identity) testChallenge {
	t.Helper()
	var reply struct {
		ID     string `json:"id"`
		Sealed []byte `json:"sealed"`
	}
	f.call(t, "POST", "/v1/auth/challenges", "", map[string]any{"machine_id": identity.MachineID, "public_key": identity.PublicKey}, 200, &reply)
	pub, _ := key32(identity.PublicKey)
	priv, _ := key32(identity.PrivateKey)
	plain, ok := box.OpenAnonymous(nil, reply.Sealed, pub, priv)
	if !ok {
		t.Fatal("cannot decrypt possession challenge")
	}
	var challenge testChallenge
	if err := json.Unmarshal(plain, &challenge); err != nil {
		t.Fatal(err)
	}
	if challenge.Purpose != "comms-auth-v1" || challenge.ID != reply.ID || challenge.MachineID != identity.MachineID || !bytes.Equal(challenge.PublicKey, identity.PublicKey) || challenge.Secret == "" {
		t.Fatalf("challenge identity/purpose binding failed: %+v", challenge)
	}
	if remaining := challenge.ExpiresAt - Now(); remaining < 50000 || remaining > 60000 {
		t.Fatalf("challenge TTL is %dms", remaining)
	}
	return challenge
}

func (f *brokerFixture) register(t *testing.T, identity Identity) string {
	t.Helper()
	challenge := f.challengeFor(t, identity)
	var reply struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	f.call(t, "POST", "/v1/auth/complete", "", map[string]string{"id": challenge.ID, "proof": challenge.Secret}, 200, &reply)
	if len(reply.Token) != 43 || reply.ExpiresAt < Now()+int64((23*time.Hour)/time.Millisecond) {
		t.Fatal("invalid credential/expiry")
	}
	return reply.Token
}

func (f *brokerFixture) grant(t *testing.T, grantorToken string, grantee Identity, messages, history bool, revision int64, status int) {
	t.Helper()
	f.call(t, "PUT", "/v1/grants/"+grantee.MachineID, grantorToken, Grant{Grantee: grantee.MachineID, Messages: messages, History: history, Revision: revision}, status, nil)
}

type brokerTestStream struct {
	events chan Event
	done   chan struct{}
	cancel context.CancelFunc
}

func (f *brokerFixture) listen(t *testing.T, token string) *brokerTestStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, err := http.NewRequestWithContext(ctx, "GET", f.server.URL+"/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		t.Fatalf("stream: %d %s", response.StatusCode, data)
	}
	s := &brokerTestStream{events: make(chan Event, 32), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		defer response.Body.Close()
		decoder := json.NewDecoder(response.Body)
		for {
			var event Event
			if err := decoder.Decode(&event); err != nil {
				return
			}
			select {
			case s.events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-s.done })
	return s
}

func (s *brokerTestStream) next(t *testing.T, kind string) Event {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-s.events:
			if event.Type == kind {
				return event
			}
		case <-s.done:
			t.Fatal("stream ended before expected event")
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func testEnvelope(t *testing.T, from, to Identity, kind, id string) Envelope {
	t.Helper()
	p := Payload{Routing: Routing{Version: ProtocolVersion, ID: id, Sender: from.MachineID, Recipient: to.MachineID, Kind: kind, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, SenderAgent: "a_sender", RecipientAgent: "a_recipient", CreatedAt: Now(), Body: "secret broker must not see"}
	if kind == "receipt" {
		p.Body = ""
		p.Receipt = &Receipt{OriginalID: "message_original", Status: "received", At: Now()}
	}
	env, err := Encrypt(from, Peer{MachineID: to.MachineID, PublicKey: to.PublicKey}, p)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestBrokerAuthenticationChallengeAndCredential(t *testing.T) {
	f := testBroker(t, nil)
	identity := testIdentity(t)
	challenge := f.challengeFor(t, identity)
	wrong := testIdentity(t)
	wrong.MachineID = identity.MachineID
	// A challenge does not reserve an identity before proof succeeds.
	var count int
	if err := f.b.db.QueryRow(`SELECT count(*) FROM machines`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unproven identity registered: %d %v", count, err)
	}
	f.call(t, "POST", "/v1/auth/complete", "", map[string]string{"id": challenge.ID, "proof": "wrong"}, 401, nil)
	f.call(t, "POST", "/v1/auth/complete", "", map[string]string{"id": challenge.ID, "proof": challenge.Secret}, 401, nil)
	token := f.register(t, identity)
	f.call(t, "GET", "/v1/who", token, nil, 200, nil)
	f.call(t, "GET", "/v1/who", "", nil, 401, nil)
	var stored string
	if err := f.b.db.QueryRow(`SELECT token_hash FROM machines WHERE machine_id=?`, identity.MachineID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token || stored != digest([]byte(token)) {
		t.Fatal("credential was not stored as a hash")
	}
	f.call(t, "POST", "/v1/auth/challenges", "", map[string]any{"machine_id": wrong.MachineID, "public_key": wrong.PublicKey}, 409, nil)
	f.call(t, "POST", "/v1/auth/challenges", "", map[string]any{"machine_id": "low_order", "public_key": make([]byte, 32)}, 400, nil)
	f.call(t, "POST", "/v1/auth/challenges", "", map[string]any{"machine_id": "invalid/id", "public_key": identity.PublicKey}, 400, nil)
	expired := f.challengeFor(t, identity)
	f.b.mu.Lock()
	entry := f.b.challenges[expired.ID]
	entry.ExpiresAt = Now() - 1
	f.b.challenges[expired.ID] = entry
	f.b.mu.Unlock()
	f.call(t, "POST", "/v1/auth/complete", "", map[string]string{"id": expired.ID, "proof": expired.Secret}, 401, nil)
	if _, err := f.b.db.Exec(`UPDATE machines SET token_expires_at=? WHERE machine_id=?`, Now()-1, identity.MachineID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/v1/who", token, nil, 401, nil)
}

func TestBrokerProofSingleUseAndRotationClosesStream(t *testing.T) {
	f := testBroker(t, nil)
	identity := testIdentity(t)
	oldToken := f.register(t, identity)
	stream := f.listen(t, oldToken)
	challenge := f.challengeFor(t, identity)
	var reply struct {
		Token string `json:"token"`
	}
	body := map[string]string{"id": challenge.ID, "proof": challenge.Secret}
	f.call(t, "POST", "/v1/auth/complete", "", body, 200, &reply)
	f.call(t, "POST", "/v1/auth/complete", "", body, 401, nil)
	select {
	case <-stream.done:
	case <-time.After(time.Second):
		t.Fatal("credential replacement left old stream active")
	}
	f.call(t, "GET", "/v1/who", oldToken, nil, 401, nil)
	f.call(t, "GET", "/v1/who", reply.Token, nil, 200, nil)
	newStream := f.listen(t, reply.Token)
	replacement := f.listen(t, reply.Token)
	select {
	case <-newStream.done:
	case <-time.After(time.Second):
		t.Fatal("second stream did not replace first")
	}
	replacement.next(t, "heartbeat")
}

func TestBrokerDirectionalPresenceAndRevision(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi, other := testIdentity(t), testIdentity(t), testIdentity(t)
	macToken, piToken, otherToken := f.register(t, mac), f.register(t, pi), f.register(t, other)
	f.listen(t, piToken)
	presence := map[string]any{"revision": 1, "agents": []Presence{{AgentID: "a_brain", Alias: "brain", Persistent: true, Online: true}, {AgentID: "a_worker", Alias: "worker", Online: true}}}
	f.call(t, "PUT", "/v1/presence", piToken, presence, 200, nil)
	var who []Presence
	f.call(t, "GET", "/v1/who", macToken, nil, 200, &who)
	if len(who) != 0 {
		t.Fatal("ungranted machine saw presence")
	}
	f.grant(t, piToken, mac, true, false, 1, 200)
	f.call(t, "GET", "/v1/who", macToken, nil, 200, &who)
	if len(who) != 2 || !who[0].Online || who[0].MachineID != pi.MachineID {
		t.Fatalf("incorrect permitted presence: %+v", who)
	}
	f.call(t, "GET", "/v1/who", otherToken, nil, 200, &who)
	if len(who) != 0 {
		t.Fatal("presence leaked to third machine")
	}
	f.call(t, "PUT", "/v1/presence", piToken, presence, 200, nil) // Identical snapshot renews lease.
	f.call(t, "PUT", "/v1/presence", piToken, map[string]any{"revision": 1, "agents": []Presence{}}, 409, nil)
	f.call(t, "PUT", "/v1/presence", piToken, map[string]any{"revision": 2, "agents": []Presence{{MachineID: mac.MachineID, AgentID: "a_fake", Alias: "fake"}}}, 403, nil)
	if _, err := f.b.db.Exec(`UPDATE machines SET lease_expires_at=? WHERE machine_id=?`, Now()-1, pi.MachineID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/v1/who", macToken, nil, 200, &who)
	if len(who) != 1 || who[0].Alias != "brain" || who[0].Online {
		t.Fatalf("offline persistent visibility incorrect: %+v", who)
	}
	f.grant(t, piToken, mac, false, false, 2, 200)
	f.grant(t, piToken, mac, true, false, 1, 409)
	f.grant(t, piToken, mac, true, false, 2, 409)
	f.call(t, "GET", "/v1/who", macToken, nil, 200, &who)
	if len(who) != 0 {
		t.Fatal("revoked machine still saw presence")
	}
}

func TestBrokerQueueDeliveryDeduplicationAcknowledgment(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi := testIdentity(t), testIdentity(t)
	macToken, piToken := f.register(t, mac), f.register(t, pi)
	env := testEnvelope(t, mac, pi, "message", "message_one")
	f.call(t, "POST", "/v1/messages", macToken, env, 403, nil)
	f.grant(t, piToken, mac, true, false, 1, 200)
	f.call(t, "POST", "/v1/messages", macToken, env, 202, nil)
	f.call(t, "POST", "/v1/messages", macToken, env, 200, nil)
	changed := env
	changed.Ciphertext = bytes.Clone(env.Ciphertext)
	changed.Ciphertext[0] ^= 1
	f.call(t, "POST", "/v1/messages", macToken, changed, 409, nil)
	var wire []byte
	var count int
	if err := f.b.db.QueryRow(`SELECT envelope FROM pending_messages`).Scan(&wire); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("secret broker must not see")) {
		t.Fatal("broker retained plaintext")
	}
	stream := f.listen(t, piToken)
	event := stream.next(t, "envelope")
	if event.Envelope.ID != env.ID {
		t.Fatal("wrong queued envelope")
	}
	if _, err := Decrypt(pi, Peer{MachineID: mac.MachineID, PublicKey: mac.PublicKey}, *event.Envelope); err != nil {
		t.Fatal(err)
	}
	ack := map[string]string{"sender_machine_id": mac.MachineID, "id": env.ID}
	f.call(t, "POST", "/v1/acks", macToken, ack, 200, nil) // Other owners cannot delete it.
	if err := f.b.db.QueryRow(`SELECT count(*) FROM pending_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatal("nonrecipient deleted queue item")
	}
	stream.cancel()
	<-stream.done
	stream = f.listen(t, piToken)
	if stream.next(t, "envelope").Envelope.ID != env.ID {
		t.Fatal("unacked ciphertext did not replay on reconnect")
	}
	f.call(t, "POST", "/v1/acks", piToken, ack, 200, nil)
	f.call(t, "POST", "/v1/acks", piToken, ack, 200, nil)
	if err := f.b.db.QueryRow(`SELECT count(*) FROM pending_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("ack did not remove queue item: %d %v", count, err)
	}
}

func TestBrokerRejectsRoutingAbuseAndExpiry(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi := testIdentity(t), testIdentity(t)
	macToken, piToken := f.register(t, mac), f.register(t, pi)
	f.grant(t, piToken, mac, true, false, 1, 200)
	env := testEnvelope(t, mac, pi, "message", "message_one")
	spoof := env
	spoof.Sender = pi.MachineID
	f.call(t, "POST", "/v1/messages", macToken, spoof, 403, nil)
	expired := env
	expired.ExpiresAt = Now() - 1
	f.call(t, "POST", "/v1/messages", macToken, expired, 410, nil)
	tooLong := env
	tooLong.ExpiresAt = time.Now().Add(8 * 24 * time.Hour).UnixMilli()
	f.call(t, "POST", "/v1/messages", macToken, tooLong, 400, nil)
	wrongKind := env
	wrongKind.Kind = "history_request"
	f.call(t, "POST", "/v1/messages", macToken, wrongKind, 400, nil)
	f.call(t, "POST", "/v1/receipts", macToken, env, 400, nil)
	badNonce := env
	badNonce.Nonce = nil
	f.call(t, "POST", "/v1/messages", macToken, badNonce, 400, nil)
	unknown := env
	unknown.Recipient = "m_unknown"
	f.call(t, "POST", "/v1/messages", macToken, unknown, 404, nil)
	tooBig := env
	tooBig.Ciphertext = make([]byte, MaxWire+1)
	f.call(t, "POST", "/v1/messages", macToken, tooBig, 400, nil)
	env.Recipient = mac.MachineID
	f.call(t, "POST", "/v1/messages", macToken, env, 400, nil)
}

func TestBrokerRevocationSuppressesQueuedMessages(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi := testIdentity(t), testIdentity(t)
	macToken, piToken := f.register(t, mac), f.register(t, pi)
	f.grant(t, piToken, mac, true, false, 1, 200)
	env := testEnvelope(t, mac, pi, "message", "message_revoked")
	f.call(t, "POST", "/v1/messages", macToken, env, 202, nil)
	f.grant(t, piToken, mac, false, false, 2, 200)
	f.call(t, "POST", "/v1/messages", macToken, env, 403, nil)
	s := f.listen(t, piToken)
	select {
	case event := <-s.events:
		if event.Type != "heartbeat" {
			t.Fatalf("revoked ciphertext forwarded: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no stream progress after skipping revoked queue item")
	}
	var count int
	if err := f.b.db.QueryRow(`SELECT count(*) FROM pending_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("revocation unexpectedly altered retained queue: %d %v", count, err)
	}
	// A receipt in the same direction passes without the revoked grant.
	receipt := testEnvelope(t, mac, pi, "receipt", "receipt_after_revoke")
	f.call(t, "POST", "/v1/receipts", macToken, receipt, 202, nil)
	if s.next(t, "envelope").Envelope.ID != receipt.ID {
		t.Fatal("receipt blocked by messaging revocation")
	}
}

func TestBrokerIndependentReceiptCapacityAndByteQuota(t *testing.T) {
	f := testBroker(t, func(c *Config) { c.BrokerCount = 1; c.ReceiptCount = 1 })
	mac, pi := testIdentity(t), testIdentity(t)
	macToken, piToken := f.register(t, mac), f.register(t, pi)
	f.grant(t, piToken, mac, true, false, 1, 200)
	first := testEnvelope(t, mac, pi, "message", "one")
	second := testEnvelope(t, mac, pi, "message", "two")
	f.call(t, "POST", "/v1/messages", macToken, first, 202, nil)
	f.call(t, "POST", "/v1/messages", macToken, first, 200, nil)
	f.call(t, "POST", "/v1/messages", macToken, second, 429, nil)
	receipt := testEnvelope(t, mac, pi, "receipt", "receipt_one")
	f.call(t, "POST", "/v1/receipts", macToken, receipt, 202, nil)
	f.call(t, "POST", "/v1/receipts", macToken, testEnvelope(t, mac, pi, "receipt", "receipt_two"), 429, nil)
	// Reverse receipt requires no Mac->Pi message grant.
	f.call(t, "POST", "/v1/receipts", piToken, testEnvelope(t, pi, mac, "receipt", "reverse_receipt"), 202, nil)
	if _, err := f.b.db.Exec(`UPDATE pending_messages SET expires_at=? WHERE id=?`, Now()-1, first.ID); err != nil {
		t.Fatal(err)
	}
	f.call(t, "POST", "/v1/messages", macToken, second, 202, nil)
	var count int
	if err := f.b.db.QueryRow(`SELECT count(*) FROM pending_messages WHERE id=?`, first.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired ciphertext retained after queue admission")
	}
	limited := testBroker(t, func(c *Config) { c.BrokerBytes = 64 })
	a, b := testIdentity(t), testIdentity(t)
	at, bt := limited.register(t, a), limited.register(t, b)
	limited.grant(t, bt, a, true, false, 1, 200)
	limited.call(t, "POST", "/v1/messages", at, testEnvelope(t, a, b, "message", "oversize_queue"), 429, nil)
}

func testRelay(t *testing.T, from, to Identity, deadline time.Time) RelayQuery {
	t.Helper()
	id := NewID("q_")
	sealed, err := SealQuery(from, Peer{MachineID: to.MachineID, PublicKey: to.PublicKey}, QueryPayload{Purpose: "comms-history-query-v1", ID: id, Sender: from.MachineID, Recipient: to.MachineID, AgentID: "a_brain", Limit: 50, Deadline: deadline.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	return RelayQuery{ID: id, Sender: from.MachineID, Recipient: to.MachineID, Sealed: sealed, Deadline: deadline.UnixMilli()}
}

type testHTTPResult struct {
	status int
	body   []byte
	err    error
}

func (f *brokerFixture) relayAsync(token string, q RelayQuery) <-chan testHTTPResult {
	out := make(chan testHTTPResult, 1)
	go func() {
		req, err := http.NewRequest("POST", f.server.URL+"/v1/history/"+q.Recipient, bytes.NewReader(marshal(q)))
		if err != nil {
			out <- testHTTPResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := f.client.Do(req)
		if err != nil {
			out <- testHTTPResult{err: err}
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		out <- testHTTPResult{resp.StatusCode, data, err}
	}()
	return out
}

func TestBrokerEncryptedOnlineHistoryRelay(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi, other := testIdentity(t), testIdentity(t), testIdentity(t)
	macToken, piToken, otherToken := f.register(t, mac), f.register(t, pi), f.register(t, other)
	s := f.listen(t, piToken)
	q := testRelay(t, mac, pi, time.Now().Add(2*time.Second))
	f.call(t, "POST", "/v1/history/"+pi.MachineID, macToken, q, 403, nil)
	f.grant(t, piToken, mac, true, false, 1, 200)
	f.call(t, "POST", "/v1/history/"+pi.MachineID, macToken, q, 403, nil)
	f.grant(t, piToken, mac, true, true, 2, 200)
	result := f.relayAsync(macToken, q)
	event := s.next(t, "history_query")
	plain, err := OpenQuery(pi, Peer{MachineID: mac.MachineID, PublicKey: mac.PublicKey}, event.Query.Sealed)
	if err != nil || plain.AgentID != "a_brain" {
		t.Fatalf("history query decryption: %+v %v", plain, err)
	}
	answer, err := SealQuery(pi, Peer{MachineID: mac.MachineID, PublicKey: mac.PublicKey}, QueryPayload{Purpose: "comms-history-result-v1", ID: q.ID, Sender: pi.MachineID, Recipient: mac.MachineID, Deadline: q.Deadline, Page: &HistoryPage{Messages: []Message{{ID: "history_one", Body: "private history"}}}})
	if err != nil {
		t.Fatal(err)
	}
	f.call(t, "POST", "/v1/history-results/"+q.ID, otherToken, map[string]any{"sealed": answer}, 403, nil)
	f.call(t, "POST", "/v1/history-results/"+q.ID, piToken, map[string]any{"sealed": answer}, 200, nil)
	response := <-result
	if response.err != nil || response.status != 200 {
		t.Fatalf("history relay: %+v", response)
	}
	var returned struct {
		Sealed []byte `json:"sealed"`
	}
	if err = json.Unmarshal(response.body, &returned); err != nil {
		t.Fatal(err)
	}
	decrypted, err := OpenQuery(mac, Peer{MachineID: pi.MachineID, PublicKey: pi.PublicKey}, returned.Sealed)
	if err != nil || decrypted.Page == nil || decrypted.Page.Messages[0].Body != "private history" {
		t.Fatalf("history response: %+v %v", decrypted, err)
	}
	var count int
	if err = f.b.db.QueryRow(`SELECT count(*) FROM pending_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("query created durable mail: %d %v", count, err)
	}
	f.call(t, "POST", "/v1/history-results/"+q.ID, piToken, map[string]any{"sealed": answer}, 404, nil)
}

func TestBrokerHistoryOfflineTimeoutAndRevocation(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi := testIdentity(t), testIdentity(t)
	macToken, piToken := f.register(t, mac), f.register(t, pi)
	f.grant(t, piToken, mac, true, true, 1, 200)
	f.call(t, "POST", "/v1/history/"+pi.MachineID, macToken, testRelay(t, mac, pi, time.Now().Add(time.Second)), 503, nil)
	s := f.listen(t, piToken)
	q := testRelay(t, mac, pi, time.Now().Add(150*time.Millisecond))
	result := f.relayAsync(macToken, q)
	s.next(t, "history_query")
	if response := <-result; response.err != nil || response.status != 504 {
		t.Fatalf("timeout: %+v", response)
	}
	q = testRelay(t, mac, pi, time.Now().Add(2*time.Second))
	result = f.relayAsync(macToken, q)
	s.next(t, "history_query")
	f.grant(t, piToken, mac, true, false, 2, 200)
	if response := <-result; response.err != nil || response.status != 403 {
		t.Fatalf("revoked in-flight history: %+v", response)
	}
	f.grant(t, piToken, mac, true, true, 3, 200)
	q = testRelay(t, mac, pi, time.Now().Add(2*time.Second))
	result = f.relayAsync(macToken, q)
	s.next(t, "history_query")
	s.cancel()
	if response := <-result; response.err != nil || response.status != 503 {
		t.Fatalf("disconnected target: %+v", response)
	}
}

func TestBrokerRequestValidationAndChallengeBound(t *testing.T) {
	f := testBroker(t, nil)
	identity := testIdentity(t)
	for i := 0; i < 8; i++ {
		f.challengeFor(t, identity)
	}
	f.call(t, "POST", "/v1/auth/challenges", "", map[string]any{"machine_id": identity.MachineID, "public_key": identity.PublicKey}, 429, nil)
	f.call(t, "POST", "/v1/auth/complete", "", map[string]string{"id": "x", "proof": strings.Repeat("x", 5000)}, 413, nil)
	r, err := http.NewRequest("POST", f.server.URL+"/v1/auth/complete", strings.NewReader(`{"id":"a","proof":"b"} {}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("trailing JSON value accepted")
	}
}

func TestBrokerDatabaseRestartRetainsCiphertext(t *testing.T) {
	f := testBroker(t, nil)
	mac, pi := testIdentity(t), testIdentity(t)
	macToken, piToken := f.register(t, mac), f.register(t, pi)
	f.grant(t, piToken, mac, true, false, 1, 200)
	env := testEnvelope(t, mac, pi, "message", "survives_restart")
	f.call(t, "POST", "/v1/messages", macToken, env, 202, nil)
	if err := f.b.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewBroker(f.b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(reopened.Handler())
	next := &brokerFixture{reopened, s, f.client}
	t.Cleanup(func() { reopened.Close(); s.Close() })
	stream := next.listen(t, piToken)
	if stream.next(t, "envelope").Envelope.ID != env.ID {
		t.Fatal("queued ciphertext lost on restart")
	}
}
