package porter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

// Wire message types. Every porter message is a JSON object with a "type"
// field, carried as the body of an ordinary comms message. See docs/PORTER.md.
const (
	TypeSubscribe   = "porter.subscribe"
	TypeUnsubscribe = "porter.unsubscribe"
	TypeSnapshot    = "porter.snapshot"
	TypeAgent       = "porter.agent"
	TypeRemove      = "porter.remove"
)

// Push is a subscriber's push registration. Only Expo is supported.
type Push struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// Control is an inbound subscription request from a peer.
type Control struct {
	Type string `json:"type"`
	Push *Push  `json:"push,omitempty"`
}

var expoToken = regexp.MustCompile(`^Expo(nent)?PushToken\[[A-Za-z0-9_-]{1,200}\]$`)

// ParseControl validates an inbound porter message body. Unknown fields are
// ignored so newer clients can add optional data.
func ParseControl(body string) (Control, error) {
	var c Control
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return c, fmt.Errorf("porter control is not JSON")
	}
	switch c.Type {
	case TypeSubscribe:
		if c.Push != nil && (c.Push.Provider != "expo" || !expoToken.MatchString(c.Push.Token)) {
			return c, fmt.Errorf("unsupported push registration")
		}
	case TypeUnsubscribe:
		c.Push = nil
	default:
		return c, fmt.Errorf("unknown porter control %q", c.Type)
	}
	return c, nil
}

type Snapshot struct {
	Type      string  `json:"type"`
	Machine   string  `json:"machine"`
	Agents    []Agent `json:"agents"`
	Truncated bool    `json:"truncated,omitempty"`
}

type AgentUpdate struct {
	Type    string `json:"type"`
	Machine string `json:"machine"`
	Agent   Agent  `json:"agent"`
}

type Remove struct {
	Type    string `json:"type"`
	Machine string `json:"machine"`
	ID      string `json:"id"`
}

// NewSnapshot lists all agents, dropping the least relevant ones when the
// encoded message would exceed limit bytes.
func NewSnapshot(machine string, s *State, limit int) Snapshot {
	snap := Snapshot{Type: TypeSnapshot, Machine: machine, Agents: s.List()}
	for len(snap.Agents) > 0 {
		b, _ := json.Marshal(snap)
		if len(b) <= limit {
			break
		}
		snap.Agents = snap.Agents[:len(snap.Agents)-1]
		snap.Truncated = true
	}
	return snap
}

// Diff reports agents that are new or changed in next, and ids that are gone.
func Diff(prev, next map[string]*Agent) (changed []Agent, removed []string) {
	for id, a := range next {
		if p, ok := prev[id]; !ok || !sameAgent(p, a) {
			changed = append(changed, *a)
		}
	}
	for id := range prev {
		if _, ok := next[id]; !ok {
			removed = append(removed, id)
		}
	}
	return changed, removed
}

func sameAgent(a, b *Agent) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// PushWanted reports whether moving from prev (nil when new) to next deserves
// a push: entering needs_you always, finishing a turn only for special agents.
// A freshly started session is not a stop.
func PushWanted(prev *Agent, next Agent) bool {
	if prev == nil && next.Status == StatusDone {
		return false
	}
	entered := prev == nil || prev.Status != next.Status || !prev.StatusSince.Equal(next.StatusSince)
	if !entered {
		return false
	}
	return next.Status == StatusNeedsYou || (next.Special && next.Status == StatusDone)
}

// Subscriber is a peer receiving porter updates. Peer is the peer's immutable
// machine id; Agent is the agent id that subscribed and receives updates.
type Subscriber struct {
	Peer  string    `json:"peer"`
	Agent string    `json:"agent"`
	Push  *Push     `json:"push,omitempty"`
	Since time.Time `json:"subscribed_at"`
}

// Subscribers persists subscriptions as a JSON file. Only the node writes it,
// and the node already holds an exclusive data-dir lock, so no file lock.
type Subscribers struct{ Path string }

type subscribersFile struct {
	Subscribers map[string]Subscriber `json:"subscribers"`
}

func (s Subscribers) Load() (map[string]Subscriber, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Subscriber{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f subscribersFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("read porter subscribers %s: %w", s.Path, err)
	}
	if f.Subscribers == nil {
		f.Subscribers = map[string]Subscriber{}
	}
	return f.Subscribers, nil
}

func (s Subscribers) Save(m map[string]Subscriber) error {
	return writeJSON(s.Path, subscribersFile{m})
}
