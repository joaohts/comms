// Package porter keeps a harness-agnostic view of the agents running on this
// machine. Integrations (hooks, scripts) report lifecycle events; porter maps
// them to a small status model and accumulates running and waiting time.
// It knows nothing about any particular harness or about transport.
package porter

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Event kinds reported by integrations.
const (
	KindStart  = "start"  // session began; nothing pending
	KindPrompt = "prompt" // work started or resumed
	KindNeeds  = "needs"  // blocked on the user (permission, question)
	KindStop   = "stop"   // turn finished
	KindError  = "error"  // turn ended on an API error
	KindEnd    = "end"    // session ended; agent moves to the graveyard
	KindUpdate = "update" // metadata only (title, summary, special, ...)
)

// Agent statuses.
const (
	StatusRunning  = "running"
	StatusAway     = "away" // running, but the transcript has been silent (set by the node)
	StatusNeedsYou = "needs_you"
	StatusDone     = "done"
	StatusError    = "error" // the turn ended on an API error
	StatusEnded    = "ended"
)

// Active reports whether status counts as running (running or away).
func Active(status string) bool { return status == StatusRunning || status == StatusAway }

// Graveyard reasons and retention.
const (
	EndClosed  = "closed"
	EndStale   = "stale"
	StaleAfter = 24 * time.Hour
	KeepEnded  = 7 * 24 * time.Hour
)

type Needs struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// ErrorInfo details an error status: Kind is the harness's error type
// (rate_limit, overloaded, server_error, ...).
type ErrorInfo struct {
	Kind string `json:"kind"`
}

// Identity links an agent to its comms identity on this machine.
type Identity struct {
	Alias      string `json:"alias"`
	Address    string `json:"address"`
	Persistent bool   `json:"persistent"`
}

type Event struct {
	Agent     string
	Harness   string
	Kind      string
	At        time.Time
	Title     string
	Project   string
	Summary   string
	Parent    string
	AgentType string
	Needs     *Needs
	Error     *ErrorInfo
	Special   *bool
	// Transcript is the harness transcript path, kept in local state only.
	Transcript string
	Recap      string    // stored only where config recap = true; the caller decides
	Decision   *Decision // last resolved permission question for this agent
}

// MaxRecap bounds an agent recap (3-5 short bullets).
const MaxRecap = 600

// Decision answers a permission question tied to an agent. AnsweredBy is
// "approver" (a phone or other approver; ApproverID tells which, so an app
// shows "phone" when it is its own id), "terminal" (the question was withdrawn
// and the prompt went to the terminal) or "timeout".
type Decision struct {
	Choice     string    `json:"choice,omitempty"`
	AnsweredBy string    `json:"answered_by"`
	Approver   string    `json:"approver,omitempty"`
	ApproverID string    `json:"approver_id,omitempty"`
	At         time.Time `json:"at"`
}

// Decision sources.
const (
	AnsweredByApprover = "approver"
	AnsweredByTerminal = "terminal"
	AnsweredByTimeout  = "timeout"
)

type Agent struct {
	ID           string     `json:"id"`
	Harness      string     `json:"harness"`
	Parent       string     `json:"parent,omitempty"`
	AgentType    string     `json:"agent_type,omitempty"`
	Title        string     `json:"title,omitempty"`
	Project      string     `json:"project,omitempty"`
	Identity     *Identity  `json:"identity,omitempty"`
	Status       string     `json:"status"`
	Needs        *Needs     `json:"needs,omitempty"`
	Error        *ErrorInfo `json:"error,omitempty"`
	Summary      string     `json:"summary,omitempty"`
	Recap        string     `json:"recap,omitempty"`
	LastDecision *Decision  `json:"last_decision,omitempty"`
	Special      bool       `json:"special"`
	StartedAt    time.Time  `json:"started_at"`
	LastActiveAt time.Time  `json:"last_active_at"`
	StatusSince  time.Time  `json:"status_since"`
	RunningMS    int64      `json:"running_ms"`
	WaitingMS    int64      `json:"waiting_ms"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EndReason    string     `json:"end_reason,omitempty"`
}

// View is the agent as published: the title falls back to the comms alias.
func (a Agent) View() Agent {
	if a.Title == "" && a.Identity != nil {
		a.Title = a.Identity.Alias
	}
	return a
}

// Published is the agent as sent to peers; recap only when the machine opted in.
func (a Agent) Published(recap bool) Agent {
	a = a.View()
	if !recap {
		a.Recap = ""
	}
	return a
}

type State struct {
	Agents map[string]*Agent `json:"agents"`
	// SpecialIdentities holds the special flag of persistent comms identities
	// (by alias) so it survives across their sessions.
	SpecialIdentities map[string]bool `json:"special_identities,omitempty"`
	// PhoneAsks marks needs_you transitions (agent id → status_since) for
	// which a permission question goes to the phone; that notify carries the
	// push, so the agent-status push for the same transition is skipped.
	PhoneAsks map[string]time.Time `json:"phone_asks,omitempty"`
	// Transcripts maps agent id → harness transcript path. Local only: it is
	// never part of an agent as published.
	Transcripts map[string]string `json:"transcripts,omitempty"`
}

// MarkPhoneAsk records that a's current needs_you transition is being asked
// on the phone.
func (s *State) MarkPhoneAsk(a *Agent) {
	if a == nil || a.Status != StatusNeedsYou {
		return
	}
	if s.PhoneAsks == nil {
		s.PhoneAsks = map[string]time.Time{}
	}
	s.PhoneAsks[a.ID] = a.StatusSince
}

// PhoneAsked reports whether a's current transition was marked by MarkPhoneAsk.
func (s *State) PhoneAsked(a Agent) bool {
	t, ok := s.PhoneAsks[a.ID]
	return ok && a.Status == StatusNeedsYou && t.Equal(a.StatusSince)
}

func NewState() *State { return &State{Agents: map[string]*Agent{}} }

func statusFor(kind string) (string, bool) {
	switch kind {
	case KindStart, KindStop:
		return StatusDone, true
	case KindPrompt:
		return StatusRunning, true
	case KindNeeds:
		return StatusNeedsYou, true
	case KindError:
		return StatusError, true
	case KindEnd:
		return StatusEnded, true
	}
	return "", false
}

// Apply folds one event into the state and returns the affected agent.
func (s *State) Apply(e Event) (*Agent, error) {
	e.Agent = strings.TrimSpace(e.Agent)
	if e.Agent == "" {
		return nil, fmt.Errorf("event requires an agent id")
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.At = e.At.UTC()
	if s.Agents == nil {
		s.Agents = map[string]*Agent{}
	}
	status, changes := statusFor(e.Kind)
	if !changes && e.Kind != KindUpdate {
		return nil, fmt.Errorf("unknown event kind %q", e.Kind)
	}
	a := s.Agents[e.Agent]
	if a == nil {
		if e.Kind == KindEnd {
			return nil, nil // nothing to bury
		}
		a = &Agent{ID: e.Agent, Status: StatusDone, StartedAt: e.At, StatusSince: e.At}
		s.Agents[e.Agent] = a
	}
	if e.Harness != "" {
		a.Harness = e.Harness
	}
	if e.Title != "" {
		a.Title = e.Title
	}
	if e.Project != "" {
		a.Project = e.Project
	}
	if e.Summary != "" {
		a.Summary = e.Summary
	}
	if e.Recap != "" {
		a.Recap = clip(strings.TrimSpace(e.Recap), MaxRecap)
	}
	if e.Decision != nil {
		d := *e.Decision
		a.LastDecision = &d
	}
	if e.Parent != "" {
		a.Parent = e.Parent
	}
	if e.AgentType != "" {
		a.AgentType = e.AgentType
	}
	if e.Special != nil {
		s.SetSpecial(a, *e.Special)
	}
	if e.Transcript != "" {
		if s.Transcripts == nil {
			s.Transcripts = map[string]string{}
		}
		s.Transcripts[a.ID] = e.Transcript
	}
	// An event older than the newest one seen only carries metadata, so a
	// late hook can never roll the status or its timers back.
	if e.At.Before(a.LastActiveAt) {
		changes = false
	} else {
		a.LastActiveAt = e.At
	}
	if changes {
		s.setStatus(a, e.At, status, EndClosed)
		if status == StatusNeedsYou {
			a.Needs = e.Needs
		}
		if status == StatusError {
			a.Error = e.Error
			if a.Error == nil {
				a.Error = &ErrorInfo{Kind: "unknown"}
			}
		}
		if status == StatusEnded {
			// Subagents cannot outlive their parent.
			for _, c := range s.Agents {
				if c.Parent == a.ID && c.Status != StatusEnded {
					s.setStatus(c, e.At, StatusEnded, EndClosed)
				}
			}
		}
	}
	return a, nil
}

func (s *State) setStatus(a *Agent, t time.Time, status, reason string) {
	delete(s.PhoneAsks, a.ID)
	a.accumulate(t, status)
	a.Status = status
	a.Needs, a.Error = nil, nil
	a.EndedAt, a.EndReason = nil, ""
	if status == StatusEnded {
		at := t
		a.EndedAt, a.EndReason = &at, reason
		delete(s.Transcripts, a.ID)
	}
}

// SetAway moves agent id between running and away at t: away when away is
// true and it is running, running when away is false and it is away. It
// reports whether the status changed. A resume counts as activity.
func (s *State) SetAway(id string, away bool, t time.Time) bool {
	a := s.Agents[id]
	if a == nil {
		return false
	}
	t = t.UTC()
	switch {
	case away && a.Status == StatusRunning:
		s.setStatus(a, t, StatusAway, "")
	case !away && a.Status == StatusAway:
		s.setStatus(a, t, StatusRunning, "")
		if t.After(a.LastActiveAt) {
			a.LastActiveAt = t
		}
	default:
		return false
	}
	return true
}

// SetSpecial sets the special flag. It sticks to a persistent comms identity,
// otherwise to this session only.
func (s *State) SetSpecial(a *Agent, v bool) {
	a.Special = v
	if a.Identity != nil && a.Identity.Persistent {
		if s.SpecialIdentities == nil {
			s.SpecialIdentities = map[string]bool{}
		}
		if v {
			s.SpecialIdentities[a.Identity.Alias] = true
		} else {
			delete(s.SpecialIdentities, a.Identity.Alias)
		}
	}
}

// SetIdentity links a to its comms identity, inheriting a sticky special flag.
// It reports whether anything changed.
func (s *State) SetIdentity(a *Agent, id Identity) bool {
	if a.Identity != nil && *a.Identity == id {
		return false
	}
	a.Identity = &id
	if id.Persistent && s.SpecialIdentities[id.Alias] {
		a.Special = true
	}
	return true
}

// Sweep buries agents silent for StaleAfter and forgets agents ended longer
// than KeepEnded ago. It returns the ids of forgotten agents.
func (s *State) Sweep(now time.Time) (changed bool, purged []string) {
	now = now.UTC()
	busy := map[string]bool{}
	for _, c := range s.Agents {
		if c.Parent != "" && (Active(c.Status) || c.Status == StatusNeedsYou) {
			busy[c.Parent] = true
		}
	}
	for id, a := range s.Agents {
		if a.Status == StatusEnded {
			if a.EndedAt == nil || now.Sub(*a.EndedAt) > KeepEnded {
				delete(s.Agents, id)
				delete(s.PhoneAsks, id)
				delete(s.Transcripts, id)
				purged = append(purged, id)
			}
			continue
		}
		// Background subagents are silent between start and stop, and a parent
		// stays alive while any of its subagents works.
		if a.Parent != "" && Active(a.Status) || busy[id] {
			continue
		}
		if now.Sub(a.LastActiveAt) > StaleAfter {
			s.setStatus(a, now, StatusEnded, EndStale)
			changed = true
		}
	}
	sort.Strings(purged)
	return changed || len(purged) > 0, purged
}

// accumulate closes the current status period at t and starts a new one when
// the status changes.
func (a *Agent) accumulate(t time.Time, next string) {
	if t.After(a.StatusSince) {
		d := t.Sub(a.StatusSince).Milliseconds()
		switch a.Status {
		case StatusRunning, StatusAway:
			a.RunningMS += d
		case StatusNeedsYou:
			a.WaitingMS += d
		}
		a.StatusSince = t
	}
	if next != a.Status {
		a.StatusSince = t
	}
}

// Running counts agents currently running (away included).
func (s *State) Running() int {
	n := 0
	for _, a := range s.Agents {
		if Active(a.Status) {
			n++
		}
	}
	return n
}

// List returns agents as shown by default: needs_you first, then most
// recently active, the graveyard last.
func (s *State) List() []Agent {
	out := make([]Agent, 0, len(s.Agents))
	for _, a := range s.Agents {
		out = append(out, a.View())
	}
	rank := func(a Agent) int {
		switch a.Status {
		case StatusNeedsYou:
			return 0
		case StatusEnded:
			return 2
		}
		return 1
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if !out[i].LastActiveAt.Equal(out[j].LastActiveAt) {
			return out[i].LastActiveAt.After(out[j].LastActiveAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}
