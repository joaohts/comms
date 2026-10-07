package comms

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/joaohts/comms/internal/porter"
)

// PorterAlias is the persistent global agent through which porter talks to
// peers. Peers address it as <machine>:porter.
const PorterAlias = "porter"

// The porter bridge is a node-owned receiver, like native Codex: the node is
// already always on, so it attaches the porter identity itself, takes
// subscriptions from granted peers and publishes state changes written to
// <data-dir>/porter/state.json by short-lived `comms porter event` processes.
type porterBridge struct {
	n       *Node
	state   porter.Store
	file    porter.Subscribers
	push    porter.Pusher
	session Session
	mu      sync.Mutex
	subs    map[string]porter.Subscriber
	last    map[string]*porter.Agent
	seen    os.FileInfo
	pushes  sync.WaitGroup
}

func (n *Node) runPorter() {
	dir := filepath.Join(n.cfg.DataDir, "porter")
	b := &porterBridge{n: n, state: porter.Store{Path: filepath.Join(dir, "state.json")}, file: porter.Subscribers{Path: filepath.Join(dir, "subscribers.json")}, push: n.cfg.PorterPush}
	if b.push == nil {
		b.push = porter.Expo{HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
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
	} else {
		log.Printf("porter: %v", e)
		b.last = map[string]*porter.Agent{}
	}
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
	b.session = v.Session
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
	poll := min(n.cfg.Heartbeat, 500*time.Millisecond)
	pollTick, leaseTick := time.NewTicker(poll), time.NewTicker(n.cfg.Heartbeat)
	defer pollTick.Stop()
	defer leaseTick.Stop()
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
		log.Printf("porter: ignored control from ungranted or local sender")
		return
	}
	c, e := porter.ParseControl(m.Body)
	if e != nil {
		log.Printf("porter: %v", e)
		return
	}
	b.mu.Lock()
	var sub porter.Subscriber
	if c.Type == porter.TypeSubscribe {
		sub = porter.Subscriber{Peer: m.SenderMachine, Agent: m.SenderAgent, Push: c.Push, Since: time.Now().UTC()}
		b.subs[m.SenderMachine] = sub
	} else {
		delete(b.subs, m.SenderMachine)
	}
	e = b.file.Save(b.subs)
	b.mu.Unlock()
	if e != nil {
		log.Printf("porter subscribers: %v", e)
	}
	if c.Type == porter.TypeSubscribe {
		b.snapshot(sub)
	}
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

func (b *porterBridge) machine() string { return b.n.Store.Setting("name") }

func (b *porterBridge) send(s porter.Subscriber, v any) {
	_, e := b.n.Send(b.n.ctx, SendRequest{SessionID: b.session.ID, To: s.Peer + ":" + s.Agent, Body: string(marshal(v))})
	if e != nil {
		log.Printf("porter send: %s", errorCode(e))
	}
}

func (b *porterBridge) snapshot(s porter.Subscriber) {
	st, e := b.state.Load()
	if e != nil {
		log.Printf("porter: %v", e)
		return
	}
	b.send(s, porter.NewSnapshot(b.machine(), st, MaxBody-1024))
}

func (b *porterBridge) snapshotAll() {
	for _, s := range b.current() {
		b.snapshot(s)
	}
}

// poll publishes changes when state.json was replaced since the last look.
// Writers always rename a new file into place, so a new inode means a change.
func (b *porterBridge) poll() {
	info, e := os.Stat(b.state.Path)
	if e != nil {
		return
	}
	if b.seen != nil && os.SameFile(b.seen, info) && b.seen.ModTime().Equal(info.ModTime()) && b.seen.Size() == info.Size() {
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
	changed, removed := porter.Diff(prev, st.Agents)
	if len(changed) == 0 && len(removed) == 0 {
		return
	}
	machine := b.machine()
	for _, s := range b.current() {
		for _, a := range changed {
			b.send(s, porter.AgentUpdate{Type: porter.TypeAgent, Machine: machine, Agent: a})
			if s.Push != nil && porter.PushWanted(prev[a.ID], a) {
				b.notify(s, porter.NoticeFor(machine, a))
			}
		}
		for _, id := range removed {
			b.send(s, porter.Remove{Type: porter.TypeRemove, Machine: machine, ID: id})
		}
	}
}

// notify sends one encrypted push without blocking message handling. A dead
// token is forgotten; the subscription itself stays.
func (b *porterBridge) notify(s porter.Subscriber, notice porter.Notice) {
	peer, e := b.n.Store.Peer(s.Peer)
	if e != nil {
		return
	}
	data, e := porter.PushData(notice, b.n.Store.Identity.MachineID, peer.PublicKey, b.n.Store.Identity.PrivateKey)
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
