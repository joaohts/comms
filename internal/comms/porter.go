package comms

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/joaohts/comms/internal/porter"
)

// PorterAlias is the persistent global agent through which porter talks to
// peers. Peers address it as <machine>:porter.
const PorterAlias = "porter"

// AppAlias is the agent a phone runs; `--to phone` means phone:app.
const AppAlias = "app"

// The porter bridge is a node-owned receiver, like native Codex: the node is
// already always on, so it attaches the porter identity itself, takes
// subscriptions and control messages from granted peers, publishes topics,
// carries notifications and questions, and caches peers' topics. Agent state
// is written to <data-dir>/porter/state.json by short-lived `comms porter`
// processes (hooks); the bridge polls it for changes.
type porterBridge struct {
	n          *Node
	dir        string
	cfgPath    string
	state      porter.Store
	file       porter.Subscribers
	trustsPath string
	cache      porter.Cache
	push       porter.Pusher
	pushes     sync.WaitGroup

	mu        sync.Mutex
	session   Session
	subs      map[string]porter.Subscriber
	questions map[string]*question
	trusts    *porter.Trusts
	trustInfo os.FileInfo

	// Owned by the attach goroutine.
	last          map[string]*porter.Agent
	seen          os.FileInfo
	lastTrusts    map[string]porter.Trust
	trustSeen     os.FileInfo
	running       int
	lastSubscribe time.Time
	peerOK        map[string]bool
	trustMissing  map[string]time.Time
	lastSweep     time.Time
	lastStatus    time.Time
	lastPrune     time.Time
}

func newPorterBridge(n *Node) *porterBridge {
	dir := filepath.Join(n.cfg.DataDir, "porter")
	b := &porterBridge{n: n, dir: dir, cfgPath: n.cfg.PorterConfig, state: porter.Store{Path: filepath.Join(dir, "state.json")},
		file: porter.Subscribers{Path: filepath.Join(dir, "subscribers.json")}, trustsPath: filepath.Join(dir, "trusts.json"),
		cache: porter.Cache{Dir: filepath.Join(dir, "cache")}, push: n.cfg.PorterPush, questions: map[string]*question{},
		peerOK: map[string]bool{}, trustMissing: map[string]time.Time{}}
	if b.cfgPath == "" {
		b.cfgPath = porter.ConfigPath()
	}
	if b.push == nil {
		b.push = porter.Expo{HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
	return b
}

func (b *porterBridge) config() porter.Config {
	c, e := porter.LoadConfig(b.cfgPath)
	if e != nil {
		// Fail closed: a broken config shares nothing and trusts no approver.
		log.Printf("porter config: %v", e)
		return porter.Config{Approvals: porter.ApprovalsTerminal, AskTimeout: 2 * time.Minute, IdleThreshold: 2 * time.Minute, Share: map[string][]string{}}
	}
	return c
}

func (n *Node) runPorter() {
	b := n.porter
	defer b.pushes.Wait()
	var e error
	if b.subs, e = b.file.Load(); e != nil {
		log.Printf("porter: %v", e)
		return
	}
	// Changes made while the node was down are not pushed; the snapshot sent
	// on attach carries them instead.
	b.seen, _ = os.Stat(b.state.Path)
	if s, e := b.state.Load(); e == nil {
		b.last = s.Agents
		b.running = s.Running()
	} else {
		log.Printf("porter: %v", e)
		b.last = map[string]*porter.Agent{}
	}
	b.trustSeen, _ = os.Stat(b.trustsPath)
	b.lastTrusts = b.loadTrusts().Trusts
	first := true
	for n.ctx.Err() == nil {
		if e = b.attach(first); e != nil {
			log.Printf("porter attach: %s", errorCode(e))
		}
		first = false
		select {
		case <-n.ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// attach owns the porter receiver until it is dropped or the node stops.
func (b *porterBridge) attach(first bool) error {
	n := b.n
	v, e := n.Store.OpenAgent(OpenRequest{Alias: PorterAlias, Persistent: true, Scope: "global", Harness: "service", HarnessID: "porter"})
	if e != nil {
		return e
	}
	b.mu.Lock()
	b.session = v.Session
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(n.ctx)
	defer cancel()
	r := &receiver{v.Session, make(chan Event, 1), ctx, cancel}
	n.mu.Lock()
	if n.closing.Load() {
		n.mu.Unlock()
		return nil
	}
	if old := n.receivers[v.Agent.ID]; old != nil {
		old.cancel()
	}
	n.receivers[v.Agent.ID] = r
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		same := n.receivers[v.Agent.ID] == r
		if same {
			delete(n.receivers, v.Agent.ID)
		}
		n.mu.Unlock()
		if same {
			n.Store.Disconnect(v.Session.ID)
		}
		n.notify()
		n.requestSync()
		n.publish("presence")
	}()
	n.notify()
	n.requestSync()
	n.publish("presence")
	if first {
		b.snapshotAll()
	}
	b.lastSubscribe = time.Time{}
	poll := min(n.cfg.Heartbeat, 500*time.Millisecond)
	pollTick, leaseTick, slowTick := time.NewTicker(poll), time.NewTicker(n.cfg.Heartbeat), time.NewTicker(min(time.Second, 4*poll))
	defer pollTick.Stop()
	defer leaseTick.Stop()
	defer slowTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-leaseTick.C:
			if e = n.Store.Renew(v.Session.ID); e != nil {
				return e
			}
		case <-pollTick.C:
			b.poll()
			b.pollTrusts()
		case <-slowTick.C:
			b.maintain()
		case ev := <-r.events:
			if ev.Message == nil {
				continue
			}
			m := *ev.Message
			b.handle(m)
			// Control messages are applied before acknowledgment; a malformed or
			// refused one is still consumed, never retried.
			h := Handoff{SenderMachine: m.SenderMachine, MessageID: m.ID, AttemptID: m.AttemptID, Status: "handed_off"}
			if e := n.recordHandoff(v.Session.ID, h); e != nil {
				log.Printf("porter handoff: %s", errorCode(e))
			}
		}
	}
}

func (b *porterBridge) granted(peer string) bool {
	g, e := b.n.Store.Grant(peer)
	return e == nil && g.Messages
}

func (b *porterBridge) handle(m Message) {
	if m.SenderMachine == b.n.Store.Identity.MachineID || !b.granted(m.SenderMachine) {
		log.Printf("porter: ignored message from ungranted or local sender")
		return
	}
	c, e := porter.Parse(m.Body)
	if e != nil {
		log.Printf("porter: %v %q from %s:%s", e, m.Body[:min(len(m.Body), 80)], m.SenderMachine, m.SenderAgent)
		return
	}
	peer := m.SenderMachine
	switch c.Type {
	case porter.TypeSubscribe, porter.TypeUnsubscribe:
		// Publish pending file changes first (to the existing subscribers), so
		// the new snapshot matches what later updates are diffed against and
		// is not followed by a stale update of the same state.
		b.poll()
		b.pollTrusts()
		b.subscribe(peer, m.SenderAgent, c)
	case porter.TypeSet:
		if !b.config().IsApprover(peer) {
			log.Printf("porter: ignored porter.set from non-approver")
			return
		}
		e = b.state.Update(func(s *porter.State) error {
			if a := s.Agents[c.Agent]; a != nil {
				s.SetSpecial(a, *c.Special)
			}
			return nil
		})
	case porter.TypeRevoke:
		if !b.config().IsApprover(peer) {
			log.Printf("porter: ignored porter.revoke from non-approver")
			return
		}
		e = porter.UpdateJSON(b.trustsPath, func(t *porter.Trusts) (bool, error) {
			_, ok := t.Trusts[c.Trust]
			delete(t.Trusts, c.Trust)
			return ok, nil
		})
	case porter.TypeAnswer:
		b.answer(peer, c.ID, c.Choice, c.Text)
	case porter.TypeResign:
		b.resign(peer, m.SenderAgent)
	case porter.TypeSubscribed:
		b.mu.Lock()
		b.peerOK[peer] = true
		b.mu.Unlock()
	case porter.TypeSnapshot, porter.TypeUpdate, porter.TypeRemove:
		e = b.cache.Apply(peer, c)
	}
	if e != nil {
		log.Printf("porter %s: %v", c.Type, e)
	}
}

func (b *porterBridge) subscribe(peer, agent string, c porter.Inbound) {
	cfg := b.config()
	b.mu.Lock()
	var sub porter.Subscriber
	if c.Type == porter.TypeSubscribe {
		sub = porter.Subscriber{Peer: peer, Agent: agent, Topics: c.Topics, Push: c.Push, Since: time.Now().UTC()}
		b.subs[peer] = sub
	} else {
		delete(b.subs, peer)
	}
	e := b.file.Save(b.subs)
	b.mu.Unlock()
	if e != nil {
		log.Printf("porter subscribers: %v", e)
	}
	if c.Type != porter.TypeSubscribe {
		return
	}
	reply := b.subscribed(cfg, peer, c.Topics, sub.Push != nil)
	b.send(sub.Peer+":"+sub.Agent, reply)
	for _, t := range reply.Topics {
		b.snapshot(sub, t)
	}
}

// subscribed is the porter.subscribed reply for peer asking for topics.
func (b *porterBridge) subscribed(cfg porter.Config, peer string, topics []string, push bool) porter.Subscribed {
	reply := porter.Subscribed{Type: porter.TypeSubscribed, V: porter.WireVersion, Machine: b.machine(), Topics: []string{}, Refused: []string{},
		Approver: cfg.IsApprover(peer), Push: push, Version: Version}
	for _, t := range topics {
		if slices.Contains(porter.Topics, t) && cfg.Shares(t, peer) {
			reply.Topics = append(reply.Topics, t)
		} else {
			reply.Refused = append(reply.Refused, t)
		}
	}
	return reply
}

// resign removes an approver at its own request. Only a current approver may
// resign, only itself; no message can ever add an approver. Its pending
// questions are cancelled, and it gets a fresh porter.subscribed.
func (b *porterBridge) resign(peer, agent string) {
	if !b.config().IsApprover(peer) {
		log.Printf("porter: ignored porter.resign from non-approver")
		return
	}
	removed, e := porter.RemoveApprover(b.cfgPath, peer)
	if e != nil {
		log.Printf("porter resign: %v", e)
		return
	}
	if !removed {
		return
	}
	porter.AppendAudit(filepath.Join(b.dir, "audit.jsonl"), porter.AuditRecord{Kind: "resign", By: peer, Result: "resigned"})
	b.cancelQuestionsTo(peer)
	b.mu.Lock()
	sub, ok := b.subs[peer]
	b.mu.Unlock()
	topics := []string{}
	if ok {
		topics = sub.Topics
	}
	b.send(peer+":"+agent, b.subscribed(b.config(), peer, topics, ok && sub.Push != nil))
}

// current returns subscribers that still hold a messaging grant, forgetting
// any whose grant was revoked.
func (b *porterBridge) current() []porter.Subscriber {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []porter.Subscriber{}
	dropped := false
	for id, s := range b.subs {
		if !b.granted(id) {
			delete(b.subs, id)
			dropped = true
			continue
		}
		out = append(out, s)
	}
	if dropped {
		if e := b.file.Save(b.subs); e != nil {
			log.Printf("porter subscribers: %v", e)
		}
	}
	return out
}

// receiving returns current subscribers that asked for topic and may get it.
func (b *porterBridge) receiving(topic string) []porter.Subscriber {
	cfg := b.config()
	out := []porter.Subscriber{}
	for _, s := range b.current() {
		if s.Wants(topic) && cfg.Shares(topic, s.Peer) {
			out = append(out, s)
		}
	}
	return out
}

func (b *porterBridge) machine() string { return b.n.Store.Setting("name") }

func (b *porterBridge) porterSession() Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

func (b *porterBridge) send(to string, v any) error {
	s := b.porterSession()
	if s.ID == "" {
		return problem(503, "porter_not_ready", "porter is not attached yet")
	}
	_, e := b.n.Send(b.n.ctx, SendRequest{SessionID: s.ID, To: to, Body: string(marshal(v))})
	if e != nil {
		log.Printf("porter send: %s", errorCode(e))
	}
	return e
}

func (b *porterBridge) snapshot(s porter.Subscriber, topic string) {
	to := s.Peer + ":" + s.Agent
	switch topic {
	case porter.TopicAgents:
		st, e := b.state.Load()
		if e != nil {
			log.Printf("porter: %v", e)
			return
		}
		b.send(to, porter.NewSnapshot(b.machine(), topic, porter.AgentItems(st, b.config().Recap), MaxBody-1024))
	case porter.TopicStatus:
		b.send(to, porter.NewValueSnapshot(b.machine(), topic, b.status()))
	case porter.TopicTrusts:
		items := []any{}
		for _, t := range b.loadTrusts().List() {
			items = append(items, t.Item())
		}
		b.send(to, porter.NewSnapshot(b.machine(), topic, items, MaxBody-1024))
	}
}

func (b *porterBridge) snapshotAll() {
	cfg := b.config()
	for _, s := range b.current() {
		for _, t := range porter.Topics {
			if s.Wants(t) && cfg.Shares(t, s.Peer) {
				b.snapshot(s, t)
			}
		}
	}
}

func (b *porterBridge) status() porter.Status {
	running := 0
	if st, e := b.state.Load(); e == nil {
		running = st.Running()
	}
	return porter.CollectStatus(b.machine(), Version, b.n.cfg.DataDir, running)
}

func (b *porterBridge) publishStatus() {
	b.lastStatus = time.Now()
	subs := b.receiving(porter.TopicStatus)
	if len(subs) == 0 {
		return
	}
	u := porter.NewValueUpdate(b.machine(), porter.TopicStatus, b.status())
	for _, s := range subs {
		b.send(s.Peer+":"+s.Agent, u)
	}
}

// changed reports whether path was replaced since seen. Writers always
// rename a new file into place.
func changed(path string, seen os.FileInfo) (os.FileInfo, bool) {
	info, e := os.Stat(path)
	if e != nil {
		return seen, seen != nil
	}
	if seen != nil && os.SameFile(seen, info) && seen.ModTime().Equal(info.ModTime()) && seen.Size() == info.Size() {
		return seen, false
	}
	return info, true
}

// poll publishes agent changes when state.json was replaced since the last look.
func (b *porterBridge) poll() {
	info, ok := changed(b.state.Path, b.seen)
	if !ok {
		return
	}
	st, e := b.state.Load()
	if e != nil {
		log.Printf("porter: %v", e)
		return
	}
	b.seen = info
	prev := b.last
	b.last = st.Agents
	changedAgents, removed := porter.Diff(prev, st.Agents)
	machine := b.machine()
	recap := b.config().Recap
	for _, s := range b.receiving(porter.TopicAgents) {
		for _, a := range changedAgents {
			b.send(s.Peer+":"+s.Agent, porter.NewUpdate(machine, porter.TopicAgents, a.Published(recap)))
			// A permission question on its way to the phone carries the push.
			if s.Push != nil && porter.PushWanted(prev[a.ID], a) && !st.PhoneAsked(a) {
				b.pushTo(s, porter.NoticeFor(machine, a.View()), nil)
			}
		}
		for _, id := range removed {
			b.send(s.Peer+":"+s.Agent, porter.NewRemove(machine, porter.TopicAgents, id))
		}
	}
	if r := st.Running(); r != b.running {
		b.running = r
		b.publishStatus()
	}
}

func (b *porterBridge) loadTrusts() *porter.Trusts {
	t, e := porter.LoadJSON[porter.Trusts](b.trustsPath)
	if e != nil {
		log.Printf("porter trusts: %v", e)
		return &porter.Trusts{}
	}
	if t.Trusts == nil {
		t.Trusts = map[string]porter.Trust{}
	}
	return t
}

// label is the trust label for a peer message delivered to a local agent.
func (b *porterBridge) label(receiverID, senderID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if info, ok := changed(b.trustsPath, b.trustInfo); ok || b.trusts == nil {
		b.trusts, b.trustInfo = b.loadTrusts(), info
	}
	return b.trusts.Label(receiverID, senderID)
}

func (b *porterBridge) pollTrusts() {
	info, ok := changed(b.trustsPath, b.trustSeen)
	if !ok {
		return
	}
	b.trustSeen = info
	next := b.loadTrusts().Trusts
	prev := b.lastTrusts
	b.lastTrusts = next
	machine := b.machine()
	for _, s := range b.receiving(porter.TopicTrusts) {
		for id, t := range next {
			if p, ok := prev[id]; !ok || p != t {
				b.send(s.Peer+":"+s.Agent, porter.NewUpdate(machine, porter.TopicTrusts, t.Item()))
			}
		}
		for id := range prev {
			if _, ok := next[id]; !ok {
				b.send(s.Peer+":"+s.Agent, porter.NewRemove(machine, porter.TopicTrusts, id))
			}
		}
	}
}

// maintain runs the slow periodic duties.
func (b *porterBridge) maintain() {
	now := time.Now()
	b.linkIdentities()
	if now.Sub(b.lastSweep) >= time.Minute {
		b.lastSweep = now
		if e := b.state.Update(func(s *porter.State) error { s.Sweep(now); return nil }); e != nil {
			log.Printf("porter sweep: %v", e)
		}
	}
	interval := b.n.cfg.PorterStatusInterval
	if interval <= 0 {
		interval = time.Minute
	}
	if now.Sub(b.lastStatus) >= interval {
		b.publishStatus()
	}
	b.expireQuestions(now)
	b.subscribePeers(now)
	if now.Sub(b.lastPrune) >= time.Minute {
		b.lastPrune = now
		b.pruneSessionTrusts(now)
	}
}

// linkIdentities attaches each live agent's comms identity, found through
// comms' own agent_sessions.harness_session_id (read-only).
func (b *porterBridge) linkIdentities() {
	st, e := b.state.Load()
	if e != nil {
		return
	}
	found := map[string]porter.Identity{}
	for id, a := range st.Agents {
		if a.Status == porter.StatusEnded || a.Parent != "" {
			continue
		}
		if v, ok := b.n.Store.harnessIdentity(id); ok {
			v.Address = b.machine() + ":" + v.Alias
			if a.Identity == nil || *a.Identity != v {
				found[id] = v
			}
		}
	}
	if len(found) == 0 {
		return
	}
	e = b.state.Update(func(s *porter.State) error {
		for id, v := range found {
			if a := s.Agents[id]; a != nil {
				s.SetIdentity(a, v)
			}
		}
		return nil
	})
	if e != nil {
		log.Printf("porter identities: %v", e)
	}
}

func (s *Store) harnessIdentity(harnessID string) (porter.Identity, bool) {
	var v porter.Identity
	e := s.ReadDB.QueryRow(`SELECT a.alias,a.persistent FROM agent_sessions s JOIN agents a ON a.id=s.agent_id WHERE s.harness_session_id=? AND s.ended_at IS NULL AND s.harness IN('claude','codex') AND a.retired_at IS NULL LIMIT 1`, harnessID).Scan(&v.Alias, &v.Persistent)
	return v, e == nil
}

// subscribePeers subscribes to every granted peer's porter until it replies.
// One directory lookup per round finds which peers run porter at all.
func (b *porterBridge) subscribePeers(now time.Time) {
	every := b.n.cfg.PorterResubscribe
	if every <= 0 {
		every = 10 * time.Minute
	}
	if now.Sub(b.lastSubscribe) < every || b.n.currentBrokerURL() == "" {
		return
	}
	b.lastSubscribe = now
	grants, e := b.n.Store.Grants()
	if e != nil {
		return
	}
	due := map[string]bool{}
	b.mu.Lock()
	for _, g := range grants {
		if g.Messages && !b.peerOK[g.Grantee] {
			due[g.Grantee] = true
		}
	}
	b.mu.Unlock()
	if len(due) == 0 {
		return
	}
	var who []Presence
	if e := b.n.brokerRequest(b.n.ctx, "GET", "/v1/who", nil, &who); e != nil {
		return
	}
	for _, p := range who {
		if due[p.MachineID] && p.Alias == PorterAlias {
			b.send(p.MachineID+":"+p.AgentID, map[string]any{"type": porter.TypeSubscribe, "v": porter.WireVersion, "topics": porter.Topics})
		}
	}
}

// pruneSessionTrusts drops session trusts whose sender identity has been
// absent from the broker's directory for the grace period: an ephemeral
// identity never returns once it ends.
func (b *porterBridge) pruneSessionTrusts(now time.Time) {
	t := b.loadTrusts()
	hasSession := false
	for _, v := range t.Trusts {
		hasSession = hasSession || v.Scope == porter.TrustSession
	}
	if !hasSession || b.n.currentBrokerURL() == "" {
		return
	}
	var who []Presence
	if e := b.n.brokerRequest(b.n.ctx, "GET", "/v1/who", nil, &who); e != nil {
		return
	}
	present := map[string]bool{}
	for _, p := range who {
		present[p.MachineID+":"+p.AgentID] = true
	}
	grace := b.n.cfg.PorterTrustGrace
	if grace <= 0 {
		grace = 10 * time.Minute
	}
	dead := map[string]bool{}
	for id, v := range t.Trusts {
		if v.Scope != porter.TrustSession || present[v.SenderID] {
			delete(b.trustMissing, id)
			continue
		}
		if since, ok := b.trustMissing[id]; !ok {
			b.trustMissing[id] = now
		} else if now.Sub(since) >= grace {
			dead[id] = true
			delete(b.trustMissing, id)
		}
	}
	if len(dead) == 0 {
		return
	}
	e := porter.UpdateJSON(b.trustsPath, func(t *porter.Trusts) (bool, error) {
		for id := range dead {
			delete(t.Trusts, id)
		}
		return true, nil
	})
	if e != nil {
		log.Printf("porter trusts: %v", e)
	}
}

// pushTo sends one encrypted push without blocking message handling. A dead
// token is forgotten; the subscription itself stays. When n is set it is a
// notification push; otherwise notice describes an agent.
func (b *porterBridge) pushTo(s porter.Subscriber, notice any, n *porter.Notify) {
	if s.Push == nil {
		return
	}
	peer, e := b.n.Store.Peer(s.Peer)
	if e != nil {
		return
	}
	id := b.n.Store.Identity
	var data map[string]string
	if n != nil {
		data, e = porter.NotifyPushData(*n, id.MachineID, peer.PublicKey, id.PrivateKey)
	} else {
		data, e = porter.PushData(notice, id.MachineID, peer.PublicKey, id.PrivateKey)
	}
	if e != nil {
		log.Printf("porter push: %v", e)
		return
	}
	token := s.Push.Token
	b.pushes.Add(1)
	go func() {
		defer b.pushes.Done()
		ctx, cancel := context.WithTimeout(b.n.ctx, 10*time.Second)
		defer cancel()
		e := b.push.Push(ctx, token, data)
		if errors.Is(e, porter.ErrDeviceNotRegistered) {
			b.mu.Lock()
			if cur, ok := b.subs[s.Peer]; ok && cur.Push != nil && cur.Push.Token == token {
				cur.Push = nil
				b.subs[s.Peer] = cur
				if e := b.file.Save(b.subs); e != nil {
					log.Printf("porter subscribers: %v", e)
				}
			}
			b.mu.Unlock()
		} else if e != nil {
			log.Printf("porter push: %v", e)
		}
	}()
}
