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
	TypeSubscribed  = "porter.subscribed"
	TypeSnapshot    = "porter.snapshot"
	TypeUpdate      = "porter.update"
	TypeRemove      = "porter.remove"
	TypeSet         = "porter.set"
	TypeRevoke      = "porter.revoke"
	TypeNotify      = "porter.notify"
	TypeAnswer      = "porter.answer"
	TypeCancel      = "porter.cancel"
	TypeResign      = "porter.resign"
)

// WireVersion is the "v" of every porter message.
const WireVersion = 1

// Push is a subscriber's push registration. Only Expo is supported.
type Push struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
}

// Inbound is any porter message received by a porter node. Fields not used
// by its type are ignored.
type Inbound struct {
	Type    string            `json:"type"`
	Topics  []string          `json:"topics,omitempty"`
	Push    *Push             `json:"-"`
	RawPush json.RawMessage   `json:"push,omitempty"`
	Agent   string            `json:"agent,omitempty"`
	Special *bool             `json:"special,omitempty"`
	Trust   string            `json:"trust,omitempty"`
	ID      string            `json:"id,omitempty"`
	Choice  string            `json:"choice,omitempty"`
	Text    string            `json:"text,omitempty"`
	Machine string            `json:"machine,omitempty"`
	Topic   string            `json:"topic,omitempty"`
	Items   []json.RawMessage `json:"items,omitempty"`
	Item    json.RawMessage   `json:"item,omitempty"`
	Value   json.RawMessage   `json:"value,omitempty"`
	Version string            `json:"porter_version,omitempty"`
	Refused []string          `json:"refused,omitempty"`
}

var expoToken = regexp.MustCompile(`^Expo(nent)?PushToken\[[A-Za-z0-9_-]{1,200}\]$`)

// ErrNotPorter marks a body that is not a porter message at all.
var ErrNotPorter = errors.New("not a porter message")

// Parse validates an inbound porter message body. Unknown fields are ignored;
// unknown porter types parse successfully and are ignored by the caller.
func Parse(body string) (Inbound, error) {
	var c Inbound
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return c, ErrNotPorter
	}
	switch c.Type {
	case TypeSubscribe:
		if len(c.RawPush) > 0 && string(c.RawPush) != "null" {
			if err := json.Unmarshal(c.RawPush, &c.Push); err != nil {
				return c, fmt.Errorf("unsupported push registration")
			}
		}
		if c.Push != nil && (c.Push.Provider != "expo" || !expoToken.MatchString(c.Push.Token)) {
			return c, fmt.Errorf("unsupported push registration")
		}
		if c.Topics == nil {
			c.Topics = []string{TopicAgents, TopicStatus}
		}
	case TypeUnsubscribe:
		c.Push = nil
	case TypeSet:
		if c.Agent == "" || c.Special == nil {
			return c, fmt.Errorf("porter.set requires agent and special")
		}
	case TypeRevoke:
		if c.Trust == "" {
			return c, fmt.Errorf("porter.revoke requires trust")
		}
	case TypeAnswer:
		if c.ID == "" || c.Choice == "" {
			return c, fmt.Errorf("porter.answer requires id and choice")
		}
		if len([]rune(c.Text)) > MaxDraft {
			return c, fmt.Errorf("porter.answer text too long")
		}
	case "":
		return c, ErrNotPorter
	}
	return c, nil
}

// Subscribed is the reply to every subscribe.
type Subscribed struct {
	Type     string   `json:"type"`
	V        int      `json:"v"`
	Machine  string   `json:"machine"`
	Topics   []string `json:"topics"`
	Refused  []string `json:"refused"`
	Approver bool     `json:"approver"`
	Push     bool     `json:"push"`
	Version  string   `json:"porter_version"`
}

// Snapshot is a full topic value: Items for collections, Value otherwise.
type Snapshot struct {
	Type      string `json:"type"`
	V         int    `json:"v"`
	Machine   string `json:"machine"`
	Topic     string `json:"topic"`
	Items     []any  `json:"items,omitempty"`
	Value     any    `json:"value,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type Update struct {
	Type    string `json:"type"`
	V       int    `json:"v"`
	Machine string `json:"machine"`
	Topic   string `json:"topic"`
	Item    any    `json:"item,omitempty"`
	Value   any    `json:"value,omitempty"`
}

type Remove struct {
	Type    string `json:"type"`
	V       int    `json:"v"`
	Machine string `json:"machine"`
	Topic   string `json:"topic"`
	ID      string `json:"id"`
}

func NewUpdate(machine, topic string, item any) Update {
	return Update{Type: TypeUpdate, V: WireVersion, Machine: machine, Topic: topic, Item: item}
}

func NewValueUpdate(machine, topic string, value any) Update {
	return Update{Type: TypeUpdate, V: WireVersion, Machine: machine, Topic: topic, Value: value}
}

func NewRemove(machine, topic, id string) Remove {
	return Remove{Type: TypeRemove, V: WireVersion, Machine: machine, Topic: topic, ID: id}
}

// NewSnapshot builds a collection snapshot, dropping items from the end
// while the encoded message exceeds limit bytes.
func NewSnapshot(machine, topic string, items []any, limit int) Snapshot {
	snap := Snapshot{Type: TypeSnapshot, V: WireVersion, Machine: machine, Topic: topic, Items: items}
	if snap.Items == nil {
		snap.Items = []any{}
	}
	for len(snap.Items) > 0 {
		b, _ := json.Marshal(snap)
		if len(b) <= limit {
			break
		}
		snap.Items = snap.Items[:len(snap.Items)-1]
		snap.Truncated = true
	}
	return snap
}

func NewValueSnapshot(machine, topic string, value any) Snapshot {
	return Snapshot{Type: TypeSnapshot, V: WireVersion, Machine: machine, Topic: topic, Value: value}
}

// AgentItems converts the default agent order into snapshot items; recap
// only when the machine opted in.
func AgentItems(s *State, recap bool) []any {
	l := s.List()
	out := make([]any, len(l))
	for i, a := range l {
		out[i] = a.Published(recap)
	}
	return out
}

// Diff reports agents that are new or changed in next, and ids that are gone.
func Diff(prev, next map[string]*Agent) (changed []Agent, removed []string) {
	for id, a := range next {
		if p, ok := prev[id]; !ok || !sameJSON(p, a) {
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

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// PushWanted reports whether moving from prev (nil when new) to next deserves
// a push: entering needs_you always, finishing a turn (done or error) only
// for special agents.
// A freshly started session is not a stop.
func PushWanted(prev *Agent, next Agent) bool {
	if prev == nil && next.Status == StatusDone {
		return false
	}
	entered := prev == nil || prev.Status != next.Status || !prev.StatusSince.Equal(next.StatusSince)
	if !entered {
		return false
	}
	return next.Status == StatusNeedsYou || (next.Special && (next.Status == StatusDone || next.Status == StatusError))
}

// Subscriber is a peer receiving porter updates. Peer is the peer's immutable
// machine id; Agent is the agent id that subscribed and receives updates.
type Subscriber struct {
	Peer   string    `json:"peer"`
	Agent  string    `json:"agent"`
	Topics []string  `json:"topics,omitempty"`
	Push   *Push     `json:"push,omitempty"`
	Since  time.Time `json:"subscribed_at"`
}

// Wants reports whether the subscriber asked for topic.
func (s Subscriber) Wants(topic string) bool {
	for _, t := range s.Topics {
		if t == topic {
			return true
		}
	}
	return false
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
	for id, sub := range f.Subscribers {
		if sub.Topics == nil { // written by porter v1
			sub.Topics = []string{TopicAgents}
			f.Subscribers[id] = sub
		}
	}
	return f.Subscribers, nil
}

func (s Subscribers) Save(m map[string]Subscriber) error {
	return writeJSON(s.Path, subscribersFile{m})
}
