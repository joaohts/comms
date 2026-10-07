package comms

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/joaohts/comms/internal/porter"
)

var Version = "0.2.0"

const ProtocolVersion = 1
const MessageTTL = 7 * 24 * time.Hour
const MaxBody = 64 << 10
const MaxWire = 768 << 10
const MaxHistory = 1 << 20

type Config struct {
	DataDir              string
	BrokerListen         string
	LegacyURL            string
	Workers              int
	Heartbeat            time.Duration
	Lease                time.Duration
	Drain                time.Duration
	LocalCount           int
	LocalBytes           int64
	BrokerCount          int
	BrokerBytes          int64
	ReceiptCount         int
	ReceiptBytes         int64
	AllowInsecure        bool
	BrokerServiceKey     string `json:"-"`
	BrokerServiceKeyFile string `json:"-"`
	// Porter publishes local agent state to subscribed peers (opt-in).
	Porter     bool
	PorterPush porter.Pusher `json:"-"`
	// PorterConfig overrides the porter config path (default $PORTER_CONFIG
	// or ~/.config/porter/config.toml).
	PorterConfig         string        `json:"-"`
	PorterStatusInterval time.Duration `json:"-"`
	PorterResubscribe    time.Duration `json:"-"`
	PorterTrustGrace     time.Duration `json:"-"`
}

func DefaultConfig() Config {
	d := os.Getenv("COMMS_DATA_DIR")
	if d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".local", "share", "comms")
	}
	return Config{DataDir: d, BrokerServiceKeyFile: os.Getenv("COMMS_BROKER_SERVICE_KEY_FILE"), Workers: 4, Heartbeat: 15 * time.Second, Lease: 60 * time.Second,
		Drain: 10 * time.Second, LocalCount: 1000, LocalBytes: 32 << 20,
		BrokerCount: 10000, BrokerBytes: 256 << 20, ReceiptCount: 2048, ReceiptBytes: 8 << 20}
}

func Now() int64 { return time.Now().UTC().UnixMilli() }
func NewID(prefix string) string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return prefix + hex.EncodeToString(b[:])
}
func randomToken() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func marshal(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var agentIDPattern = regexp.MustCompile(`^a_[0-9a-f]{32}$`)
var wireIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func validAlias(s string) bool  { return aliasPattern.MatchString(s) && !agentIDPattern.MatchString(s) }
func validWireID(s string) bool { return wireIDPattern.MatchString(s) }

type APIError struct {
	Code      string `json:"code"`
	ErrorText string `json:"error"`
	Status    int    `json:"-"`
}

func (e *APIError) Error() string                 { return e.ErrorText }
func problem(status int, code, text string) error { return &APIError{code, text, status} }
func errorCode(e error) string {
	var a *APIError
	if errors.As(e, &a) {
		return a.Code
	}
	return "internal_error"
}

type Identity struct {
	MachineID  string `json:"machine_id"`
	PublicKey  []byte `json:"public_key"`
	PrivateKey []byte `json:"-"`
	CreatedAt  int64  `json:"created_at"`
}
type Peer struct {
	MachineID  string `json:"machine_id"`
	Alias      string `json:"alias"`
	PublicKey  []byte `json:"public_key"`
	VerifiedAt int64  `json:"verified_at"`
}
type Grant struct {
	Grantee  string `json:"grantee_machine_id"`
	Messages bool   `json:"allow_messages"`
	History  bool   `json:"allow_history"`
	Revision int64  `json:"revision"`
}
type Agent struct {
	ID         string `json:"id"`
	Alias      string `json:"alias"`
	Persistent bool   `json:"persistent"`
	CreatedAt  int64  `json:"created_at"`
	RetiredAt  *int64 `json:"retired_at,omitempty"`
	Scope      string `json:"scope"`
	Online     bool   `json:"online"`
}
type Session struct {
	ID             string `json:"id"`
	AgentID        string `json:"agent_id"`
	Harness        string `json:"harness"`
	HarnessID      string `json:"harness_session_id"`
	Scope          string `json:"scope"`
	Target         string `json:"delivery_target,omitempty"`
	ProcessID      int    `json:"process_id,omitempty"`
	ProcessStarted string `json:"process_started,omitempty"`
	StartedAt      int64  `json:"started_at"`
	LeaseExpiresAt int64  `json:"lease_expires_at"`
	EndedAt        *int64 `json:"ended_at,omitempty"`
}
type OpenRequest struct {
	Alias          string `json:"alias"`
	Persistent     bool   `json:"persistent"`
	Scope          string `json:"scope"`
	Harness        string `json:"harness"`
	HarnessID      string `json:"harness_session_id"`
	Target         string `json:"delivery_target"`
	ProcessID      int    `json:"process_id"`
	ProcessStarted string `json:"process_started"`
	Takeover       bool   `json:"takeover"`
}
type OpenResponse struct {
	Agent   Agent   `json:"agent"`
	Session Session `json:"session"`
}

type Message struct {
	ID               string `json:"id"`
	SenderMachine    string `json:"sender_machine_id"`
	SenderAgent      string `json:"sender_agent_id"`
	RecipientMachine string `json:"recipient_machine_id"`
	RecipientAgent   string `json:"recipient_agent_id"`
	Kind             string `json:"kind"`
	Body             string `json:"body,omitempty"`
	Wire             []byte `json:"-"`
	State            string `json:"state"`
	CreatedAt        int64  `json:"created_at"`
	ExpiresAt        int64  `json:"expires_at"`
	ReceivedAt       *int64 `json:"received_at,omitempty"`
	HandedOffAt      *int64 `json:"handed_off_at,omitempty"`
	Attempts         int    `json:"attempt_count"`
	NextAttempt      int64  `json:"next_attempt_at"`
	Failure          string `json:"failure_code,omitempty"`
	PrunedAt         *int64 `json:"pruned_at,omitempty"`
	AttemptID        string `json:"attempt_id,omitempty"`
	AttachmentID     string `json:"attachment_id,omitempty"`
	Hash             string `json:"-"`
	// Trust is porter's label for a peer message delivered to a local agent
	// (never stored): "trusted: session", "trusted: always" or "untrusted".
	Trust string `json:"trust,omitempty"`
}

func (m Message) Key() string { return m.SenderMachine + "/" + m.ID }
func (m Message) Terminal() bool {
	return m.State == "handed_off" || m.State == "undeliverable" || m.State == "expired"
}

type SendRequest struct {
	SessionID string `json:"session_id"`
	To        string `json:"to"`
	Body      string `json:"body"`
	ID        string `json:"id,omitempty"`
}
type Handoff struct {
	SenderMachine string `json:"sender_machine_id"`
	MessageID     string `json:"message_id"`
	AttemptID     string `json:"attempt_id"`
	Status        string `json:"status"`
	Failure       string `json:"failure_code,omitempty"`
}
type Receipt struct {
	OriginalID string `json:"original_id"`
	Status     string `json:"status"`
	At         int64  `json:"at"`
	Failure    string `json:"failure_code,omitempty"`
}
type Routing struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	Sender    string `json:"sender_machine_id"`
	Recipient string `json:"recipient_machine_id"`
	Kind      string `json:"kind"`
	ExpiresAt int64  `json:"expires_at"`
}
type Envelope struct {
	Routing
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}
type Payload struct {
	Routing
	SenderAgent    string   `json:"sender_agent_id"`
	RecipientAgent string   `json:"recipient_agent_id"`
	CreatedAt      int64    `json:"created_at"`
	Body           string   `json:"body,omitempty"`
	Receipt        *Receipt `json:"receipt,omitempty"`
}
type Presence struct {
	MachineID  string `json:"machine_id"`
	PeerAlias  string `json:"peer_alias,omitempty"`
	AgentID    string `json:"agent_id"`
	Alias      string `json:"alias"`
	Persistent bool   `json:"persistent"`
	Online     bool   `json:"online"`
	ExpiresAt  int64  `json:"lease_expires_at"`
}

func (p Presence) Address() string {
	if p.PeerAlias != "" {
		return p.PeerAlias + ":" + p.Alias
	}
	return p.Alias
}

type HistoryRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Target    string `json:"target"`
	AgentID   string `json:"agent_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Pending   bool   `json:"pending,omitempty"`
}
type HistoryPage struct {
	Messages []Message `json:"messages"`
	Cursor   string    `json:"next_cursor,omitempty"`
}
type Event struct {
	Type     string      `json:"type"`
	Message  *Message    `json:"message,omitempty"`
	Envelope *Envelope   `json:"envelope,omitempty"`
	Query    *RelayQuery `json:"query,omitempty"`
	Detail   string      `json:"detail,omitempty"`
}
type RelayQuery struct {
	ID        string `json:"id"`
	Sender    string `json:"sender_machine_id"`
	Recipient string `json:"recipient_machine_id"`
	Sealed    []byte `json:"sealed"`
	Deadline  int64  `json:"deadline"`
}
type QueryPayload struct {
	Purpose   string       `json:"purpose"`
	ID        string       `json:"id"`
	Sender    string       `json:"sender"`
	Recipient string       `json:"recipient"`
	AgentID   string       `json:"agent_id"`
	Limit     int          `json:"limit"`
	Cursor    string       `json:"cursor,omitempty"`
	Deadline  int64        `json:"deadline"`
	Page      *HistoryPage `json:"page,omitempty"`
	Error     *APIError    `json:"error,omitempty"`
}

func validateRouting(r Routing) error {
	if r.Version != ProtocolVersion || !validWireID(r.ID) || len(r.ID) > 80 || !validWireID(r.Sender) || !validWireID(r.Recipient) {
		return problem(400, "bad_envelope", "invalid routing")
	}
	if r.Kind != "message" && r.Kind != "receipt" {
		return problem(400, "bad_kind", "only message and receipt envelopes are supported")
	}
	if r.ExpiresAt <= Now() {
		return problem(410, "expired", "envelope expired")
	}
	if r.ExpiresAt > time.Now().Add(MessageTTL+time.Minute).UnixMilli() {
		return problem(400, "bad_expiry", "expiry exceeds seven days")
	}
	return nil
}
func key32(b []byte) (*[32]byte, error) {
	if len(b) != 32 {
		return nil, fmt.Errorf("expected 32-byte key")
	}
	var v [32]byte
	copy(v[:], b)
	return &v, nil
}
