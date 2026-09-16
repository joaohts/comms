package comms

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"
)

func (n *Node) currentBrokerURL() string { n.bcMu.Lock(); defer n.bcMu.Unlock(); return n.brokerURL }
func validBrokerURL(raw string, insecure bool) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return problem(400, "bad_broker_url", "broker URL must be an HTTPS origin without credentials/query")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (insecure || u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return problem(400, "https_required", "remote brokers require HTTPS; HTTP is allowed only on loopback")
}
func (n *Node) ConfigureBroker(ctx context.Context, raw string) error {
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		n.bcMu.Lock()
		if n.streamCancel != nil {
			n.streamCancel()
		}
		n.brokerURL = ""
		n.brokerToken = ""
		n.brokerTokenExpiry = 0
		n.bcMu.Unlock()
		n.brokerOnline.Store(false)
		return n.Store.SetSetting("broker_url", "")
	}
	if e := validBrokerURL(raw, n.cfg.AllowInsecure); e != nil {
		return e
	}
	token, expires, e := n.authenticate(ctx, raw)
	if e != nil {
		return e
	}
	if e = n.Store.SetSetting("broker_url", raw); e != nil {
		return e
	}
	n.bcMu.Lock()
	if n.streamCancel != nil {
		n.streamCancel()
	}
	n.brokerURL = raw
	n.brokerToken = token
	n.brokerTokenExpiry = expires
	n.bcMu.Unlock()
	n.requestSync()
	n.notify()
	return nil
}
func (n *Node) rawRequest(ctx context.Context, base, token, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(marshal(body))
	}
	req, e := http.NewRequestWithContext(ctx, method, base+path, reader)
	if e != nil {
		return e
	}
	req.Header.Set("User-Agent", "comms/"+Version)
	if n.cfg.BrokerServiceKey != "" {
		req.Header.Set(ServiceKeyHeader, n.cfg.BrokerServiceKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, e := n.httpClient.Do(req)
	if e != nil {
		return problem(503, "broker_unavailable", "broker request failed: "+e.Error())
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var a APIError
		if json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&a) != nil || a.Code == "" {
			a = APIError{Code: "broker_error", ErrorText: http.StatusText(res.StatusCode)}
		}
		a.Status = res.StatusCode
		return &a
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(res.Body, 3*MaxHistory)).Decode(out); e != nil {
			return problem(502, "bad_broker_response", "invalid broker JSON response")
		}
	} else {
		io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
	}
	return nil
}
func (n *Node) authenticate(ctx context.Context, base string) (string, int64, error) {
	var start struct {
		ID     string `json:"id"`
		Sealed []byte `json:"sealed"`
	}
	if e := n.rawRequest(ctx, base, "", "POST", "/v1/auth/challenges", map[string]any{"machine_id": n.Store.Identity.MachineID, "public_key": n.Store.Identity.PublicKey}, &start); e != nil {
		return "", 0, e
	}
	if len(start.Sealed) > 8192 {
		return "", 0, problem(502, "bad_challenge", "challenge exceeds limit")
	}
	pub, _ := key32(n.Store.Identity.PublicKey)
	priv, _ := key32(n.Store.Identity.PrivateKey)
	plain, ok := box.OpenAnonymous(nil, start.Sealed, pub, priv)
	if !ok {
		return "", 0, problem(403, "bad_challenge", "challenge authentication failed")
	}
	var challenge struct {
		Purpose   string `json:"purpose"`
		ID        string `json:"id"`
		MachineID string `json:"machine_id"`
		PublicKey []byte `json:"public_key"`
		Secret    string `json:"secret"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if e := json.Unmarshal(plain, &challenge); e != nil {
		return "", 0, e
	}
	if challenge.Purpose != "comms-auth-v1" || challenge.ID != start.ID || challenge.MachineID != n.Store.Identity.MachineID || !SameKey(challenge.PublicKey, n.Store.Identity.PublicKey) || challenge.ExpiresAt <= Now() || challenge.ExpiresAt > time.Now().Add(2*time.Minute).UnixMilli() || len(challenge.Secret) < 32 || len(challenge.Secret) > 128 {
		return "", 0, problem(403, "bad_challenge", "challenge purpose/identity/deadline mismatch")
	}
	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if e := n.rawRequest(ctx, base, "", "POST", "/v1/auth/complete", map[string]string{"id": start.ID, "proof": challenge.Secret}, &result); e != nil {
		return "", 0, e
	}
	if len(result.Token) < 32 || result.ExpiresAt <= Now() {
		return "", 0, problem(502, "bad_credential", "invalid broker credential")
	}
	return result.Token, result.ExpiresAt, nil
}
func (n *Node) credentials(ctx context.Context) (string, string, error) {
	n.bcMu.Lock()
	defer n.bcMu.Unlock()
	if n.brokerURL == "" {
		return "", "", problem(503, "broker_not_configured", "configure a broker before using remote comms")
	}
	if n.brokerToken != "" && n.brokerTokenExpiry > time.Now().Add(time.Minute).UnixMilli() {
		return n.brokerURL, n.brokerToken, nil
	}
	token, expiry, e := n.authenticate(ctx, n.brokerURL)
	if e != nil {
		return "", "", e
	}
	n.brokerToken = token
	n.brokerTokenExpiry = expiry
	return n.brokerURL, token, nil
}
func (n *Node) brokerRequest(ctx context.Context, method, path string, in, out any) error {
	base, token, e := n.credentials(ctx)
	if e != nil {
		return e
	}
	e = n.rawRequest(ctx, base, token, method, path, in, out)
	var a *APIError
	if errors.As(e, &a) && a.Status == 401 {
		n.bcMu.Lock()
		if n.brokerToken == token {
			n.brokerToken = ""
			n.brokerTokenExpiry = 0
		}
		n.bcMu.Unlock()
	}
	return e
}

func (n *Node) brokerLoop() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); n.brokerReader() }()
	go func() { defer wg.Done(); n.brokerSender() }()
	tick := time.NewTicker(n.cfg.Heartbeat)
	defer tick.Stop()
	defer wg.Wait()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.syncWake:
		case <-tick.C:
		}
		if n.currentBrokerURL() == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
		e := n.syncBroker(ctx)
		cancel()
		if e != nil && n.ctx.Err() == nil {
			log.Printf("broker sync: %s", errorCode(e))
		}
	}
}
func (n *Node) syncBroker(ctx context.Context) error {
	grants, e := n.Store.Grants()
	if e != nil {
		return e
	}
	for _, g := range grants {
		if e = n.brokerRequest(ctx, "PUT", "/v1/grants/"+url.PathEscape(g.Grantee), g, nil); e != nil {
			return e
		}
	}
	agents, e := n.Store.Agents(false)
	if e != nil {
		return e
	}
	presence := []Presence{}
	for _, a := range agents {
		if a.Scope != "global" {
			continue
		}
		online := n.ready(a.ID)
		if !online && !a.Persistent {
			continue
		}
		presence = append(presence, Presence{MachineID: n.Store.Identity.MachineID, AgentID: a.ID, Alias: a.Alias, Persistent: a.Persistent, Online: online, ExpiresAt: Now() + n.cfg.Lease.Milliseconds()})
	}
	rev, _ := strconv.ParseInt(n.Store.Setting("presence_revision"), 10, 64)
	if rev < Now() {
		rev = Now()
	} else {
		rev++
	}
	if e = n.Store.SetSetting("presence_revision", strconv.FormatInt(rev, 10)); e != nil {
		return e
	}
	return n.brokerRequest(ctx, "PUT", "/v1/presence", map[string]any{"revision": rev, "agents": presence}, nil)
}
func (n *Node) brokerReader() {
	for n.ctx.Err() == nil {
		if n.currentBrokerURL() == "" {
			select {
			case <-n.ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		ctx, cancel := context.WithCancel(n.ctx)
		n.bcMu.Lock()
		n.streamCancel = cancel
		n.bcMu.Unlock()
		e := n.readBrokerStream(ctx)
		cancel()
		n.brokerOnline.Store(false)
		if e != nil && n.ctx.Err() == nil {
			log.Printf("broker receive: %s", errorCode(e))
		}
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func (n *Node) readBrokerStream(ctx context.Context) error {
	base, token, e := n.credentials(ctx)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", base+"/v1/stream", nil)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "comms/"+Version)
	if n.cfg.BrokerServiceKey != "" {
		req.Header.Set(ServiceKeyHeader, n.cfg.BrokerServiceKey)
	}
	client := &http.Client{Transport: n.httpClient.Transport, CheckRedirect: noBrokerRedirect}
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if res.StatusCode == 401 {
			n.bcMu.Lock()
			if n.brokerToken == token {
				n.brokerToken = ""
			}
			n.bcMu.Unlock()
		}
		return problem(res.StatusCode, "stream_unavailable", "broker stream unavailable")
	}
	n.brokerOnline.Store(true)
	n.requestSync()
	n.notify()
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(make([]byte, 4096), 3*MaxHistory)
	for scan.Scan() {
		var ev Event
		if e = json.Unmarshal(scan.Bytes(), &ev); e != nil {
			return e
		}
		switch ev.Type {
		case "heartbeat":
			continue
		case "envelope":
			if ev.Envelope != nil {
				if e = n.acceptEnvelope(ctx, *ev.Envelope); e != nil {
					log.Printf("envelope rejected: %s", errorCode(e))
				}
			}
		case "history_query":
			if ev.Query != nil {
				q := *ev.Query
				select {
				case n.querySlots <- struct{}{}:
					go func() { defer func() { <-n.querySlots }(); n.answerHistory(ctx, q) }()
				default:
					go n.answerHistoryError(ctx, q, problem(429, "query_busy", "history query capacity is busy"))
				}
			}
		}
	}
	return scan.Err()
}

func (n *Node) acceptEnvelope(ctx context.Context, env Envelope) error {
	peer, e := n.Store.Peer(env.Sender)
	if e != nil {
		return n.discardEnvelope(ctx, env, problem(403, "unpaired_sender", "sender is not pinned"))
	}
	p, e := Decrypt(n.Store.Identity, peer, env)
	if e != nil {
		return n.discardEnvelope(ctx, env, e)
	}
	if env.Kind == "receipt" {
		if e = n.Store.ApplyReceipt(peer.MachineID, *p.Receipt); e != nil {
			return n.discardEnvelope(ctx, env, e)
		}
		n.publish("messages")
		return n.ackEnvelope(ctx, env)
	}
	state := "received"
	failure := ""
	a, e := n.Store.Agent(p.RecipientAgent)
	if e != nil || a.RetiredAt != nil || a.ID != p.RecipientAgent {
		state = "undeliverable"
		failure = "agent_ended"
	} else if a.Scope != "global" {
		state = "undeliverable"
		failure = "local_only"
	} else if g, e := n.Store.Grant(peer.MachineID); e != nil || !g.Messages {
		state = "undeliverable"
		failure = "grant_revoked"
	}
	now := Now()
	m := Message{ID: p.ID, SenderMachine: p.Sender, SenderAgent: p.SenderAgent, RecipientMachine: p.Recipient, RecipientAgent: p.RecipientAgent, Kind: "message", Body: p.Body, State: state, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, ReceivedAt: &now, Failure: failure, Hash: digest(marshal(p))}
	if _, _, e = n.Store.Insert(m, true); e != nil {
		return e
	} // quota/storage failure leaves broker item unacknowledged
	n.notify()
	n.publish("messages")
	return n.ackEnvelope(ctx, env)
}
func (n *Node) ackEnvelope(ctx context.Context, e Envelope) error {
	return n.brokerRequest(ctx, "POST", "/v1/acks", map[string]string{"sender_machine_id": e.Sender, "id": e.ID}, nil)
}
func (n *Node) discardEnvelope(ctx context.Context, e Envelope, reason error) error {
	if err := n.ackEnvelope(ctx, e); err != nil {
		return err
	}
	return reason
}
func (n *Node) brokerSender() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	sem := make(chan struct{}, n.cfg.Workers)
	var activeMu sync.Mutex
	active := map[string]bool{}
	var tasks sync.WaitGroup
	defer tasks.Wait()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.outWake:
		case <-ticker.C:
		}
		if n.currentBrokerURL() == "" || n.closing.Load() {
			continue
		}
		if e := n.Store.FlushReceipts(); e != nil {
			log.Printf("receipt obligations: %v", e)
		}
		pending, e := n.Store.PendingOutgoing(256)
		if e != nil {
			log.Printf("outgoing queue: %v", e)
			continue
		}
		for _, m := range pending {
			activeMu.Lock()
			busy := active[m.Key()]
			activeMu.Unlock()
			if busy {
				continue
			}
			select {
			case sem <- struct{}{}:
			default:
				continue
			}
			activeMu.Lock()
			active[m.Key()] = true
			activeMu.Unlock()
			tasks.Add(1)
			go func(m Message) {
				defer tasks.Done()
				defer func() {
					activeMu.Lock()
					delete(active, m.Key())
					activeMu.Unlock()
					<-sem
					select {
					case n.outWake <- struct{}{}:
					default:
					}
				}()
				ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
				defer cancel()
				if e := n.forward(ctx, m); e != nil {
					state := m.State
					code := errorCode(e)
					var api *APIError
					if errors.As(e, &api) && (api.Status == 400 || api.Status == 409 || api.Status == 413 || (api.Status == 403 && api.Code == "message_denied")) {
						state = "undeliverable"
						code = "broker_rejected:" + code
					}
					if se := n.Store.MarkTransport(m, state, code, retryAt(m.Attempts), nil); se != nil {
						log.Printf("transport outcome: %v", se)
					}
				}
			}(m)
		}
	}
}
func (n *Node) forward(ctx context.Context, m Message) error {
	var env Envelope
	var wire []byte
	if len(m.Wire) > 0 {
		if e := json.Unmarshal(m.Wire, &env); e != nil {
			return e
		}
	} else {
		p, e := n.Store.Peer(m.RecipientMachine)
		if e != nil {
			return e
		}
		env, e = Encrypt(n.Store.Identity, p, payloadFor(m))
		if e != nil {
			return e
		}
		wire = marshal(env)
		if e = n.Store.SaveWire(m, wire); e != nil {
			return e
		}
	}
	path := "/v1/messages"
	if m.Kind == "receipt" {
		path = "/v1/receipts"
	}
	if e := n.brokerRequest(ctx, "POST", path, env, nil); e != nil {
		return e
	}
	state := "forwarded"
	if m.Kind == "receipt" {
		state = "handed_off"
	}
	if e := n.Store.MarkTransport(m, state, "", retryAt(m.Attempts), nil); e != nil {
		return e
	}
	n.publish("messages")
	return nil
}

func (n *Node) History(ctx context.Context, q HistoryRequest) (HistoryPage, error) {
	if q.Target == "" && q.AgentID != "" {
		q.Target = q.AgentID
	}
	if q.Target == "" {
		return n.Store.History("", q.Limit, q.Cursor, q.Pending, false)
	}
	// Retired local identities retain readable history by immutable ID.
	parts := strings.SplitN(q.Target, ":", 2)
	if len(parts) == 1 || parts[0] == "self" || parts[0] == n.Store.Setting("name") || parts[0] == n.Store.Identity.MachineID {
		a, e := n.Store.Agent(parts[len(parts)-1])
		if e != nil {
			return HistoryPage{}, e
		}
		return n.Store.History(a.ID, q.Limit, q.Cursor, q.Pending, false)
	}
	allow := true
	if q.SessionID != "" {
		s, e := n.Store.Session(q.SessionID)
		if e != nil || s.EndedAt != nil {
			return HistoryPage{}, problem(403, "no_attachment", "requesting attachment ended")
		}
		allow = s.Scope == "global"
	}
	machine, agent, e := n.resolve(ctx, q.Target, allow)
	if e != nil {
		return HistoryPage{}, e
	}
	if machine == n.Store.Identity.MachineID {
		return n.Store.History(agent, q.Limit, q.Cursor, q.Pending, false)
	}
	if q.Pending {
		return HistoryPage{}, problem(400, "remote_inbox", "remote history cannot consume or inspect another agent's pending inbox")
	}
	peer, e := n.Store.Peer(machine)
	if e != nil {
		return HistoryPage{}, e
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	deadline := time.Now().Add(10 * time.Second).UnixMilli()
	id := NewID("q_")
	p := QueryPayload{Purpose: "comms-history-query-v1", ID: id, Sender: n.Store.Identity.MachineID, Recipient: machine, AgentID: agent, Limit: q.Limit, Cursor: q.Cursor, Deadline: deadline}
	sealed, e := SealQuery(n.Store.Identity, peer, p)
	if e != nil {
		return HistoryPage{}, e
	}
	query := RelayQuery{id, n.Store.Identity.MachineID, machine, sealed, deadline}
	var result struct {
		Sealed []byte `json:"sealed"`
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if e = n.brokerRequest(ctx, "POST", "/v1/history/"+url.PathEscape(machine), query, &result); e != nil {
		return HistoryPage{}, e
	}
	reply, e := OpenQuery(n.Store.Identity, peer, result.Sealed)
	if e != nil {
		return HistoryPage{}, e
	}
	if reply.Purpose != "comms-history-result-v1" || reply.ID != id || reply.AgentID != agent || reply.Deadline != deadline {
		return HistoryPage{}, problem(403, "query_mismatch", "history response does not match request")
	}
	if reply.Error != nil {
		reply.Error.Status = 403
		if reply.Error.Code == "query_busy" {
			reply.Error.Status = 429
		}
		return HistoryPage{}, reply.Error
	}
	if reply.Page == nil || len(reply.Page.Messages) > q.Limit || len(marshal(reply.Page)) > MaxHistory {
		return HistoryPage{}, problem(502, "bad_history_response", "history response exceeds bounds")
	}
	for _, m := range reply.Page.Messages {
		if m.Kind != "message" || m.SenderMachine == m.RecipientMachine || (!(m.SenderMachine == machine && m.SenderAgent == agent) && !(m.RecipientMachine == machine && m.RecipientAgent == agent)) {
			return HistoryPage{}, problem(403, "bad_history_response", "history response contains excluded messages")
		}
	}
	return *reply.Page, nil
}
func (n *Node) answerHistory(ctx context.Context, q RelayQuery) { n.answerHistoryError(ctx, q, nil) }
func (n *Node) answerHistoryError(ctx context.Context, q RelayQuery, prior error) {
	peer, e := n.Store.Peer(q.Sender)
	if e != nil {
		return
	}
	p, e := OpenQuery(n.Store.Identity, peer, q.Sealed)
	if e != nil {
		return
	}
	if p.Purpose != "comms-history-query-v1" || p.ID != q.ID || p.Deadline != q.Deadline || q.Recipient != n.Store.Identity.MachineID || p.Limit < 1 || p.Limit > 200 {
		return
	}
	var page HistoryPage
	e = prior
	if e == nil {
		g, ge := n.Store.Grant(peer.MachineID)
		a, ae := n.Store.Agent(p.AgentID)
		if ge != nil || !g.History || ae != nil || a.Scope != "global" {
			e = problem(403, "history_denied", "history access is not granted for this agent")
		} else {
			page, e = n.Store.History(p.AgentID, p.Limit, p.Cursor, false, true)
		}
	}
	if e == nil {
		g, ge := n.Store.Grant(peer.MachineID)
		a, ae := n.Store.Agent(p.AgentID)
		if ge != nil || !g.History || ae != nil || a.Scope != "global" {
			e = problem(403, "history_denied", "history permission was revoked")
		}
	}
	reply := QueryPayload{Purpose: "comms-history-result-v1", ID: p.ID, Sender: n.Store.Identity.MachineID, Recipient: peer.MachineID, AgentID: p.AgentID, Deadline: p.Deadline}
	if e != nil {
		var a *APIError
		if !errors.As(e, &a) {
			a = &APIError{Code: "query_failed", ErrorText: "history query failed", Status: 500}
		}
		reply.Error = a
	} else {
		reply.Page = &page
	}
	sealed, e := SealQuery(n.Store.Identity, peer, reply)
	if e != nil {
		return
	}
	if e = n.brokerRequest(ctx, "POST", "/v1/history-results/"+url.PathEscape(q.ID), map[string]any{"sealed": sealed}, nil); e != nil && ctx.Err() == nil {
		log.Printf("history reply: %s", errorCode(e))
	}
}
