package comms

import (
	"context"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joaohts/comms/internal/porter"
)

// Notifications and questions are carried by <machine>:porter, so answers
// come back to porter, which resolves blocking asks and forwards answers to
// the asking agent. The originating agent is in the node-stamped context.

type question struct {
	notify   porter.Notify
	asker    string   // local agent id that gets the answer forwarded; "" for blocking asks
	targets  []string // exact machine_id:agent_id recipients
	expires  time.Time
	state    string // pending | answered | timeout | cancelled
	choice   string
	text     string // the approver's (possibly edited) draft
	by       string
	trust    *porter.Trust
	done     chan struct{}
	resolved time.Time
	// resolution is the porter.Resolved* sent with cancels once settled.
	resolution string
}

// PorterNotifyRequest is the local API body of POST /v1/porter/notify.
type PorterNotifyRequest struct {
	SessionID string   `json:"session_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	To        string   `json:"to"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Reason    string   `json:"reason"`
	Priority  string   `json:"priority,omitempty"`
	Ask       []string `json:"ask,omitempty"`
	Link      string   `json:"link,omitempty"`
	Draft     string   `json:"draft,omitempty"`
	ExpiresMS int64    `json:"expires_ms,omitempty"`
}

// PorterQuestionRequest is the local API body of POST /v1/porter/questions:
// a blocking question to every approver.
type PorterQuestionRequest struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Sender    string `json:"sender,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// PorterQuestion is the state of a question as the local API reports it.
type PorterQuestion struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Choice    string    `json:"choice,omitempty"`
	Text      string    `json:"text,omitempty"`
	By        string    `json:"by,omitempty"`
	ByAlias   string    `json:"by_alias,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (n *Node) porterRoutes(mux *http.ServeMux) {
	enabled := func(w http.ResponseWriter) *porterBridge {
		if n.porter == nil {
			nodeError(w, problem(409, "porter_disabled", "porter is off; start the node with comms serve --porter"))
			return nil
		}
		return n.porter
	}
	mux.HandleFunc("POST /v1/porter/notify", func(w http.ResponseWriter, r *http.Request) {
		b := enabled(w)
		if b == nil {
			return
		}
		var q PorterNotifyRequest
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		v, e := b.notify(r.Context(), q)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 202, v)
	})
	mux.HandleFunc("POST /v1/porter/questions", func(w http.ResponseWriter, r *http.Request) {
		b := enabled(w)
		if b == nil {
			return
		}
		var q PorterQuestionRequest
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		v, e := b.ask(r.Context(), q)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 202, v)
	})
	mux.HandleFunc("GET /v1/porter/questions/{id}", func(w http.ResponseWriter, r *http.Request) {
		b := enabled(w)
		if b == nil {
			return
		}
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait_ms"))
		v, e := b.wait(r.Context(), r.PathValue("id"), time.Duration(min(max(wait, 0), 25000))*time.Millisecond)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("DELETE /v1/porter/questions/{id}", func(w http.ResponseWriter, r *http.Request) {
		b := enabled(w)
		if b == nil {
			return
		}
		b.resolve(r.PathValue("id"), "cancelled", "", "", porter.ResolvedTerminal)
		v, e := b.wait(r.Context(), r.PathValue("id"), 0)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
}

// stamp builds the context of a notification from the caller's comms session
// and/or the porter agent it reports for. Callers cannot supply it directly.
func (b *porterBridge) stamp(sessionID, agent string) (porter.Context, string, error) {
	c := porter.Context{Machine: b.machine()}
	asker := ""
	if sessionID != "" {
		s, e := b.n.Store.Session(sessionID)
		if e != nil || s.EndedAt != nil {
			return c, "", problem(403, "no_attachment", "sender requires a current attachment")
		}
		a, e := b.n.Store.Agent(s.AgentID)
		if e != nil {
			return c, "", e
		}
		asker = a.ID
		c.Agent, c.Harness, c.Session, c.Title = c.Machine+":"+a.Alias, s.Harness, s.HarnessID, a.Alias
		if agent == "" && s.Harness != "service" {
			agent = s.HarnessID
		}
	}
	if agent != "" {
		if st, e := b.state.Load(); e == nil {
			if a := st.Agents[agent]; a != nil {
				v := a.View()
				c.Harness, c.Session, c.Project = v.Harness, v.ID, v.Project
				if v.Title != "" {
					c.Title = v.Title
				}
				if v.Identity != nil && c.Agent == "" {
					c.Agent = v.Identity.Address
				}
			}
		}
		if c.Session == "" {
			c.Session = agent
		}
	}
	return c, asker, nil
}

// target resolves a notify recipient: a bare peer means that machine's app.
func (b *porterBridge) target(ctx context.Context, to string) (string, string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", "", problem(400, "missing_recipient", "--to is required")
	}
	if !strings.Contains(to, ":") {
		to += ":" + AppAlias
	}
	machine, agent, e := b.n.resolve(ctx, to, true)
	if e != nil {
		return "", "", e
	}
	if machine == b.n.Store.Identity.MachineID {
		return "", "", problem(400, "local_recipient", "notifications go to a peer machine")
	}
	return machine, agent, nil
}

func (b *porterBridge) notify(ctx context.Context, q PorterNotifyRequest) (map[string]any, error) {
	c, asker, e := b.stamp(q.SessionID, q.Agent)
	if e != nil {
		return nil, e
	}
	nt := porter.Notify{Type: porter.TypeNotify, V: porter.WireVersion, ID: porter.NewID("ntf_"), Kind: porter.NotifyMessage,
		Title: q.Title, Body: q.Body, Reason: q.Reason, Priority: q.Priority, Ask: q.Ask, Link: q.Link, Draft: q.Draft, Context: c, SentAt: time.Now().UTC().Truncate(time.Second)}
	if e = porter.ValidateNotify(&nt); e != nil {
		return nil, problem(400, "bad_notify", e.Error())
	}
	if len(nt.Ask) > 0 && asker == "" {
		return nil, problem(400, "no_attachment", "--ask needs a comms identity to receive the answer; use --from ALIAS")
	}
	machine, agent, e := b.target(ctx, q.To)
	if e != nil {
		return nil, e
	}
	var qn *question
	if len(nt.Ask) > 0 {
		ttl := time.Duration(q.ExpiresMS) * time.Millisecond
		if ttl <= 0 || ttl > MessageTTL {
			ttl = time.Hour
		}
		exp := nt.SentAt.Add(ttl)
		nt.ExpiresAt = &exp
		qn = &question{notify: nt, asker: asker, targets: []string{machine + ":" + agent}, expires: exp, state: "pending", done: make(chan struct{})}
		b.mu.Lock()
		b.questions[nt.ID] = qn
		b.mu.Unlock()
	}
	if e = b.deliver(machine, agent, nt); e != nil {
		if qn != nil {
			b.mu.Lock()
			delete(b.questions, nt.ID)
			b.mu.Unlock()
		}
		return nil, e
	}
	return map[string]any{"id": nt.ID, "to": machine + ":" + agent}, nil
}

// deliver sends the notify message and its encrypted push.
func (b *porterBridge) deliver(machine, agent string, nt porter.Notify) error {
	if e := b.send(machine+":"+agent, nt); e != nil {
		return e
	}
	b.mu.Lock()
	sub, ok := b.subs[machine]
	b.mu.Unlock()
	if ok && b.granted(machine) {
		b.pushTo(sub, nil, &nt)
	}
	return nil
}

func (b *porterBridge) ask(ctx context.Context, q PorterQuestionRequest) (PorterQuestion, error) {
	cfg := b.config()
	if len(cfg.Approvers) == 0 {
		return PorterQuestion{}, problem(409, "no_approvers", "porter has no approvers; run comms porter setup --approver PEER")
	}
	c, asker, e := b.stamp(q.SessionID, q.Agent)
	if e != nil {
		return PorterQuestion{}, e
	}
	timeout := time.Duration(q.TimeoutMS) * time.Millisecond
	if timeout <= 0 || timeout > time.Hour {
		timeout = cfg.AskTimeout
	}
	// The wire carries whole seconds, but the deadline itself must not be
	// truncated: a sub-second timeout would otherwise be born expired.
	exact := time.Now().UTC()
	now := exact.Truncate(time.Second)
	exp := exact.Add(timeout).Truncate(time.Second)
	nt := porter.Notify{Type: porter.TypeNotify, V: porter.WireVersion, ID: porter.NewID("ntf_"), Kind: q.Kind, Priority: "high", Context: c, SentAt: now, ExpiresAt: &exp}
	qn := &question{expires: exact.Add(timeout), state: "pending", done: make(chan struct{})}
	switch q.Kind {
	case porter.NotifyPermission:
		nt.Title, nt.Body, nt.Reason, nt.Ask = "Permission needed", q.Text, "an agent is waiting on a permission prompt", porter.PermissionChoices
	case porter.NotifyTrust:
		if asker == "" {
			return PorterQuestion{}, problem(400, "no_attachment", "ask-trust needs the receiving agent's comms identity; use --from ALIAS")
		}
		t, e := b.pendingTrust(ctx, asker, q.Sender)
		if e != nil {
			return PorterQuestion{}, e
		}
		qn.trust = &t
		nt.Trust = &porter.TrustRef{Receiver: t.Receiver, Sender: t.Sender}
		nt.Title, nt.Reason, nt.Ask = "Trust request", "first order from an untrusted peer agent", porter.TrustChoices
		nt.Body = t.Sender + " wants to give orders to " + t.Receiver
		if q.Text != "" {
			nt.Body += ": " + q.Text
		}
	default:
		return PorterQuestion{}, problem(400, "bad_kind", "question kind must be permission or trust")
	}
	if e = porter.ValidateNotify(&nt); e != nil {
		return PorterQuestion{}, problem(400, "bad_notify", e.Error())
	}
	qn.notify = nt
	b.mu.Lock()
	b.questions[nt.ID] = qn
	b.mu.Unlock()
	for _, approver := range cfg.Approvers {
		machine, agent, e := b.target(ctx, approver)
		if e == nil {
			e = b.deliver(machine, agent, nt)
		}
		if e != nil {
			log.Printf("porter ask %s: %s", approver, errorCode(e))
			continue
		}
		// The question may have settled (expired, answered by another
		// approver) while this delivery was in flight; resolve only cancels
		// targets it already knows, so cancel a late one here.
		b.mu.Lock()
		qn.targets = append(qn.targets, machine+":"+agent)
		settled, resolution, by := qn.state != "pending", qn.resolution, qn.by
		b.mu.Unlock()
		if settled && (by == "" || machine != by) {
			b.send(machine+":"+agent, porter.Cancel{Type: porter.TypeCancel, V: porter.WireVersion, ID: nt.ID, Resolution: resolution})
		}
	}
	b.mu.Lock()
	reached := len(qn.targets)
	if reached == 0 {
		delete(b.questions, nt.ID)
	}
	b.mu.Unlock()
	if reached == 0 {
		return PorterQuestion{}, problem(502, "approvers_unreachable", "no approver could be reached")
	}
	return PorterQuestion{ID: nt.ID, State: "pending", ExpiresAt: exp}, nil
}

// pendingTrust describes the trust a kind=trust question would store.
func (b *porterBridge) pendingTrust(ctx context.Context, receiverID, sender string) (porter.Trust, error) {
	machine, agent, ok := strings.Cut(strings.TrimSpace(sender), ":")
	if !ok || !agentIDPattern.MatchString(agent) {
		return porter.Trust{}, problem(400, "bad_sender", "sender must be the exact machine_id:agent_id from the message")
	}
	p, e := b.n.Store.Peer(machine)
	if e != nil || p.MachineID != machine {
		return porter.Trust{}, problem(404, "unknown_peer", "sender machine is not paired")
	}
	r, e := b.n.Store.Agent(receiverID)
	if e != nil {
		return porter.Trust{}, e
	}
	name := agent
	var who []Presence
	if b.n.currentBrokerURL() != "" && b.n.brokerRequest(ctx, "GET", "/v1/who", nil, &who) == nil {
		for _, v := range who {
			if v.MachineID == machine && v.AgentID == agent {
				name = v.Alias
			}
		}
	}
	return porter.Trust{Receiver: b.machine() + ":" + r.Alias, ReceiverID: r.ID, Sender: p.Alias + ":" + name, SenderID: machine + ":" + agent}, nil
}

func (b *porterBridge) answer(peer, id, choice, text string) {
	b.mu.Lock()
	q := b.questions[id]
	b.mu.Unlock()
	if q == nil || !b.config().IsApprover(peer) || time.Now().After(q.expires) || !slices.Contains(q.notify.Ask, choice) {
		log.Printf("porter: ignored answer (unknown, expired, invalid or not from an approver)")
		return
	}
	b.mu.Lock()
	if q.state == "pending" {
		q.text = text
	}
	b.mu.Unlock()
	b.resolve(id, "answered", choice, peer, porter.ResolvedAnswered)
}

// cancelQuestionsTo withdraws pending questions from peer's recipients (it
// resigned as an approver, so its answers no longer count). A question left
// with no recipient is cancelled for its asker too.
func (b *porterBridge) cancelQuestionsTo(peer string) {
	type cancel struct{ to, id string }
	var cancels []cancel
	var orphaned []string
	b.mu.Lock()
	for id, q := range b.questions {
		if q.state != "pending" {
			continue
		}
		kept := q.targets[:0:0]
		for _, t := range q.targets {
			if strings.HasPrefix(t, peer+":") {
				cancels = append(cancels, cancel{t, id})
			} else {
				kept = append(kept, t)
			}
		}
		if len(kept) != len(q.targets) {
			q.targets = kept
			if len(kept) == 0 {
				orphaned = append(orphaned, id)
			}
		}
	}
	b.mu.Unlock()
	for _, c := range cancels {
		b.send(c.to, porter.Cancel{Type: porter.TypeCancel, V: porter.WireVersion, ID: c.id, Resolution: porter.ResolvedResigned})
	}
	for _, id := range orphaned {
		b.resolve(id, "cancelled", "", "", porter.ResolvedResigned)
	}
}

// resolve settles a pending question once and tells everyone else.
func (b *porterBridge) resolve(id, state, choice, by, resolution string) {
	b.mu.Lock()
	q := b.questions[id]
	if q == nil || q.state != "pending" {
		b.mu.Unlock()
		return
	}
	q.state, q.choice, q.by, q.resolved, q.resolution = state, choice, by, time.Now(), resolution
	close(q.done)
	targets := slices.Clone(q.targets)
	b.mu.Unlock()
	for _, t := range targets {
		if by != "" && strings.HasPrefix(t, by+":") {
			continue
		}
		b.send(t, porter.Cancel{Type: porter.TypeCancel, V: porter.WireVersion, ID: id, Resolution: resolution})
	}
	if state == "answered" && q.trust != nil && (choice == "This session" || choice == "Always") {
		t := *q.trust
		t.Scope, t.ApprovedAt = porter.TrustSession, time.Now().UTC().Truncate(time.Second)
		if choice == "Always" {
			t.Scope = porter.TrustAlways
		}
		e := porter.UpdateJSON(b.trustsPath, func(ts *porter.Trusts) (bool, error) { ts.Put(t); return true, nil })
		if e != nil {
			log.Printf("porter trusts: %v", e)
		}
	}
	if q.asker != "" {
		if state == "answered" {
			b.mu.Lock()
			text := q.text
			b.mu.Unlock()
			b.send(q.asker, porter.Answer{Type: porter.TypeAnswer, V: porter.WireVersion, ID: id, Choice: choice, Text: text})
		} else {
			b.send(q.asker, porter.Cancel{Type: porter.TypeCancel, V: porter.WireVersion, ID: id, Resolution: resolution})
		}
	}
}

func (b *porterBridge) expireQuestions(now time.Time) {
	b.mu.Lock()
	var due []string
	for id, q := range b.questions {
		if q.state == "pending" && now.After(q.expires) {
			due = append(due, id)
		} else if q.state != "pending" && now.Sub(q.resolved) > 10*time.Minute {
			delete(b.questions, id)
		}
	}
	b.mu.Unlock()
	for _, id := range due {
		b.resolve(id, "timeout", "", "", porter.ResolvedTimeout)
	}
}

// wait reports a question, waiting up to d for it to settle.
func (b *porterBridge) wait(ctx context.Context, id string, d time.Duration) (PorterQuestion, error) {
	b.mu.Lock()
	q := b.questions[id]
	b.mu.Unlock()
	if q == nil {
		return PorterQuestion{}, problem(404, "unknown_question", "question not found")
	}
	if until := time.Until(q.expires); until < d {
		d = max(until, 0)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-q.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	if time.Now().After(q.expires) {
		b.resolve(id, "timeout", "", "", porter.ResolvedTimeout)
	}
	b.mu.Lock()
	out := PorterQuestion{ID: id, State: q.state, Choice: q.choice, Text: q.text, By: q.by, ExpiresAt: q.expires}
	b.mu.Unlock()
	if out.By != "" {
		if p, e := b.n.Store.Peer(out.By); e == nil {
			out.ByAlias = p.Alias
		}
	}
	return out, nil
}
