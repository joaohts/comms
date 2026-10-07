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
	KindEnd    = "end"    // session ended; agent is removed
	KindUpdate = "update" // metadata only (title, summary, special, ...)
)

// Agent statuses.
const (
	StatusRunning  = "running"
	StatusNeedsYou = "needs_you"
	StatusDone     = "done"
)

type Needs struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type Event struct {
	Agent   string
	Harness string
	Kind    string
	At      time.Time
	Title   string
	Project string
	Summary string
	Needs   *Needs
	Special *bool
}

type Agent struct {
	ID           string    `json:"id"`
	Harness      string    `json:"harness"`
	Title        string    `json:"title,omitempty"`
	Project      string    `json:"project,omitempty"`
	Status       string    `json:"status"`
	Needs        *Needs    `json:"needs,omitempty"`
	Summary      string    `json:"summary,omitempty"`
	Special      bool      `json:"special"`
	StartedAt    time.Time `json:"started_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	StatusSince  time.Time `json:"status_since"`
	RunningMS    int64     `json:"running_ms"`
	WaitingMS    int64     `json:"waiting_ms"`
}

type State struct {
	Agents map[string]*Agent `json:"agents"`
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
	}
	return "", false
}

// Apply folds one event into the state. It returns the affected agent, or nil
// when the agent was removed.
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
	if e.Kind == KindEnd {
		delete(s.Agents, e.Agent)
		return nil, nil
	}
	status, changes := statusFor(e.Kind)
	if !changes && e.Kind != KindUpdate {
		return nil, fmt.Errorf("unknown event kind %q", e.Kind)
	}
	a := s.Agents[e.Agent]
	if a == nil {
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
	if e.Special != nil {
		a.Special = *e.Special
	}
	// An event older than the newest one seen only carries metadata, so a
	// late hook can never roll the status or its timers back.
	if e.At.Before(a.LastActiveAt) {
		changes = false
	} else {
		a.LastActiveAt = e.At
	}
	if changes {
		a.accumulate(e.At, status)
		a.Status = status
		a.Needs = nil
		if status == StatusNeedsYou {
			a.Needs = e.Needs
		}
	}
	return a, nil
}

// accumulate closes the current status period at t and starts a new one when
// the status changes.
func (a *Agent) accumulate(t time.Time, next string) {
	if t.After(a.StatusSince) {
		d := t.Sub(a.StatusSince).Milliseconds()
		switch a.Status {
		case StatusRunning:
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

// List returns agents with needs_you first, then most recently active.
func (s *State) List() []Agent {
	out := make([]Agent, 0, len(s.Agents))
	for _, a := range s.Agents {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		ni, nj := out[i].Status == StatusNeedsYou, out[j].Status == StatusNeedsYou
		if ni != nj {
			return ni
		}
		return out[i].LastActiveAt.After(out[j].LastActiveAt)
	})
	return out
}
