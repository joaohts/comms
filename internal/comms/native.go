package comms

import (
	"context"
	"log"
	"time"

	"github.com/joaohts/comms/internal/codex"
)

// Native Codex is a receiver owned by the node, backed by an independently
// running Codex app-server. The app-server and model sessions survive node/GUI
// restarts. Only already loaded, exact threads can be attached or delivered to.
func (n *Node) startNative(s Session) {
	if s.Harness != "codex" || s.Target == "" || s.EndedAt != nil {
		return
	}
	n.mu.Lock()
	if n.closing.Load() {
		n.mu.Unlock()
		return
	}
	if n.nativeStarting == nil {
		n.nativeStarting = map[string]bool{}
	}
	if n.nativeStarting[s.AgentID] || n.receivers[s.AgentID] != nil {
		n.mu.Unlock()
		return
	}
	n.nativeStarting[s.AgentID] = true
	n.native.Add(1)
	n.mu.Unlock()
	go func() {
		defer n.native.Done()
		defer func() { n.mu.Lock(); delete(n.nativeStarting, s.AgentID); n.mu.Unlock() }()
		ctx, cancel := context.WithCancel(n.ctx)
		defer cancel()
		if e := codex.ValidateSession(ctx, s.Target, s.HarnessID); e != nil {
			return
		}
		current, e := n.Store.Session(s.ID)
		if e != nil || current.EndedAt != nil {
			return
		}
		r := &receiver{s, make(chan Event, 1), ctx, cancel}
		n.mu.Lock()
		if n.closing.Load() || n.receivers[s.AgentID] != nil {
			n.mu.Unlock()
			return
		}
		n.receivers[s.AgentID] = r
		n.mu.Unlock()
		defer func() {
			n.mu.Lock()
			same := n.receivers[s.AgentID] == r
			if same {
				delete(n.receivers, s.AgentID)
			}
			n.mu.Unlock()
			if same {
				n.Store.Disconnect(s.ID)
			}
			n.notify()
			if s.Scope == "global" {
				n.requestSync()
			}
			n.publish("presence")
		}()
		n.Store.Renew(s.ID)
		n.notify()
		if s.Scope == "global" {
			n.requestSync()
		}
		n.publish("presence")
		tick := time.NewTicker(n.cfg.Heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				current, e := n.Store.Session(s.ID)
				if e != nil || current.EndedAt != nil {
					return
				}
				if e = codex.ValidateSession(ctx, s.Target, s.HarnessID); e != nil {
					return
				}
				if e = n.Store.Renew(s.ID); e != nil {
					return
				}
			case ev := <-r.events:
				if ev.Message == nil {
					continue
				}
				m := *ev.Message
				payload := marshal(ContentForPeer(m))
				result, err := codex.Deliver(ctx, s.Target, s.HarnessID, payload)
				h := Handoff{SenderMachine: m.SenderMachine, MessageID: m.ID, AttemptID: m.AttemptID}
				switch result.Outcome {
				case codex.Accepted:
					h.Status = "handed_off"
				case codex.NotSent:
					h.Status = "retry"
					h.Failure = "adapter_error"
				default:
					h.Status = "uncertain"
					h.Failure = "adapter_error"
				}
				if err != nil {
					log.Printf("Codex handoff %s: %v", result.Outcome, err)
				}
				if e := n.recordHandoff(s.ID, h); e != nil {
					log.Printf("Codex handoff persistence: %v", e)
				}
			}
		}
	}()
}

func (n *Node) recordHandoff(session string, h Handoff) error {
	if !validReceiptFailure(h.Failure) {
		h.Failure = "adapter_error"
	}
	if n.Store.HandoffRecorded(session, h) {
		return nil
	}
	n.mu.Lock()
	wait := n.waits[h.AttemptID]
	valid := wait != nil && wait.session == session && wait.message.ID == h.MessageID && wait.message.SenderMachine == h.SenderMachine
	n.mu.Unlock()
	if !valid {
		// A positively confirmed late adapter result may discharge uncertainty;
		// it never replays the original body and cannot affect a newer attempt.
		if h.Status == "handed_off" {
			return n.Store.ResolveUncertain(h.SenderMachine, h.MessageID, "handed_off", session, h.AttemptID)
		}
		return problem(409, "stale_attempt", "delivery attempt is no longer current")
	}
	state := h.Status
	var next int64
	if state == "retry" {
		state = "received"
		next = retryAt(max(0, wait.message.Attempts-1))
	}
	if e := n.Store.Finish(wait.message, state, h.Failure, next); e != nil {
		if n.Store.HandoffRecorded(session, h) {
			return nil
		}
		return e
	}
	select {
	case wait.result <- h:
	default:
	}
	n.publish("messages")
	n.notify()
	return nil
}
