package comms

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
)

const (
	brokerChallengeTTL    = time.Minute
	brokerCredentialTTL   = 24 * time.Hour
	brokerHistoryTTL      = 10 * time.Second
	brokerMaxChallenges   = 1024
	brokerMaxStreams      = 256
	brokerMaxPresence     = 4096
	brokerReceiptWire     = 4096 + box.Overhead
	brokerMaxEnvelopeJSON = MaxWire*4/3 + 4096
)

// Broker is an optional role with independent durable state. It never receives
// a peer's private key or plaintext message/history content.
type Broker struct {
	db              *sql.DB
	cfg             Config
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	closed          bool
	streams         map[string]*brokerStream
	challenges      map[string]brokerChallenge
	queries         map[string]*brokerQuery
	requests        chan struct{}
	historyRequests chan struct{}
	wg              sync.WaitGroup
	closeOnce       sync.Once
	closeErr        error
}

type brokerChallenge struct {
	MachineID  string
	PublicKey  []byte
	SecretHash string
	ExpiresAt  int64
}

type brokerPrincipal struct {
	MachineID string
	TokenHash string
	ExpiresAt int64
}

type brokerStream struct {
	principal brokerPrincipal
	wake      chan struct{}
	replay    chan struct{}
	queries   chan RelayQuery
	done      chan struct{}
	once      sync.Once
}

func (s *brokerStream) stop() { s.once.Do(func() { close(s.done) }) }
func (s *brokerStream) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

type brokerQuery struct {
	query  RelayQuery
	target *brokerStream
	result chan brokerQueryResult
}

type brokerQueryResult struct {
	sealed []byte
	err    error
}

func NewBroker(cfg Config) (*Broker, error) {
	defaults := DefaultConfig()
	if cfg.DataDir == "" {
		cfg.DataDir = defaults.DataDir
	}
	if err := loadServiceKey(&cfg); err != nil {
		return nil, err
	}
	if cfg.Lease <= 0 {
		cfg.Lease = defaults.Lease
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = defaults.Heartbeat
	}
	if cfg.BrokerCount <= 0 {
		cfg.BrokerCount = defaults.BrokerCount
	}
	if cfg.BrokerBytes <= 0 {
		cfg.BrokerBytes = defaults.BrokerBytes
	}
	if cfg.ReceiptCount <= 0 {
		cfg.ReceiptCount = defaults.ReceiptCount
	}
	if cfg.ReceiptBytes <= 0 {
		cfg.ReceiptBytes = defaults.ReceiptBytes
	}
	db, err := openDB(filepath.Join(cfg.DataDir, "broker.db"))
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS schema_version(version INTEGER PRIMARY KEY)`); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow(`SELECT COALESCE(max(version),0) FROM schema_version`).Scan(&version); err != nil || version > 1 {
		db.Close()
		return nil, fmt.Errorf("unsupported broker database version %d: %v", version, err)
	}
	_, err = db.Exec(`
BEGIN;
CREATE TABLE IF NOT EXISTS machines(
 machine_id TEXT PRIMARY KEY, public_key BLOB NOT NULL CHECK(length(public_key)=32),
 registered_at INTEGER NOT NULL, token_hash TEXT UNIQUE, token_expires_at INTEGER NOT NULL DEFAULT 0,
 presence_revision INTEGER NOT NULL DEFAULT 0, presence_hash TEXT NOT NULL DEFAULT '',
 lease_expires_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS grants(
 grantor_machine_id TEXT NOT NULL REFERENCES machines(machine_id), grantee_machine_id TEXT NOT NULL,
 allow_messages INTEGER NOT NULL CHECK(allow_messages IN(0,1)),
 allow_history INTEGER NOT NULL CHECK(allow_history IN(0,1)), revision INTEGER NOT NULL CHECK(revision>0),
 PRIMARY KEY(grantor_machine_id,grantee_machine_id));
CREATE TABLE IF NOT EXISTS presence(
 machine_id TEXT NOT NULL REFERENCES machines(machine_id), agent_id TEXT NOT NULL, alias TEXT NOT NULL,
 persistent INTEGER NOT NULL CHECK(persistent IN(0,1)), online INTEGER NOT NULL CHECK(online IN(0,1)),
 lease_expires_at INTEGER NOT NULL, PRIMARY KEY(machine_id,agent_id), UNIQUE(machine_id,alias));
CREATE TABLE IF NOT EXISTS pending_messages(
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 sender_machine_id TEXT NOT NULL REFERENCES machines(machine_id), id TEXT NOT NULL,
 recipient_machine_id TEXT NOT NULL REFERENCES machines(machine_id),
 kind TEXT NOT NULL CHECK(kind IN('message','receipt')), envelope BLOB NOT NULL,
 payload_hash TEXT NOT NULL, enqueued_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
 UNIQUE(sender_machine_id,id));
CREATE INDEX IF NOT EXISTS pending_recipient ON pending_messages(recipient_machine_id,kind,sequence);
CREATE INDEX IF NOT EXISTS pending_expiry ON pending_messages(expires_at);
INSERT OR IGNORE INTO schema_version VALUES(1);
UPDATE machines SET lease_expires_at=0;
COMMIT;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Broker{db: db, cfg: cfg, ctx: ctx, cancel: cancel, streams: map[string]*brokerStream{},
		challenges: map[string]brokerChallenge{}, queries: map[string]*brokerQuery{},
		requests: make(chan struct{}, 32), historyRequests: make(chan struct{}, 16)}
	b.wg.Add(1)
	go b.maintain()
	return b, nil
}

func (b *Broker) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.cancel()
		for _, s := range b.streams {
			s.stop()
		}
		b.mu.Unlock()
		b.wg.Wait()
		b.closeErr = b.db.Close()
	})
	return b.closeErr
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/challenges", b.challenge)
	mux.HandleFunc("POST /v1/auth/complete", b.complete)
	mux.HandleFunc("PUT /v1/presence", b.auth(b.putPresence))
	mux.HandleFunc("PUT /v1/grants/{peer}", b.auth(b.putGrant))
	mux.HandleFunc("GET /v1/who", b.auth(b.who))
	mux.HandleFunc("POST /v1/messages", b.auth(b.enqueue))
	mux.HandleFunc("POST /v1/receipts", b.auth(b.enqueue))
	mux.HandleFunc("POST /v1/acks", b.auth(b.ack))
	mux.HandleFunc("GET /v1/stream", b.auth(b.stream))
	mux.HandleFunc("POST /v1/history/{machine}", b.auth(b.history))
	mux.HandleFunc("POST /v1/history-results/{id}", b.auth(b.historyResult))
	return b.serviceKeyGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			brokerError(w, problem(503, "stopping", "broker is stopping"))
			return
		}
		b.wg.Add(1)
		b.mu.Unlock()
		defer b.wg.Done()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(b.ctx, cancel)
		defer func() { stop(); cancel() }()
		r = r.WithContext(ctx)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Long-lived streams and synchronous history queries have independent
		// budgets, leaving short writes, acknowledgments and heartbeats capacity.
		if r.URL.Path != "/v1/stream" && !strings.HasPrefix(r.URL.Path, "/v1/history/") {
			select {
			case b.requests <- struct{}{}:
				defer func() { <-b.requests }()
			default:
				brokerError(w, problem(429, "broker_busy", "broker request capacity reached"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
}

func brokerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func brokerError(w http.ResponseWriter, err error) {
	var api *APIError
	if !errors.As(err, &api) {
		api = &APIError{Code: "internal_error", ErrorText: "broker storage or processing failed", Status: 500}
	}
	brokerJSON(w, api.Status, api)
}

// Release SQLite's writer before an HTTP error response can wait on a client.
func brokerTransactionError(w http.ResponseWriter, tx *sql.Tx, err error) {
	if tx != nil {
		_ = tx.Rollback()
	}
	brokerError(w, err)
}

func brokerDecode(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Second))
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return problem(413, "too_large", "request exceeds size limit")
		}
		return problem(400, "bad_json", "invalid JSON request")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return problem(400, "bad_json", "expected one JSON object")
	}
	return nil
}

func brokerID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func (b *Broker) authenticate(r *http.Request) (brokerPrincipal, error) {
	p := brokerPrincipal{}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) != len("Bearer ")+43 {
		return p, problem(401, "unauthorized", "valid broker credential required")
	}
	p.TokenHash = digest([]byte(strings.TrimPrefix(header, "Bearer ")))
	err := b.db.QueryRowContext(r.Context(), `SELECT machine_id,token_expires_at FROM machines WHERE token_hash=?`, p.TokenHash).Scan(&p.MachineID, &p.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && p.ExpiresAt <= Now() {
		return p, problem(401, "unauthorized", "broker credential expired or revoked")
	}
	return p, err
}

func (b *Broker) auth(next func(http.ResponseWriter, *http.Request, brokerPrincipal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := b.authenticate(r)
		if err != nil {
			brokerError(w, err)
			return
		}
		next(w, r, p)
	}
}

func (b *Broker) challenge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MachineID string `json:"machine_id"`
		PublicKey []byte `json:"public_key"`
	}
	if err := brokerDecode(w, r, &req, 4096); err != nil {
		brokerError(w, err)
		return
	}
	if !brokerID(req.MachineID) || len(req.PublicKey) != 32 {
		brokerError(w, problem(400, "bad_identity", "valid machine ID and 32-byte public key required"))
		return
	}
	// Reject low-order points: anonymous boxes to these keys can have a publicly
	// known shared secret and therefore cannot establish key possession.
	var scalar [32]byte
	if _, err := rand.Read(scalar[:]); err != nil {
		brokerError(w, err)
		return
	}
	if _, err := curve25519.X25519(scalar[:], req.PublicKey); err != nil {
		brokerError(w, problem(400, "bad_key", "invalid X25519 public key"))
		return
	}
	var old []byte
	err := b.db.QueryRowContext(r.Context(), `SELECT public_key FROM machines WHERE machine_id=?`, req.MachineID).Scan(&old)
	if err == nil && !SameKey(old, req.PublicKey) {
		brokerError(w, problem(409, "key_changed", "v1 does not replace registered keys; use a new machine ID"))
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		brokerError(w, err)
		return
	}
	id, secret, expiry := NewID("c_"), randomToken(), time.Now().Add(brokerChallengeTTL).UnixMilli()
	plain := struct {
		Purpose   string `json:"purpose"`
		ID        string `json:"id"`
		MachineID string `json:"machine_id"`
		PublicKey []byte `json:"public_key"`
		Secret    string `json:"secret"`
		ExpiresAt int64  `json:"expires_at"`
	}{"comms-auth-v1", id, req.MachineID, req.PublicKey, secret, expiry}
	pub, _ := key32(req.PublicKey)
	sealed, err := box.SealAnonymous(nil, marshal(plain), pub, rand.Reader)
	if err != nil {
		brokerError(w, err)
		return
	}
	b.mu.Lock()
	now, count := Now(), 0
	for k, v := range b.challenges {
		if v.ExpiresAt <= now {
			delete(b.challenges, k)
		} else if v.MachineID == req.MachineID {
			count++
		}
	}
	if len(b.challenges) >= brokerMaxChallenges || count >= 8 {
		b.mu.Unlock()
		brokerError(w, problem(429, "challenge_limit", "too many pending authentication challenges"))
		return
	}
	b.challenges[id] = brokerChallenge{req.MachineID, bytes.Clone(req.PublicKey), digest([]byte(secret)), expiry}
	b.mu.Unlock()
	brokerJSON(w, 200, map[string]any{"id": id, "sealed": sealed})
}

func (b *Broker) complete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Proof string `json:"proof"`
	}
	if err := brokerDecode(w, r, &req, 4096); err != nil {
		brokerError(w, err)
		return
	}
	b.mu.Lock()
	c, ok := b.challenges[req.ID]
	delete(b.challenges, req.ID) // Every attempt consumes the challenge.
	if !ok || c.ExpiresAt <= Now() || subtle.ConstantTimeCompare([]byte(c.SecretHash), []byte(digest([]byte(req.Proof)))) != 1 {
		b.mu.Unlock()
		brokerError(w, problem(401, "invalid_proof", "invalid, expired or used challenge"))
		return
	}
	// Serialize credential replacement with stream installation, which rechecks
	// the token under this same lock before creating a current connection.
	tx, err := b.db.BeginTx(r.Context(), nil)
	if err != nil {
		b.mu.Unlock()
		brokerTransactionError(w, tx, err)
		return
	}
	defer tx.Rollback()
	var existing []byte
	err = tx.QueryRow(`SELECT public_key FROM machines WHERE machine_id=?`, c.MachineID).Scan(&existing)
	if err == nil && !SameKey(existing, c.PublicKey) {
		b.mu.Unlock()
		brokerTransactionError(w, tx, problem(409, "key_changed", "machine ID is already bound to a different key"))
		return
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		b.mu.Unlock()
		brokerTransactionError(w, tx, err)
		return
	}
	token, expires := randomToken(), time.Now().Add(brokerCredentialTTL).UnixMilli()
	_, err = tx.Exec(`INSERT INTO machines(machine_id,public_key,registered_at,token_hash,token_expires_at) VALUES(?,?,?,?,?) ON CONFLICT(machine_id) DO UPDATE SET token_hash=excluded.token_hash,token_expires_at=excluded.token_expires_at`, c.MachineID, c.PublicKey, Now(), digest([]byte(token)), expires)
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		if s := b.streams[c.MachineID]; s != nil {
			s.stop()
		}
	}
	b.mu.Unlock()
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	brokerJSON(w, 200, map[string]any{"token": token, "expires_at": expires})
}

func (b *Broker) putPresence(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	var req struct {
		Revision int64      `json:"revision"`
		Agents   []Presence `json:"agents"`
	}
	if err := brokerDecode(w, r, &req, 1<<20); err != nil {
		brokerError(w, err)
		return
	}
	if req.Revision <= 0 || len(req.Agents) > brokerMaxPresence {
		brokerError(w, problem(400, "bad_presence", "invalid revision or too many agents"))
		return
	}
	ids, aliases := map[string]bool{}, map[string]bool{}
	for i := range req.Agents {
		a := &req.Agents[i]
		if a.MachineID != "" && a.MachineID != p.MachineID {
			brokerError(w, problem(403, "sender_mismatch", "presence identity must match authentication"))
			return
		}
		if !brokerID(a.AgentID) || !validAlias(a.Alias) || ids[a.AgentID] || aliases[a.Alias] {
			brokerError(w, problem(400, "bad_presence", "invalid or duplicate agent identity/alias"))
			return
		}
		ids[a.AgentID], aliases[a.Alias] = true, true
		a.MachineID, a.PeerAlias, a.ExpiresAt = p.MachineID, "", 0
	}
	sort.Slice(req.Agents, func(i, j int) bool { return req.Agents[i].AgentID < req.Agents[j].AgentID })
	hash, lease := digest(marshal(req.Agents)), Now()+b.cfg.Lease.Milliseconds()
	tx, err := b.db.BeginTx(r.Context(), nil)
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	defer tx.Rollback()
	var rev int64
	var oldHash string
	if err = tx.QueryRow(`SELECT presence_revision,presence_hash FROM machines WHERE machine_id=?`, p.MachineID).Scan(&rev, &oldHash); err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	if req.Revision < rev || req.Revision == rev && hash != oldHash {
		brokerTransactionError(w, tx, problem(409, "stale_revision", "presence revision is stale or conflicts"))
		return
	}
	if _, err = tx.Exec(`DELETE FROM presence WHERE machine_id=?`, p.MachineID); err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	for _, a := range req.Agents {
		if _, err = tx.Exec(`INSERT INTO presence VALUES(?,?,?,?,?,?)`, p.MachineID, a.AgentID, a.Alias, a.Persistent, a.Online, lease); err != nil {
			brokerTransactionError(w, tx, err)
			return
		}
	}
	_, err = tx.Exec(`UPDATE machines SET presence_revision=?,presence_hash=?,lease_expires_at=? WHERE machine_id=?`, req.Revision, hash, lease, p.MachineID)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	b.wakeMachine(p.MachineID)
	brokerJSON(w, 200, map[string]any{"ok": true, "lease_expires_at": lease})
}

func (b *Broker) putGrant(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	peer := r.PathValue("peer")
	var grant Grant
	if err := brokerDecode(w, r, &grant, 4096); err != nil {
		brokerError(w, err)
		return
	}
	if !brokerID(peer) || peer == p.MachineID || grant.Revision <= 0 || grant.Grantee != "" && grant.Grantee != peer {
		brokerError(w, problem(400, "bad_grant", "invalid grant identity or revision"))
		return
	}
	grant.Grantee = peer
	tx, err := b.db.BeginTx(r.Context(), nil)
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	defer tx.Rollback()
	var old Grant
	err = tx.QueryRow(`SELECT allow_messages,allow_history,revision FROM grants WHERE grantor_machine_id=? AND grantee_machine_id=?`, p.MachineID, peer).Scan(&old.Messages, &old.History, &old.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		brokerTransactionError(w, tx, err)
		return
	}
	if err == nil && (grant.Revision < old.Revision || grant.Revision == old.Revision && (grant.Messages != old.Messages || grant.History != old.History)) {
		brokerTransactionError(w, tx, problem(409, "stale_revision", "grant revision is stale or conflicts"))
		return
	}
	// Nodes republish every grant on each sync. Only a newly allowed sender
	// needs the stream replayed (its envelopes may have been skipped while
	// denied); replaying on every unchanged republish resends all unacked
	// envelopes from the start and starves newer ones behind the duplicates.
	replay := grant.Messages && (errors.Is(err, sql.ErrNoRows) || !old.Messages)
	_, err = tx.Exec(`INSERT INTO grants VALUES(?,?,?,?,?) ON CONFLICT(grantor_machine_id,grantee_machine_id) DO UPDATE SET allow_messages=excluded.allow_messages,allow_history=excluded.allow_history,revision=excluded.revision`, p.MachineID, peer, grant.Messages, grant.History, grant.Revision)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	b.mu.Lock()
	if !grant.History {
		for _, q := range b.queries {
			if q.query.Recipient == p.MachineID && q.query.Sender == peer {
				select {
				case q.result <- brokerQueryResult{err: problem(403, "history_denied", "history permission was revoked")}:
				default:
				}
			}
		}
	}
	if s := b.streams[p.MachineID]; s != nil && replay {
		select {
		case s.replay <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
	brokerJSON(w, 200, grant)
}

func (b *Broker) hasGrant(ctx context.Context, target, sender string, history bool) (bool, error) {
	column := "allow_messages"
	if history {
		column = "allow_history"
	}
	var allowed bool
	err := b.db.QueryRowContext(ctx, `SELECT `+column+` FROM grants WHERE grantor_machine_id=? AND grantee_machine_id=?`, target, sender).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return allowed, err
}

func (b *Broker) who(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	// Snapshot active connections; no global mutex is held while querying SQL.
	b.mu.Lock()
	live := map[string]bool{}
	for id, s := range b.streams {
		select {
		case <-s.done:
		default:
			live[id] = true
		}
	}
	b.mu.Unlock()
	rows, err := b.db.QueryContext(r.Context(), `SELECT p.machine_id,p.agent_id,p.alias,p.persistent,p.online,p.lease_expires_at,m.lease_expires_at FROM presence p JOIN machines m ON m.machine_id=p.machine_id JOIN grants g ON g.grantor_machine_id=p.machine_id AND g.grantee_machine_id=? WHERE g.allow_messages=1 ORDER BY p.machine_id,p.alias`, p.MachineID)
	if err != nil {
		brokerError(w, err)
		return
	}
	defer rows.Close()
	out := []Presence{}
	for rows.Next() {
		var a Presence
		var machineLease int64
		if err = rows.Scan(&a.MachineID, &a.AgentID, &a.Alias, &a.Persistent, &a.Online, &a.ExpiresAt, &machineLease); err != nil {
			rows.Close()
			brokerError(w, err)
			return
		}
		a.Online = a.Online && a.ExpiresAt > Now() && machineLease > Now() && live[a.MachineID]
		if a.ExpiresAt > machineLease {
			a.ExpiresAt = machineLease
		}
		if a.Online || a.Persistent {
			out = append(out, a)
			if len(out) > brokerMaxPresence {
				rows.Close()
				brokerError(w, problem(413, "presence_limit", "visible presence exceeds the supported snapshot size"))
				return
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		brokerError(w, err)
		return
	}
	brokerJSON(w, 200, out)
}

func (b *Broker) enqueue(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	var env Envelope
	limit := int64(brokerMaxEnvelopeJSON)
	if r.URL.Path == "/v1/receipts" {
		limit = 8192
	}
	if err := brokerDecode(w, r, &env, limit); err != nil {
		brokerError(w, err)
		return
	}
	if err := validateRouting(env.Routing); err != nil {
		brokerError(w, err)
		return
	}
	kind := "message"
	if r.URL.Path == "/v1/receipts" {
		kind = "receipt"
	}
	if env.Sender != p.MachineID {
		brokerError(w, problem(403, "sender_mismatch", "envelope sender must match authenticated machine"))
		return
	}
	if !brokerID(env.Recipient) || !brokerID(env.ID) || env.Sender == env.Recipient || env.Kind != kind || len(env.Nonce) != 24 || len(env.Ciphertext) < box.Overhead || len(env.Ciphertext) > MaxWire || kind == "receipt" && len(env.Ciphertext) > brokerReceiptWire {
		brokerError(w, problem(400, "bad_envelope", "invalid ciphertext, routing, or endpoint kind"))
		return
	}
	wire := marshal(env)
	hash := digest(wire)
	tx, err := b.db.BeginTx(r.Context(), nil)
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRow(`SELECT 1 FROM machines WHERE machine_id=?`, env.Recipient).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = problem(404, "unknown_machine", "recipient machine is not registered")
		}
		brokerTransactionError(w, tx, err)
		return
	}
	if kind == "message" {
		var allowed bool
		err = tx.QueryRow(`SELECT allow_messages FROM grants WHERE grantor_machine_id=? AND grantee_machine_id=?`, env.Recipient, p.MachineID).Scan(&allowed)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !allowed {
			brokerTransactionError(w, tx, problem(403, "message_denied", "recipient has not granted messaging permission"))
			return
		}
		if err != nil {
			brokerTransactionError(w, tx, err)
			return
		}
	}
	var oldHash string
	err = tx.QueryRow(`SELECT payload_hash FROM pending_messages WHERE sender_machine_id=? AND id=?`, p.MachineID, env.ID).Scan(&oldHash)
	if err == nil {
		if oldHash != hash {
			brokerTransactionError(w, tx, problem(409, "id_conflict", "envelope ID already refers to different content"))
			return
		}
		if err = tx.Commit(); err != nil {
			brokerTransactionError(w, tx, err)
			return
		}
		b.wakeMachine(env.Recipient)
		brokerJSON(w, 200, map[string]any{"id": env.ID, "state": "queued", "duplicate": true})
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		brokerTransactionError(w, tx, err)
		return
	}
	// Expired work does not consume quota; deleting it is expiry, never eviction.
	if _, err = tx.Exec(`DELETE FROM pending_messages WHERE recipient_machine_id=? AND expires_at<=?`, env.Recipient, Now()); err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	var count int
	var size int64
	err = tx.QueryRow(`SELECT count(*),COALESCE(sum(length(envelope)),0) FROM pending_messages WHERE recipient_machine_id=? AND kind=?`, env.Recipient, kind).Scan(&count, &size)
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	maxCount, maxBytes, code := b.cfg.BrokerCount, b.cfg.BrokerBytes, "queue_full"
	if kind == "receipt" {
		maxCount, maxBytes, code = b.cfg.ReceiptCount, b.cfg.ReceiptBytes, "receipt_queue_full"
	}
	if count >= maxCount || size+int64(len(wire)) > maxBytes {
		brokerTransactionError(w, tx, problem(429, code, "recipient pending queue capacity reached"))
		return
	}
	_, err = tx.Exec(`INSERT INTO pending_messages(sender_machine_id,id,recipient_machine_id,kind,envelope,payload_hash,enqueued_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`, p.MachineID, env.ID, env.Recipient, kind, wire, hash, Now(), env.ExpiresAt)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		brokerTransactionError(w, tx, err)
		return
	}
	b.wakeMachine(env.Recipient)
	brokerJSON(w, 202, map[string]any{"id": env.ID, "state": "queued"})
}

func (b *Broker) ack(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	var req struct {
		Sender string `json:"sender_machine_id"`
		ID     string `json:"id"`
	}
	if err := brokerDecode(w, r, &req, 4096); err != nil {
		brokerError(w, err)
		return
	}
	if !brokerID(req.Sender) || !brokerID(req.ID) {
		brokerError(w, problem(400, "bad_ack", "sender machine ID and envelope ID required"))
		return
	}
	_, err := b.db.ExecContext(r.Context(), `DELETE FROM pending_messages WHERE sender_machine_id=? AND id=? AND recipient_machine_id=?`, req.Sender, req.ID, p.MachineID)
	if err != nil {
		brokerError(w, err)
		return
	}
	brokerJSON(w, 200, map[string]bool{"ok": true})
}

func (b *Broker) wakeMachine(machine string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.streams[machine]; s != nil {
		s.notify()
	}
}

func (b *Broker) stream(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	if _, ok := w.(http.Flusher); !ok {
		brokerError(w, problem(500, "stream_unsupported", "HTTP transport does not support streaming"))
		return
	}
	b.mu.Lock()
	if len(b.streams) >= brokerMaxStreams && b.streams[p.MachineID] == nil {
		b.mu.Unlock()
		brokerError(w, problem(429, "stream_limit", "broker connection limit reached"))
		return
	}
	var valid int
	err := b.db.QueryRowContext(r.Context(), `SELECT 1 FROM machines WHERE machine_id=? AND token_hash=? AND token_expires_at>?`, p.MachineID, p.TokenHash, Now()).Scan(&valid)
	if err != nil {
		b.mu.Unlock()
		brokerError(w, problem(401, "unauthorized", "credential expired or revoked"))
		return
	}
	s := &brokerStream{principal: p, wake: make(chan struct{}, 1), replay: make(chan struct{}, 1), queries: make(chan RelayQuery, 8), done: make(chan struct{})}
	if old := b.streams[p.MachineID]; old != nil {
		old.stop()
	}
	b.streams[p.MachineID] = s
	b.mu.Unlock()
	defer func() {
		s.stop()
		b.mu.Lock()
		if b.streams[p.MachineID] == s {
			delete(b.streams, p.MachineID)
		}
		b.mu.Unlock()
	}()
	if _, err = b.db.ExecContext(r.Context(), `UPDATE machines SET lease_expires_at=? WHERE machine_id=?`, Now()+b.cfg.Lease.Milliseconds(), p.MachineID); err != nil {
		brokerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err = controller.Flush(); err != nil {
		return
	}
	_ = controller.SetWriteDeadline(time.Time{})
	write := func(event Event) error {
		select {
		case <-s.done:
			return context.Canceled
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := json.NewEncoder(w).Encode(event); err != nil {
			return err
		}
		err := controller.Flush()
		_ = controller.SetWriteDeadline(time.Time{})
		return err
	}
	heartbeat := time.NewTicker(b.cfg.Heartbeat)
	defer heartbeat.Stop()
	redeliver := time.NewTicker(30 * time.Second)
	defer redeliver.Stop()
	credential := time.NewTimer(time.Until(time.UnixMilli(p.ExpiresAt)))
	defer credential.Stop()
	var cursor int64
	s.notify()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-credential.C:
			return
		case <-s.replay:
			cursor = 0
			s.notify()
		case <-redeliver.C:
			cursor = 0
			s.notify()
		case <-heartbeat.C:
			var lease int64
			if err = b.db.QueryRowContext(r.Context(), `SELECT lease_expires_at FROM machines WHERE machine_id=?`, p.MachineID).Scan(&lease); err != nil || lease <= Now() {
				return
			}
			if err = write(Event{Type: "heartbeat"}); err != nil {
				return
			}
		case q := <-s.queries:
			if q.Deadline <= Now() {
				continue
			}
			allowed, e := b.hasGrant(r.Context(), p.MachineID, q.Sender, true)
			if e != nil {
				return
			}
			if !allowed {
				continue
			}
			if err = write(Event{Type: "history_query", Query: &q}); err != nil {
				return
			}
		case <-s.wake:
			items, e := b.queueBatch(r.Context(), p.MachineID, cursor)
			if e != nil {
				return
			}
			for _, item := range items {
				cursor = item.sequence
				if item.envelope.ExpiresAt <= Now() {
					continue
				}
				if item.envelope.Kind == "message" {
					allowed, e := b.hasGrant(r.Context(), p.MachineID, item.envelope.Sender, false)
					if e != nil {
						return
					}
					if !allowed {
						continue
					}
				}
				if err = write(Event{Type: "envelope", Envelope: &item.envelope}); err != nil {
					return
				}
			}
			if len(items) == 8 {
				s.notify()
			}
		}
	}
}

type brokerQueueItem struct {
	sequence int64
	envelope Envelope
}

func (b *Broker) queueBatch(ctx context.Context, machine string, cursor int64) ([]brokerQueueItem, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT sequence,envelope FROM pending_messages WHERE recipient_machine_id=? AND sequence>? AND expires_at>? ORDER BY sequence LIMIT 8`, machine, cursor, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []brokerQueueItem{}
	for rows.Next() {
		var item brokerQueueItem
		var wire []byte
		if err = rows.Scan(&item.sequence, &wire); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(wire, &item.envelope); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (b *Broker) history(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	select {
	case b.historyRequests <- struct{}{}:
		defer func() { <-b.historyRequests }()
	default:
		brokerError(w, problem(429, "history_busy", "history query capacity reached"))
		return
	}
	var q RelayQuery
	if err := brokerDecode(w, r, &q, 3*MaxHistory); err != nil {
		brokerError(w, err)
		return
	}
	target := r.PathValue("machine")
	if !brokerID(q.ID) || !brokerID(target) || q.Recipient != target || q.Sender != p.MachineID || q.Sender == target || len(q.Sealed) < 40 || len(q.Sealed) > 2*MaxHistory || q.Deadline <= Now() || q.Deadline > Now()+brokerHistoryTTL.Milliseconds()+1000 {
		brokerError(w, problem(400, "bad_query", "invalid history routing, size or deadline"))
		return
	}
	allowed, err := b.hasGrant(r.Context(), target, p.MachineID, true)
	if err != nil {
		brokerError(w, err)
		return
	}
	if !allowed {
		brokerError(w, problem(403, "history_denied", "target has not granted history access"))
		return
	}
	var lease int64
	if err = b.db.QueryRowContext(r.Context(), `SELECT lease_expires_at FROM machines WHERE machine_id=?`, target).Scan(&lease); err != nil || lease <= Now() {
		brokerError(w, problem(503, "peer_offline", "history requires an online target node"))
		return
	}
	b.mu.Lock()
	s := b.streams[target]
	if s == nil {
		b.mu.Unlock()
		brokerError(w, problem(503, "peer_offline", "history requires an online target node"))
		return
	}
	select {
	case <-s.done:
		b.mu.Unlock()
		brokerError(w, problem(503, "peer_offline", "target connection ended"))
		return
	default:
	}
	if b.queries[q.ID] != nil {
		b.mu.Unlock()
		brokerError(w, problem(409, "query_conflict", "query ID is already active"))
		return
	}
	senderCount, targetCount := 0, 0
	for _, existing := range b.queries {
		if existing.query.Sender == p.MachineID {
			senderCount++
		}
		if existing.query.Recipient == target {
			targetCount++
		}
	}
	if senderCount >= 4 || targetCount >= 8 {
		b.mu.Unlock()
		brokerError(w, problem(429, "history_busy", "peer history query capacity reached"))
		return
	}
	wait := &brokerQuery{query: q, target: s, result: make(chan brokerQueryResult, 1)}
	b.queries[q.ID] = wait
	select {
	case s.queries <- q:
	default:
		delete(b.queries, q.ID)
		b.mu.Unlock()
		brokerError(w, problem(429, "history_busy", "target history queue is full"))
		return
	}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.queries, q.ID); b.mu.Unlock() }()
	duration := time.Until(time.UnixMilli(q.Deadline))
	if duration > brokerHistoryTTL {
		duration = brokerHistoryTTL
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case result := <-wait.result:
		if result.err != nil {
			brokerError(w, result.err)
			return
		}
		allowed, err = b.hasGrant(r.Context(), target, p.MachineID, true)
		if err != nil {
			brokerError(w, err)
			return
		}
		if !allowed {
			brokerError(w, problem(403, "history_denied", "history permission was revoked"))
			return
		}
		brokerJSON(w, 200, map[string]any{"sealed": result.sealed})
	case <-s.done:
		brokerError(w, problem(503, "peer_offline", "target connection ended"))
	case <-r.Context().Done():
		return
	case <-timer.C:
		brokerError(w, problem(504, "history_timeout", "history query timed out"))
	}
}

func (b *Broker) historyResult(w http.ResponseWriter, r *http.Request, p brokerPrincipal) {
	var req struct {
		Sealed []byte `json:"sealed"`
	}
	if err := brokerDecode(w, r, &req, 3*MaxHistory); err != nil {
		brokerError(w, err)
		return
	}
	if len(req.Sealed) < 40 || len(req.Sealed) > 2*MaxHistory {
		brokerError(w, problem(400, "bad_query", "invalid encrypted history result size"))
		return
	}
	b.mu.Lock()
	q := b.queries[r.PathValue("id")]
	if q == nil || q.query.Deadline <= Now() {
		b.mu.Unlock()
		brokerError(w, problem(404, "unknown_query", "history query expired or does not exist"))
		return
	}
	if q.query.Recipient != p.MachineID {
		b.mu.Unlock()
		brokerError(w, problem(403, "wrong_query_peer", "only the requested node can answer this query"))
		return
	}
	select {
	case q.result <- brokerQueryResult{sealed: req.Sealed}:
	default:
		b.mu.Unlock()
		brokerError(w, problem(409, "query_answered", "history result already supplied"))
		return
	}
	b.mu.Unlock()
	brokerJSON(w, 200, map[string]bool{"ok": true})
}

func (b *Broker) maintain() {
	defer b.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			now := Now()
			b.mu.Lock()
			for id, c := range b.challenges {
				if c.ExpiresAt <= now {
					delete(b.challenges, id)
				}
			}
			b.mu.Unlock()
			_, _ = b.db.ExecContext(b.ctx, `DELETE FROM pending_messages WHERE expires_at<=?`, now)
			_, _ = b.db.ExecContext(b.ctx, `DELETE FROM presence WHERE persistent=0 AND lease_expires_at<=?`, now)
			_, _ = b.db.ExecContext(b.ctx, `UPDATE machines SET token_hash=NULL,token_expires_at=0 WHERE token_expires_at<=? AND token_hash IS NOT NULL`, now)
		}
	}
}
