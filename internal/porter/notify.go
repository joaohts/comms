package porter

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Notification kinds.
const (
	NotifyMessage    = "message"
	NotifyPermission = "permission"
	NotifyTrust      = "trust"
)

// Choices of the fixed questions.
var (
	PermissionChoices = []string{"Allow", "Deny"}
	TrustChoices      = []string{"Once", "This session", "Always", "Deny"}
)

// Context is stamped by the sending node from the sender's comms session.
type Context struct {
	Machine string `json:"machine"`
	Agent   string `json:"agent,omitempty"`
	Harness string `json:"harness,omitempty"`
	Session string `json:"session,omitempty"`
	Title   string `json:"title,omitempty"`
	Project string `json:"project,omitempty"`
}

type TrustRef struct {
	Receiver string `json:"receiver"`
	Sender   string `json:"sender"`
}

type Notify struct {
	Type      string     `json:"type"`
	V         int        `json:"v"`
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Title     string     `json:"title"`
	Body      string     `json:"body,omitempty"`
	Reason    string     `json:"reason"`
	Priority  string     `json:"priority"`
	Ask       []string   `json:"ask,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Link      string     `json:"link,omitempty"`
	Context   Context    `json:"context"`
	Trust     *TrustRef  `json:"trust,omitempty"`
	Draft     string     `json:"draft,omitempty"`
	SentAt    time.Time  `json:"sent_at"`
}

// MaxDraft bounds a notify draft and the (possibly edited) answer text.
const MaxDraft = 4000

type Answer struct {
	Type   string `json:"type"`
	V      int    `json:"v"`
	ID     string `json:"id"`
	Choice string `json:"choice"`
	Text   string `json:"text,omitempty"`
}

type Cancel struct {
	Type       string `json:"type"`
	V          int    `json:"v"`
	ID         string `json:"id"`
	Resolution string `json:"resolution"`
}

// Cancel resolutions.
const (
	ResolvedTerminal = "terminal"
	ResolvedTimeout  = "timeout"
	ResolvedAnswered = "answered"
	ResolvedResigned = "resigned" // the recipient resigned as an approver
)

func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}

// ValidateNotify checks user-supplied notification fields.
func ValidateNotify(n *Notify) error {
	n.Title = strings.TrimSpace(n.Title)
	n.Reason = strings.TrimSpace(n.Reason)
	if n.Title == "" {
		return fmt.Errorf("a title is required")
	}
	if n.Reason == "" {
		return fmt.Errorf("a reason is required: say why you are notifying")
	}
	if len([]rune(n.Title)) > 200 || len([]rune(n.Body)) > 4000 || len([]rune(n.Reason)) > 300 || len(n.Link) > 500 {
		return fmt.Errorf("title, body, reason or link too long")
	}
	if len([]rune(n.Draft)) > MaxDraft {
		return fmt.Errorf("draft too long")
	}
	if n.Draft != "" && len(n.Ask) == 0 {
		return fmt.Errorf("a draft needs answer choices (--ask)")
	}
	if n.Priority == "" {
		n.Priority = "normal"
	}
	if n.Priority != "normal" && n.Priority != "high" {
		return fmt.Errorf("priority must be normal or high")
	}
	if len(n.Ask) > 4 {
		return fmt.Errorf("at most 4 answer choices")
	}
	for i, a := range n.Ask {
		n.Ask[i] = strings.TrimSpace(a)
		if n.Ask[i] == "" || len([]rune(n.Ask[i])) > 40 {
			return fmt.Errorf("answer choices must be 1-40 characters")
		}
	}
	return nil
}

// Trust scopes.
const (
	TrustSession = "session"
	TrustAlways  = "always"
)

// Trust is one stored approval: receiver accepts orders from sender as if
// they came from João. ReceiverID is the local comms agent id (store only).
type Trust struct {
	ID         string    `json:"id"`
	Receiver   string    `json:"receiver"`
	ReceiverID string    `json:"receiver_id"`
	Sender     string    `json:"sender"`
	SenderID   string    `json:"sender_id"`
	Scope      string    `json:"scope"`
	ApprovedAt time.Time `json:"approved_at"`
}

// TrustItem is the TRUST wire shape.
type TrustItem struct {
	ID         string    `json:"id"`
	Receiver   string    `json:"receiver"`
	Sender     string    `json:"sender"`
	SenderID   string    `json:"sender_id"`
	Scope      string    `json:"scope"`
	ApprovedAt time.Time `json:"approved_at"`
}

func (t Trust) Item() TrustItem {
	return TrustItem{t.ID, t.Receiver, t.Sender, t.SenderID, t.Scope, t.ApprovedAt}
}

type Trusts struct {
	Trusts map[string]Trust `json:"trusts"`
}

// Label is the trust label for a message from sender to receiver.
func (t *Trusts) Label(receiverID, senderID string) string {
	best := ""
	for _, v := range t.Trusts {
		if v.ReceiverID == receiverID && v.SenderID == senderID {
			if v.Scope == TrustAlways {
				return "trusted: always"
			}
			best = "trusted: session"
		}
	}
	if best == "" {
		return "untrusted"
	}
	return best
}

// Put stores a trust, replacing any for the same pair.
func (t *Trusts) Put(v Trust) Trust {
	if t.Trusts == nil {
		t.Trusts = map[string]Trust{}
	}
	for id, old := range t.Trusts {
		if old.ReceiverID == v.ReceiverID && old.SenderID == v.SenderID {
			delete(t.Trusts, id)
		}
	}
	if v.ID == "" {
		v.ID = NewID("tr_")
	}
	t.Trusts[v.ID] = v
	return v
}

// List returns trusts, newest first.
func (t *Trusts) List() []Trust {
	out := make([]Trust, 0, len(t.Trusts))
	for _, v := range t.Trusts {
		out = append(out, v)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].ApprovedAt.After(out[j-1].ApprovedAt) || out[j].ApprovedAt.Equal(out[j-1].ApprovedAt) && out[j].ID < out[j-1].ID); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
