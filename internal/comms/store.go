package comms

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	ReadDB   *sql.DB
	DB       *sql.DB
	Identity Identity
	cfg      Config
}

func openDB(path string) (*sql.DB, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	f.Close()
	if e = os.Chmod(path, 0600); e != nil {
		return nil, e
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "cache_size(-4096)")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if e = db.Ping(); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}

func OpenStore(cfg Config) (*Store, error) {
	db, e := openDB(filepath.Join(cfg.DataDir, "node.db"))
	if e != nil {
		return nil, e
	}
	s := &Store{DB: db, cfg: cfg}
	// Refuse an unsupported database before executing any schema migration.
	var hasVersion int
	if e = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_version'`).Scan(&hasVersion); e != nil {
		db.Close()
		return nil, e
	}
	if hasVersion != 0 {
		var existing int
		if e = db.QueryRow(`SELECT COALESCE(max(version),0) FROM schema_version`).Scan(&existing); e != nil {
			db.Close()
			return nil, e
		}
		if existing > 1 || existing < 0 {
			db.Close()
			return nil, fmt.Errorf("unsupported node database version: %d", existing)
		}
	}
	_, e = db.Exec(`
CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY);
INSERT OR IGNORE INTO schema_version VALUES(1);
CREATE TABLE IF NOT EXISTS node_identity(singleton INTEGER PRIMARY KEY CHECK(singleton=1),machine_id TEXT NOT NULL UNIQUE,public_key BLOB NOT NULL CHECK(length(public_key)=32),private_key BLOB NOT NULL CHECK(length(private_key)=32),created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS peers(machine_id TEXT PRIMARY KEY,alias TEXT NOT NULL UNIQUE,public_key BLOB NOT NULL CHECK(length(public_key)=32),verified_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS grants(grantee_machine_id TEXT PRIMARY KEY REFERENCES peers(machine_id),allow_messages INTEGER NOT NULL CHECK(allow_messages IN(0,1)),allow_history INTEGER NOT NULL CHECK(allow_history IN(0,1)),revision INTEGER NOT NULL CHECK(revision>0));
CREATE TABLE IF NOT EXISTS agents(id TEXT PRIMARY KEY,alias TEXT NOT NULL,persistent INTEGER NOT NULL CHECK(persistent IN(0,1)),created_at INTEGER NOT NULL,retired_at INTEGER);
CREATE UNIQUE INDEX IF NOT EXISTS agents_alias ON agents(alias) WHERE retired_at IS NULL;
CREATE TABLE IF NOT EXISTS agent_sessions(id TEXT PRIMARY KEY,agent_id TEXT NOT NULL REFERENCES agents(id),harness TEXT NOT NULL,harness_session_id TEXT NOT NULL,scope TEXT NOT NULL CHECK(scope IN('local','global')),delivery_target TEXT NOT NULL DEFAULT '',process_id INTEGER NOT NULL DEFAULT 0,process_started TEXT NOT NULL DEFAULT '',started_at INTEGER NOT NULL,lease_expires_at INTEGER NOT NULL,ended_at INTEGER);
CREATE UNIQUE INDEX IF NOT EXISTS agent_active_session ON agent_sessions(agent_id) WHERE ended_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS harness_active_session ON agent_sessions(harness,harness_session_id) WHERE ended_at IS NULL;
CREATE TABLE IF NOT EXISTS messages(id TEXT NOT NULL,sender_machine_id TEXT NOT NULL,sender_agent_id TEXT NOT NULL,recipient_machine_id TEXT NOT NULL,recipient_agent_id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN('message','receipt')),body TEXT NOT NULL DEFAULT '',wire_envelope BLOB,state TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,received_at INTEGER,handed_off_at INTEGER,attempt_count INTEGER NOT NULL DEFAULT 0,next_attempt_at INTEGER NOT NULL DEFAULT 0,failure_code TEXT NOT NULL DEFAULT '',pruned_at INTEGER,attempt_id TEXT NOT NULL DEFAULT '',attachment_id TEXT NOT NULL DEFAULT '',payload_hash TEXT NOT NULL,PRIMARY KEY(sender_machine_id,id));
CREATE INDEX IF NOT EXISTS message_delivery ON messages(recipient_machine_id,recipient_agent_id,state,next_attempt_at);
CREATE INDEX IF NOT EXISTS message_outgoing ON messages(sender_machine_id,recipient_machine_id,state,next_attempt_at);
CREATE INDEX IF NOT EXISTS message_expiry ON messages(expires_at);
CREATE INDEX IF NOT EXISTS message_history ON messages(created_at,id);
`)
	if e != nil {
		db.Close()
		return nil, e
	}
	// The receipt obligation belongs to the accepted message. A full bounded
	// receipt outbox must not prevent recording a successful agent handoff.
	var hasReceiptPending bool
	columns, ce := db.Query(`PRAGMA table_info(messages)`)
	if ce != nil {
		db.Close()
		return nil, ce
	}
	for columns.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if ce = columns.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); ce != nil {
			columns.Close()
			db.Close()
			return nil, ce
		}
		if name == "receipt_pending" {
			hasReceiptPending = true
		}
	}
	columns.Close()
	if !hasReceiptPending {
		if _, ce = db.Exec(`ALTER TABLE messages ADD COLUMN receipt_pending INTEGER NOT NULL DEFAULT 0`); ce != nil {
			db.Close()
			return nil, ce
		}
	}
	if _, e = db.Exec(`CREATE INDEX IF NOT EXISTS receipt_obligations ON messages(receipt_pending) WHERE receipt_pending=1;
 CREATE INDEX IF NOT EXISTS active_expiry ON messages(expires_at) WHERE state IN('queued','forwarded','received');
 CREATE INDEX IF NOT EXISTS agent_session_history ON agent_sessions(agent_id);`); e != nil {
		db.Close()
		return nil, e
	}
	var version int
	if e = db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); e != nil || version != 1 {
		db.Close()
		return nil, fmt.Errorf("unsupported node database version: %d", version)
	}
	e = db.QueryRow(`SELECT machine_id,public_key,private_key,created_at FROM node_identity WHERE singleton=1`).Scan(&s.Identity.MachineID, &s.Identity.PublicKey, &s.Identity.PrivateKey, &s.Identity.CreatedAt)
	if errors.Is(e, sql.ErrNoRows) {
		s.Identity, e = GenerateIdentity()
		if e == nil {
			_, e = db.Exec(`INSERT INTO node_identity VALUES(1,?,?,?,?)`, s.Identity.MachineID, s.Identity.PublicKey, s.Identity.PrivateKey, s.Identity.CreatedAt)
		}
	}
	if e != nil {
		db.Close()
		return nil, e
	}
	// No saved socket/terminal is assumed ready on startup. A possibly emitted
	// handoff is never blindly retried after a process crash.
	_, e = db.Exec(`UPDATE messages SET state='uncertain',failure_code='node_restarted_during_handoff' WHERE state='delivering'; UPDATE agent_sessions SET lease_expires_at=0 WHERE ended_at IS NULL`)
	if e != nil {
		db.Close()
		return nil, e
	}
	readURL := url.URL{Scheme: "file", Path: filepath.Join(cfg.DataDir, "node.db")}
	rq := readURL.Query()
	rq.Set("mode", "ro")
	rq.Add("_pragma", "query_only(1)")
	rq.Add("_pragma", "busy_timeout(5000)")
	rq.Add("_pragma", "cache_size(-2048)")
	readURL.RawQuery = rq.Encode()
	s.ReadDB, e = sql.Open("sqlite", readURL.String())
	if e != nil {
		db.Close()
		return nil, e
	}
	s.ReadDB.SetMaxOpenConns(2)
	s.ReadDB.SetMaxIdleConns(2)
	if e = s.ReadDB.Ping(); e != nil {
		s.ReadDB.Close()
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error {
	var e error
	if s.ReadDB != nil {
		e = s.ReadDB.Close()
	}
	return errors.Join(e, s.DB.Close())
}
func (s *Store) Setting(key string) string {
	var v string
	s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}
func (s *Store) SetSetting(key, value string) error {
	_, e := s.DB.Exec(`INSERT INTO settings VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return e
}

func (s *Store) PutPeer(p Peer) error {
	if !validWireID(p.MachineID) || p.MachineID == s.Identity.MachineID || !validAlias(p.Alias) || p.Alias == "self" || p.Alias == s.Setting("name") || len(p.PublicKey) != 32 {
		return problem(400, "bad_peer", "invalid peer identity or alias")
	}
	if e := validatePublicKey(p.PublicKey); e != nil {
		return e
	}
	old, e := s.Peer(p.MachineID)
	if e == nil && !SameKey(old.PublicKey, p.PublicKey) {
		return problem(409, "key_changed", "v1 does not replace pinned keys; register a new machine identity")
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	p.VerifiedAt = Now()
	_, e = s.DB.Exec(`INSERT INTO peers VALUES(?,?,?,?) ON CONFLICT(machine_id) DO UPDATE SET alias=excluded.alias,verified_at=excluded.verified_at`, p.MachineID, p.Alias, p.PublicKey, p.VerifiedAt)
	if e != nil && strings.Contains(e.Error(), "UNIQUE") {
		return problem(409, "alias_taken", "peer alias already reserved")
	}
	return e
}
func (s *Store) Peer(ref string) (Peer, error) {
	var p Peer
	e := s.DB.QueryRow(`SELECT machine_id,alias,public_key,verified_at FROM peers WHERE machine_id=? OR alias=?`, ref, ref).Scan(&p.MachineID, &p.Alias, &p.PublicKey, &p.VerifiedAt)
	return p, e
}
func (s *Store) Peers() ([]Peer, error) {
	rows, e := s.DB.Query(`SELECT machine_id,alias,public_key,verified_at FROM peers ORDER BY alias`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		var p Peer
		if e = rows.Scan(&p.MachineID, &p.Alias, &p.PublicKey, &p.VerifiedAt); e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) Grant(ref string) (Grant, error) {
	var g Grant
	e := s.DB.QueryRow(`SELECT grantee_machine_id,allow_messages,allow_history,revision FROM grants WHERE grantee_machine_id=?`, ref).Scan(&g.Grantee, &g.Messages, &g.History, &g.Revision)
	return g, e
}
func (s *Store) Grants() ([]Grant, error) {
	r, e := s.DB.Query(`SELECT grantee_machine_id,allow_messages,allow_history,revision FROM grants ORDER BY grantee_machine_id`)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	v := []Grant{}
	for r.Next() {
		var g Grant
		if e = r.Scan(&g.Grantee, &g.Messages, &g.History, &g.Revision); e != nil {
			return nil, e
		}
		v = append(v, g)
	}
	return v, r.Err()
}
func (s *Store) SetGrant(ref string, messages, history bool) (Grant, error) {
	p, e := s.Peer(ref)
	if e != nil {
		return Grant{}, problem(404, "unknown_peer", "pair this peer before granting access")
	}
	_, e = s.DB.Exec(`INSERT INTO grants VALUES(?,?,?,1) ON CONFLICT(grantee_machine_id) DO UPDATE SET allow_messages=excluded.allow_messages,allow_history=excluded.allow_history,revision=grants.revision+1`, p.MachineID, messages, history)
	if e != nil {
		return Grant{}, e
	}
	return s.Grant(p.MachineID)
}

const sessionCols = `id,agent_id,harness,harness_session_id,scope,delivery_target,process_id,process_started,started_at,lease_expires_at,ended_at`

type scanner interface{ Scan(...any) error }

func scanSession(r scanner) (Session, error) {
	var v Session
	e := r.Scan(&v.ID, &v.AgentID, &v.Harness, &v.HarnessID, &v.Scope, &v.Target, &v.ProcessID, &v.ProcessStarted, &v.StartedAt, &v.LeaseExpiresAt, &v.EndedAt)
	return v, e
}
func (s *Store) Session(id string) (Session, error) {
	return scanSession(s.DB.QueryRow(`SELECT `+sessionCols+` FROM agent_sessions WHERE id=?`, id))
}
func (s *Store) ActiveSession(agent string) (Session, error) {
	return scanSession(s.DB.QueryRow(`SELECT `+sessionCols+` FROM agent_sessions WHERE agent_id=? AND ended_at IS NULL`, agent))
}
func (s *Store) Sessions() ([]Session, error) {
	r, e := s.DB.Query(`SELECT ` + sessionCols + ` FROM agent_sessions WHERE ended_at IS NULL`)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Session{}
	for r.Next() {
		v, e := scanSession(r)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, r.Err()
}
func (s *Store) Agent(ref string) (Agent, error) {
	var a Agent
	e := s.DB.QueryRow(`SELECT a.id,a.alias,a.persistent,a.created_at,a.retired_at,COALESCE((SELECT scope FROM agent_sessions WHERE agent_id=a.id ORDER BY rowid DESC LIMIT 1),'local') FROM agents a WHERE a.id=? OR (a.alias=? AND a.retired_at IS NULL) ORDER BY a.id=? DESC LIMIT 1`, ref, ref, ref).Scan(&a.ID, &a.Alias, &a.Persistent, &a.CreatedAt, &a.RetiredAt, &a.Scope)
	return a, e
}
func (s *Store) Agents(includeRetired bool) ([]Agent, error) {
	q := `SELECT a.id,a.alias,a.persistent,a.created_at,a.retired_at,COALESCE((SELECT scope FROM agent_sessions WHERE agent_id=a.id ORDER BY rowid DESC LIMIT 1),'local') FROM agents a`
	if !includeRetired {
		q += ` WHERE retired_at IS NULL`
	}
	q += ` ORDER BY alias`
	r, e := s.DB.Query(q)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Agent{}
	for r.Next() {
		var a Agent
		if e = r.Scan(&a.ID, &a.Alias, &a.Persistent, &a.CreatedAt, &a.RetiredAt, &a.Scope); e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, r.Err()
}

func (s *Store) OpenAgent(r OpenRequest) (OpenResponse, error) {
	var out OpenResponse
	if !validAlias(r.Alias) {
		return out, problem(400, "bad_alias", "alias must be 1–64 letters, digits, dots, dashes or underscores")
	}
	if r.Scope == "" {
		r.Scope = "local"
	}
	if r.Scope != "local" && r.Scope != "global" {
		return out, problem(400, "bad_scope", "scope must be local or global")
	}
	if r.Harness == "" {
		r.Harness = "service"
	}
	if r.Harness != "claude" && r.Harness != "codex" && r.Harness != "service" {
		return out, problem(400, "bad_harness", "unsupported harness")
	}
	if r.HarnessID == "" {
		return out, problem(400, "missing_session", "harness_session_id is required")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	var a Agent
	e = tx.QueryRow(`SELECT id,alias,persistent,created_at,retired_at FROM agents WHERE alias=? AND retired_at IS NULL`, r.Alias).Scan(&a.ID, &a.Alias, &a.Persistent, &a.CreatedAt, &a.RetiredAt)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if e == nil {
		if a.Persistent != r.Persistent {
			return out, problem(409, "persistence_mismatch", "use --persistent to resume a saved identity, or choose another alias")
		}
		active, ae := scanSession(tx.QueryRow(`SELECT `+sessionCols+` FROM agent_sessions WHERE agent_id=? AND ended_at IS NULL`, a.ID))
		if ae != nil && !errors.Is(ae, sql.ErrNoRows) {
			return out, ae
		}
		if ae == nil && active.Harness == r.Harness && active.HarnessID == r.HarnessID && !r.Takeover {
			_, e = tx.Exec(`UPDATE agent_sessions SET scope=?,delivery_target=?,lease_expires_at=?,process_id=?,process_started=? WHERE id=?`, r.Scope, r.Target, Now()+s.cfg.Lease.Milliseconds(), r.ProcessID, r.ProcessStarted, active.ID)
			if e != nil {
				return out, e
			}
			active.Scope = r.Scope
			active.Target = r.Target
			active.ProcessID = r.ProcessID
			active.ProcessStarted = r.ProcessStarted
			active.LeaseExpiresAt = Now() + s.cfg.Lease.Milliseconds()
			a.Scope = r.Scope
			out = OpenResponse{a, active}
			return out, tx.Commit()
		}
		if ae == nil && !r.Takeover {
			return out, problem(409, "identity_attached", "identity is already attached; explicit takeover required")
		}
		if ae == nil {
			if _, e = tx.Exec(`UPDATE agent_sessions SET ended_at=? WHERE id=?`, Now(), active.ID); e != nil {
				return out, e
			}
		}
		if !a.Persistent {
			if !r.Takeover {
				return out, problem(409, "identity_reserved", "ephemeral identity belongs to an earlier session; close or take over explicitly")
			}
			if e = s.retireTx(tx, a.ID); e != nil {
				return out, e
			}
			a = Agent{}
		}
	}
	if a.ID == "" {
		a = Agent{ID: NewID("a_"), Alias: r.Alias, Persistent: r.Persistent, CreatedAt: Now(), Scope: r.Scope}
		if _, e = tx.Exec(`INSERT INTO agents(id,alias,persistent,created_at) VALUES(?,?,?,?)`, a.ID, a.Alias, a.Persistent, a.CreatedAt); e != nil {
			return out, e
		}
	}
	v := Session{ID: NewID("s_"), AgentID: a.ID, Harness: r.Harness, HarnessID: r.HarnessID, Scope: r.Scope, Target: r.Target, ProcessID: r.ProcessID, ProcessStarted: r.ProcessStarted, StartedAt: Now(), LeaseExpiresAt: Now() + s.cfg.Lease.Milliseconds()}
	_, e = tx.Exec(`INSERT INTO agent_sessions(`+sessionCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,NULL)`, v.ID, v.AgentID, v.Harness, v.HarnessID, v.Scope, v.Target, v.ProcessID, v.ProcessStarted, v.StartedAt, v.LeaseExpiresAt)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE") {
			return out, problem(409, "session_attached", "this harness session already has an agent identity")
		}
		return out, e
	}
	a.Scope = r.Scope
	out = OpenResponse{a, v}
	return out, tx.Commit()
}
func (s *Store) Renew(id string) error {
	r, e := s.DB.Exec(`UPDATE agent_sessions SET lease_expires_at=? WHERE id=? AND ended_at IS NULL`, Now()+s.cfg.Lease.Milliseconds(), id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return problem(410, "attachment_ended", "attachment ended or replaced")
	}
	return nil
}
func (s *Store) Disconnect(id string) error {
	_, e := s.DB.Exec(`UPDATE agent_sessions SET lease_expires_at=0 WHERE id=? AND ended_at IS NULL`, id)
	return e
}
func (s *Store) CloseSession(id string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	v, e := scanSession(tx.QueryRow(`SELECT `+sessionCols+` FROM agent_sessions WHERE id=?`, id))
	if e != nil {
		return e
	}
	if v.EndedAt != nil {
		return nil
	}
	if _, e = tx.Exec(`UPDATE agent_sessions SET ended_at=?,lease_expires_at=0 WHERE id=?`, Now(), id); e != nil {
		return e
	}
	var persistent bool
	if e = tx.QueryRow(`SELECT persistent FROM agents WHERE id=?`, v.AgentID).Scan(&persistent); e != nil {
		return e
	}
	if !persistent {
		if e = s.retireTx(tx, v.AgentID); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) RetireAgent(id string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.retireTx(tx, id); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) retireTx(tx *sql.Tx, id string) error {
	if _, e := tx.Exec(`UPDATE agents SET retired_at=? WHERE id=? AND retired_at IS NULL`, Now(), id); e != nil {
		return e
	}
	if _, e := tx.Exec(`UPDATE agent_sessions SET ended_at=?,lease_expires_at=0 WHERE agent_id=? AND ended_at IS NULL`, Now(), id); e != nil {
		return e
	}
	return s.failPendingTx(tx, `recipient_machine_id=? AND recipient_agent_id=?`, []any{s.Identity.MachineID, id}, "agent_ended")
}

const msgCols = `id,sender_machine_id,sender_agent_id,recipient_machine_id,recipient_agent_id,kind,body,wire_envelope,state,created_at,expires_at,received_at,handed_off_at,attempt_count,next_attempt_at,failure_code,pruned_at,attempt_id,attachment_id,payload_hash`

func scanMessage(r scanner) (Message, error) {
	var m Message
	e := r.Scan(&m.ID, &m.SenderMachine, &m.SenderAgent, &m.RecipientMachine, &m.RecipientAgent, &m.Kind, &m.Body, &m.Wire, &m.State, &m.CreatedAt, &m.ExpiresAt, &m.ReceivedAt, &m.HandedOffAt, &m.Attempts, &m.NextAttempt, &m.Failure, &m.PrunedAt, &m.AttemptID, &m.AttachmentID, &m.Hash)
	return m, e
}
func insertMessage(tx *sql.Tx, m Message) error {
	_, e := tx.Exec(`INSERT INTO messages(`+msgCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.SenderMachine, m.SenderAgent, m.RecipientMachine, m.RecipientAgent, m.Kind, m.Body, m.Wire, m.State, m.CreatedAt, m.ExpiresAt, m.ReceivedAt, m.HandedOffAt, m.Attempts, m.NextAttempt, m.Failure, m.PrunedAt, m.AttemptID, m.AttachmentID, m.Hash)
	return e
}
func (s *Store) Message(sender, id string) (Message, error) {
	return scanMessage(s.DB.QueryRow(`SELECT `+msgCols+` FROM messages WHERE sender_machine_id=? AND id=?`, sender, id))
}
func (s *Store) FindMessage(id string) (Message, error) {
	return scanMessage(s.DB.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=? ORDER BY sender_machine_id=? DESC LIMIT 1`, id, s.Identity.MachineID))
}

func (s *Store) Insert(m Message, receipt bool) (Message, bool, error) {
	tx, e := s.DB.Begin()
	if e != nil {
		return m, false, e
	}
	defer tx.Rollback()
	old, e := scanMessage(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE sender_machine_id=? AND id=?`, m.SenderMachine, m.ID))
	if e == nil {
		if old.Hash != m.Hash {
			return old, true, problem(409, "id_conflict", "message ID already refers to different content")
		}
		if receipt && old.Kind == "message" {
			if e = s.receiptTx(tx, old, old.State, old.Failure); e != nil {
				return old, true, e
			}
		}
		return old, true, tx.Commit()
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return m, false, e
	}
	if e = s.checkQuotaTx(tx, m); e != nil {
		return m, false, e
	}
	if e = insertMessage(tx, m); e != nil {
		return m, false, e
	}
	if receipt {
		if e = s.receiptTx(tx, m, m.State, m.Failure); e != nil {
			return m, false, e
		}
	}
	return m, false, tx.Commit()
}
func (s *Store) checkQuotaTx(tx *sql.Tx, m Message) error {
	if m.Terminal() {
		return nil
	}
	var count int
	var bytes int64
	if m.Kind == "receipt" {
		if e := tx.QueryRow(`SELECT count(*),COALESCE(sum(length(CAST(body AS BLOB))+COALESCE(length(wire_envelope),0)),0) FROM messages WHERE kind='receipt' AND state IN('queued','forwarded')`).Scan(&count, &bytes); e != nil {
			return e
		}
		if count >= s.cfg.ReceiptCount || bytes+int64(len(m.Body)+len(m.Wire)) > s.cfg.ReceiptBytes {
			return problem(429, "receipt_queue_full", "receipt reserve is full")
		}
		return nil
	}
	agent := m.RecipientAgent
	if m.SenderMachine == s.Identity.MachineID && m.RecipientMachine != s.Identity.MachineID {
		agent = m.SenderAgent
	}
	e := tx.QueryRow(`SELECT count(*),COALESCE(sum(length(CAST(body AS BLOB))+COALESCE(length(wire_envelope),0)),0) FROM messages WHERE kind='message' AND state IN('queued','forwarded','received','delivering','uncertain') AND ((recipient_machine_id=? AND recipient_agent_id=?) OR (sender_machine_id=? AND sender_agent_id=? AND recipient_machine_id<>?))`, s.Identity.MachineID, agent, s.Identity.MachineID, agent, s.Identity.MachineID).Scan(&count, &bytes)
	if e != nil {
		return e
	}
	if count >= s.cfg.LocalCount || bytes+int64(len(m.Body)+len(m.Wire)) > s.cfg.LocalBytes {
		return problem(429, "queue_full", "agent pending-mail limit reached")
	}
	return nil
}
func (s *Store) receiptTx(tx *sql.Tx, m Message, status, code string) error {
	if m.Kind != "message" || m.SenderMachine == s.Identity.MachineID || m.RecipientMachine != s.Identity.MachineID {
		return nil
	}
	if status == "queued" || status == "forwarded" || status == "delivering" {
		status = "received"
	}
	if status != "received" && status != "handed_off" && status != "undeliverable" && status != "expired" && status != "uncertain" {
		return nil
	}
	if _, e := tx.Exec(`UPDATE messages SET receipt_pending=1 WHERE sender_machine_id=? AND id=?`, m.SenderMachine, m.ID); e != nil {
		return e
	}
	r := Receipt{m.ID, status, Now(), code}
	// Stable per-outcome receipt identity bounds duplicates while its broker copy
	// is outstanding. A later redelivery can safely regenerate the same receipt.
	id := "r_" + digest([]byte(m.SenderMachine + "/" + m.ID + "/" + status))[:32]
	old, e := scanMessage(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE sender_machine_id=? AND id=?`, s.Identity.MachineID, id))
	if e == nil {
		if old.State == "handed_off" {
			_, e = tx.Exec(`UPDATE messages SET state='queued',next_attempt_at=0 WHERE sender_machine_id=? AND id=?`, s.Identity.MachineID, id)
		}
		if e != nil {
			return e
		}
		_, e = tx.Exec(`UPDATE messages SET receipt_pending=0 WHERE sender_machine_id=? AND id=?`, m.SenderMachine, m.ID)
		return e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	out := Message{ID: id, SenderMachine: s.Identity.MachineID, SenderAgent: m.RecipientAgent, RecipientMachine: m.SenderMachine, RecipientAgent: m.SenderAgent, Kind: "receipt", Body: string(marshal(r)), State: "queued", CreatedAt: Now(), ExpiresAt: time.Now().Add(MessageTTL).UnixMilli(), Hash: digest(marshal(r))}
	if e = s.checkQuotaTx(tx, out); e != nil {
		var api *APIError
		if errors.As(e, &api) && api.Status == 429 {
			return nil
		}
		return e
	}
	if e = insertMessage(tx, out); e != nil {
		return e
	}
	_, e = tx.Exec(`UPDATE messages SET receipt_pending=0 WHERE sender_machine_id=? AND id=?`, m.SenderMachine, m.ID)
	return e
}

func (s *Store) FlushReceipts() error {
	all, e := s.selectMessages(`WHERE kind='message' AND receipt_pending=1 ORDER BY created_at,id LIMIT 256`)
	if e != nil {
		return e
	}
	for _, m := range all {
		tx, e := s.DB.Begin()
		if e != nil {
			return e
		}
		// A handoff may have advanced since the initial candidate scan. Select
		// the current obligation inside the same transaction that clears it.
		current, ce := scanMessage(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE sender_machine_id=? AND id=? AND receipt_pending=1`, m.SenderMachine, m.ID))
		if errors.Is(ce, sql.ErrNoRows) {
			tx.Rollback()
			continue
		}
		if ce != nil {
			tx.Rollback()
			return ce
		}
		e = s.receiptTx(tx, current, current.State, current.Failure)
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) HandoffRecorded(session string, h Handoff) bool {
	m, e := s.Message(h.SenderMachine, h.MessageID)
	if e != nil || m.AttemptID != h.AttemptID || m.AttachmentID != session {
		return false
	}
	wanted := h.Status
	if wanted == "retry" {
		wanted = "received"
	}
	return m.State == wanted
}
func (s *Store) failPendingTx(tx *sql.Tx, where string, args []any, code string) error {
	r, e := tx.Query(`SELECT `+msgCols+` FROM messages WHERE kind='message' AND state IN('queued','received') AND (`+where+`)`, args...)
	if e != nil {
		return e
	}
	var v []Message
	for r.Next() {
		m, e := scanMessage(r)
		if e != nil {
			r.Close()
			return e
		}
		v = append(v, m)
	}
	e = r.Err()
	r.Close()
	if e != nil {
		return e
	}
	for _, m := range v {
		if _, e = tx.Exec(`UPDATE messages SET state='undeliverable',failure_code=? WHERE sender_machine_id=? AND id=?`, code, m.SenderMachine, m.ID); e != nil {
			return e
		}
		if e = s.receiptTx(tx, m, "undeliverable", code); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) FailRevoked(peer string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.failPendingTx(tx, `sender_machine_id=? AND recipient_machine_id=?`, []any{peer, s.Identity.MachineID}, "grant_revoked"); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) PendingIncoming(limit int) ([]Message, error) {
	return s.selectMessages(`WHERE kind='message' AND recipient_machine_id=? AND state IN('queued','received') AND next_attempt_at<=? AND expires_at>? ORDER BY created_at,id LIMIT ?`, s.Identity.MachineID, Now(), Now(), limit)
}
func (s *Store) PendingOutgoing(limit int) ([]Message, error) {
	return s.selectMessages(`WHERE sender_machine_id=? AND recipient_machine_id<>? AND state IN('queued','forwarded') AND next_attempt_at<=? AND expires_at>? ORDER BY kind DESC,created_at,id LIMIT ?`, s.Identity.MachineID, s.Identity.MachineID, Now(), Now(), limit)
}
func (s *Store) selectMessages(tail string, args ...any) ([]Message, error) {
	return selectMessagesDB(s.DB, tail, args...)
}
func selectMessagesDB(db *sql.DB, tail string, args ...any) ([]Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, e := db.QueryContext(ctx, `SELECT `+msgCols+` FROM messages `+tail, args...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Message{}
	for r.Next() {
		m, e := scanMessage(r)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, r.Err()
}

func (s *Store) Claim(m Message, session string) (Message, error) {
	m.AttemptID = NewID("d_")
	m.AttachmentID = session
	r, e := s.DB.Exec(`UPDATE messages SET state='delivering',attempt_id=?,attachment_id=?,attempt_count=attempt_count+1 WHERE sender_machine_id=? AND id=? AND state IN('queued','received') AND expires_at>?`, m.AttemptID, session, m.SenderMachine, m.ID, Now())
	if e != nil {
		return m, e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return m, problem(409, "not_eligible", "message is no longer eligible")
	}
	m.State = "delivering"
	m.Attempts++
	return m, nil
}
func (s *Store) Finish(m Message, status, code string, next int64) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.Exec(`UPDATE messages SET state=?,failure_code=?,next_attempt_at=?,handed_off_at=CASE WHEN ?='handed_off' THEN ? ELSE handed_off_at END WHERE sender_machine_id=? AND id=? AND state='delivering' AND attempt_id=?`, status, code, next, status, Now(), m.SenderMachine, m.ID, m.AttemptID)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return problem(409, "stale_attempt", "handoff attempt is no longer current")
	}
	if e = s.receiptTx(tx, m, status, code); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) MarkIncomingFailure(m Message, code string) error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`UPDATE messages SET state='undeliverable',failure_code=? WHERE sender_machine_id=? AND id=? AND state IN('queued','received')`, code, m.SenderMachine, m.ID); e != nil {
		return e
	}
	if e = s.receiptTx(tx, m, "undeliverable", code); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) MarkTransport(m Message, state, code string, next int64, wire []byte) error {
	_, e := s.DB.Exec(`UPDATE messages SET state=?,failure_code=?,next_attempt_at=?,attempt_count=attempt_count+1,wire_envelope=COALESCE(?,wire_envelope),body=CASE WHEN ? IS NOT NULL THEN '' ELSE body END WHERE sender_machine_id=? AND id=? AND state IN('queued','forwarded')`, state, code, next, wire, wire, m.SenderMachine, m.ID)
	return e
}

func (s *Store) SaveWire(m Message, wire []byte) error {
	_, e := s.DB.Exec(`UPDATE messages SET wire_envelope=?,body='' WHERE sender_machine_id=? AND id=? AND state IN('queued','forwarded')`, wire, m.SenderMachine, m.ID)
	return e
}

func (s *Store) ApplyReceipt(peer string, r Receipt) error {
	if !validReceiptFailure(r.Failure) {
		return problem(400, "bad_receipt", "receipt failure reason must be a fixed protocol code")
	}
	if r.Status != "received" && r.Status != "handed_off" && r.Status != "undeliverable" && r.Status != "expired" && r.Status != "uncertain" {
		return problem(400, "bad_receipt", "unsupported receipt status")
	}
	m, e := s.Message(s.Identity.MachineID, r.OriginalID)
	if e != nil {
		return problem(404, "unknown_receipt", "receipt has no corresponding outgoing message")
	}
	if m.Kind != "message" || m.RecipientMachine != peer {
		return problem(403, "wrong_receipt_peer", "receipt sender is not the intended recipient")
	}
	if m.Terminal() || m.PrunedAt != nil {
		return nil
	}
	if r.Status == "received" && (m.State == "received" || m.State == "uncertain") {
		return nil
	}
	_, e = s.DB.Exec(`UPDATE messages SET state=?,failure_code=?,received_at=COALESCE(received_at,?),handed_off_at=CASE WHEN ?='handed_off' THEN ? ELSE handed_off_at END WHERE sender_machine_id=? AND id=?`, r.Status, r.Failure, Now(), r.Status, Now(), s.Identity.MachineID, r.OriginalID)
	return e
}
func (s *Store) Expire() error {
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	r, e := tx.Query(`SELECT `+msgCols+` FROM messages WHERE expires_at<=? AND state IN('queued','forwarded','received')`, Now())
	if e != nil {
		return e
	}
	var all []Message
	for r.Next() {
		m, e := scanMessage(r)
		if e != nil {
			r.Close()
			return e
		}
		all = append(all, m)
	}
	e = r.Err()
	r.Close()
	if e != nil {
		return e
	}
	for _, m := range all {
		if _, e = tx.Exec(`UPDATE messages SET state='expired',failure_code='expired' WHERE sender_machine_id=? AND id=?`, m.SenderMachine, m.ID); e != nil {
			return e
		}
		if e = s.receiptTx(tx, m, "expired", "expired"); e != nil {
			return e
		}
	}
	if _, e = tx.Exec(`DELETE FROM messages WHERE (pruned_at IS NOT NULL AND receipt_pending=0 OR kind='receipt') AND expires_at<=?`, Now()); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Prune(before int64) (int64, error) {
	r, e := s.DB.Exec(`UPDATE messages SET body='',wire_envelope=NULL,pruned_at=? WHERE kind='message' AND created_at<? AND pruned_at IS NULL AND state IN('handed_off','undeliverable','expired')`, Now(), before)
	if e != nil {
		return 0, e
	}
	return r.RowsAffected()
}

func (s *Store) History(agent string, limit int, cursor string, pending, remote bool) (HistoryPage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	args := []any{}
	q := `WHERE kind='message' AND pruned_at IS NULL`
	if agent != "" {
		q += ` AND ((sender_machine_id=? AND sender_agent_id=?) OR (recipient_machine_id=? AND recipient_agent_id=?))`
		args = append(args, s.Identity.MachineID, agent, s.Identity.MachineID, agent)
	}
	if pending {
		q += ` AND state IN('queued','received','delivering','uncertain') AND recipient_machine_id=?`
		args = append(args, s.Identity.MachineID)
	}
	if remote {
		q += ` AND sender_machine_id<>recipient_machine_id`
	}
	if cursor != "" {
		q += ` AND (created_at || ':' || sender_machine_id || ':' || id) > ?`
		args = append(args, cursor)
	}
	q += ` ORDER BY created_at,sender_machine_id,id LIMIT ?`
	args = append(args, limit+1)
	all, e := selectMessagesDB(s.ReadDB, q, args...)
	if e != nil {
		return HistoryPage{}, e
	}
	page := HistoryPage{Messages: []Message{}}
	used := 32
	for _, m := range all {
		if len(page.Messages) >= limit {
			break
		}
		if len(m.Wire) > 0 && m.Body == "" {
			peer, e := s.Peer(m.RecipientMachine)
			if e != nil {
				return page, e
			}
			var env Envelope
			if e = json.Unmarshal(m.Wire, &env); e != nil {
				return page, e
			}
			p, e := openPayload(s.Identity, peer, env, false)
			if e != nil {
				return page, e
			}
			m.Body = p.Body
		}
		m.Wire = nil
		m.Hash = ""
		size := len(marshal(m)) + 1
		if used+size > MaxHistory {
			break
		}
		used += size
		page.Messages = append(page.Messages, m)
	}
	if len(page.Messages) < len(all) && len(page.Messages) > 0 {
		m := page.Messages[len(page.Messages)-1]
		page.Cursor = fmt.Sprintf("%d:%s:%s", m.CreatedAt, m.SenderMachine, m.ID)
	}
	return page, nil
}
func (s *Store) Stats() (map[string]any, error) {
	var total, sent, received, bytes, failed, handed int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e := s.ReadDB.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(sender_machine_id=?),0),COALESCE(sum(recipient_machine_id=?),0),COALESCE(sum(length(CAST(body AS BLOB))+COALESCE(length(wire_envelope),0)),0),COALESCE(sum(state IN('undeliverable','expired')),0),COALESCE(sum(state='handed_off'),0) FROM messages WHERE kind='message' AND pruned_at IS NULL`, s.Identity.MachineID, s.Identity.MachineID).Scan(&total, &sent, &received, &bytes, &failed, &handed)
	return map[string]any{"messages": total, "sent": sent, "received": received, "stored_payload_bytes": bytes, "failed": failed, "handed_off": handed}, e
}

// Select only the oldest eligible message for each ready recipient. A backlog
// for an offline or stuck agent must not hide another recipient behind LIMIT.
func (s *Store) PendingForAgents(ids []string) ([]Message, error) {
	if len(ids) == 0 {
		return []Message{}, nil
	}
	marks := make([]string, len(ids))
	args := []any{s.Identity.MachineID, Now(), Now()}
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id)
	}
	return s.selectMessages(`WHERE rowid IN (SELECT rowid FROM (SELECT rowid,ROW_NUMBER() OVER(PARTITION BY recipient_agent_id ORDER BY created_at,id) AS position FROM messages WHERE kind='message' AND recipient_machine_id=? AND state IN('queued','received') AND next_attempt_at<=? AND expires_at>? AND recipient_agent_id IN (`+strings.Join(marks, ",")+`)) WHERE position=1)`, args...)
}

// Operator recovery is explicit. A late receiver confirmation must identify the
// exact original attachment+attempt; it cannot override a newer replay attempt.
func (s *Store) ResolveUncertain(sender, id, status, attachment, attempt string) error {
	if status != "handed_off" && status != "retry" && status != "undeliverable" {
		return problem(400, "bad_resolution", "choose handed_off, retry or undeliverable")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	m, e := scanMessage(tx.QueryRow(`SELECT `+msgCols+` FROM messages WHERE sender_machine_id=? AND id=?`, sender, id))
	if e != nil {
		return e
	}
	if attempt != "" && (m.AttemptID != attempt || m.AttachmentID != attachment) {
		return problem(409, "stale_attempt", "attempt has been replaced")
	}
	if m.State == status && attempt != "" {
		return nil
	}
	if m.State != "uncertain" {
		return problem(409, "not_uncertain", "only uncertain handoffs may be resolved")
	}
	if m.RecipientMachine != s.Identity.MachineID {
		return problem(403, "remote_resolution", "resolve at the receiving node")
	}
	nextState := status
	code := "operator_confirmed"
	var at any
	if status == "retry" {
		if m.ExpiresAt <= Now() {
			return problem(410, "expired", "expired messages cannot be retried")
		}
		nextState = "received"
		code = ""
	}
	if status == "undeliverable" {
		code = "operator_cancelled"
	}
	if status == "handed_off" {
		at = Now()
	}
	_, e = tx.Exec(`UPDATE messages SET state=?,failure_code=?,handed_off_at=COALESCE(?,handed_off_at),next_attempt_at=0,attempt_id=CASE WHEN ?='received' THEN '' ELSE attempt_id END WHERE sender_machine_id=? AND id=?`, nextState, code, at, nextState, sender, id)
	if e != nil {
		return e
	}
	if status != "retry" {
		if e = s.receiptTx(tx, m, nextState, code); e != nil {
			return e
		}
	}
	return tx.Commit()
}
