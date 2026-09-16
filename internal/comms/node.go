package comms

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/joaohts/comms/internal/codex"
	"golang.org/x/sys/unix"
)

type receiver struct {
	session Session
	events  chan Event
	ctx     context.Context
	cancel  context.CancelFunc
}
type handoffWait struct {
	session string
	message Message
	result  chan Handoff
}
type Node struct {
	Store             *Store
	cfg               Config
	ctx               context.Context
	cancel            context.CancelFunc
	server            *http.Server
	brokerServer      *http.Server
	broker            *Broker
	lock              *os.File
	wake              chan struct{}
	outWake           chan struct{}
	syncWake          chan struct{}
	jobs              chan Message
	mu                sync.Mutex
	receivers         map[string]*receiver
	busy              map[string]bool
	waits             map[string]*handoffWait
	observers         map[chan Event]struct{}
	workers           sync.WaitGroup
	background        sync.WaitGroup
	native            sync.WaitGroup
	nativeStarting    map[string]bool
	closing           atomic.Bool
	bcMu              sync.Mutex
	brokerURL         string
	brokerToken       string
	brokerTokenExpiry int64
	streamCancel      context.CancelFunc
	brokerOnline      atomic.Bool
	httpClient        *http.Client
	querySlots        chan struct{}
}

func NewNode(cfg Config) (*Node, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultConfig().DataDir
	}
	abs, absErr := filepath.Abs(cfg.DataDir)
	if absErr != nil {
		return nil, absErr
	}
	cfg.DataDir = abs
	if err := loadServiceKey(&cfg); err != nil {
		return nil, err
	}
	if cfg.Heartbeat <= 0 || cfg.Lease < cfg.Heartbeat || cfg.Drain < 0 || cfg.LocalCount < 1 || cfg.LocalBytes < 1 || cfg.ReceiptCount < 1 || cfg.ReceiptBytes < 1 {
		return nil, fmt.Errorf("invalid heartbeat, lease, drain or queue configuration")
	}
	if cfg.Workers < 1 || cfg.Workers > 64 {
		return nil, fmt.Errorf("workers must be between 1 and 64")
	}
	if e := os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return nil, e
	}
	if e := os.Chmod(cfg.DataDir, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(cfg.DataDir, "node.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("node already running for %s", cfg.DataDir)
	}
	s, e := OpenStore(cfg)
	if e != nil {
		f.Close()
		return nil, e
	}
	n := &Node{Store: s, cfg: cfg, lock: f, wake: make(chan struct{}, 1), outWake: make(chan struct{}, 1), syncWake: make(chan struct{}, 1), jobs: make(chan Message, cfg.Workers), receivers: map[string]*receiver{}, busy: map[string]bool{}, waits: map[string]*handoffWait{}, observers: map[chan Event]struct{}{}, querySlots: make(chan struct{}, 2), httpClient: &http.Client{Timeout: 15 * time.Second, CheckRedirect: noBrokerRedirect}}
	n.brokerURL = s.Setting("broker_url")
	if s.Setting("name") == "" {
		name, _ := os.Hostname()
		if !validAlias(name) {
			name = "local"
		}
		s.SetSetting("name", name)
	}
	if cfg.BrokerListen != "" {
		n.broker, e = NewBroker(cfg)
		if e != nil {
			s.Close()
			f.Close()
			return nil, e
		}
	}
	return n, nil
}
func (n *Node) Start(parent context.Context) error {
	n.ctx, n.cancel = context.WithCancel(parent)
	socket := filepath.Join(n.cfg.DataDir, "node.sock")
	if e := os.Remove(socket); e != nil && !os.IsNotExist(e) {
		return e
	}
	l, e := net.Listen("unix", socket)
	if e != nil {
		return e
	}
	if e = os.Chmod(socket, 0600); e != nil {
		l.Close()
		return e
	}
	n.server = &http.Server{Handler: n.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		if e := n.server.Serve(l); e != nil && e != http.ErrServerClosed {
			log.Printf("local server: %v", e)
		}
	}()
	if n.broker != nil {
		bl, e := net.Listen("tcp", n.cfg.BrokerListen)
		if e != nil {
			n.server.Close()
			return e
		}
		handler := n.broker.Handler()
		if n.cfg.LegacyURL != "" {
			u, e := url.Parse(n.cfg.LegacyURL)
			if e != nil {
				bl.Close()
				n.server.Close()
				return e
			}
			proxy := httputil.NewSingleHostReverseProxy(u)
			director := proxy.Director
			proxy.Director = func(r *http.Request) { director(r); r.Header.Del(ServiceKeyHeader) }
			proxy.FlushInterval = -1
			primary := handler
			handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/v1/") {
					primary.ServeHTTP(w, r)
				} else {
					proxy.ServeHTTP(w, r)
				}
			})
			handler = n.broker.serviceKeyGate(handler)
		}
		n.brokerServer = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
		go func() {
			if e := n.brokerServer.Serve(bl); e != nil && e != http.ErrServerClosed {
				log.Printf("broker server: %v", e)
			}
		}()
	}
	for i := 0; i < n.cfg.Workers; i++ {
		n.workers.Add(1)
		go n.deliveryWorker()
	}
	n.background.Add(3)
	go func() { defer n.background.Done(); n.scheduler() }()
	go func() { defer n.background.Done(); n.maintenance() }()
	go func() { defer n.background.Done(); n.brokerLoop() }()
	n.notify()
	return nil
}
func (n *Node) Close(ctx context.Context) error {
	if !n.closing.CompareAndSwap(false, true) {
		return nil
	}
	// Stop new claims while already running handoffs get a bounded drain window.
	deadline := time.NewTimer(n.cfg.Drain)
	defer deadline.Stop()
	for {
		n.mu.Lock()
		count := len(n.busy)
		n.mu.Unlock()
		if count == 0 {
			break
		}
		select {
		case <-deadline.C:
			goto drained
		case <-ctx.Done():
			goto drained
		case <-time.After(20 * time.Millisecond):
		}
	}
drained:
	if n.cancel != nil {
		n.cancel()
	}
	n.mu.Lock()
	for _, r := range n.receivers {
		r.cancel()
	}
	n.mu.Unlock()
	n.workers.Wait()
	n.background.Wait()
	n.native.Wait()
	if n.server != nil {
		n.server.Close()
	}
	if n.brokerServer != nil {
		n.brokerServer.Close()
	}
	if n.broker != nil {
		n.broker.Close()
	}
	os.Remove(filepath.Join(n.cfg.DataDir, "node.sock"))
	e := n.Store.Close()
	n.lock.Close()
	return e
}
func (n *Node) notify() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
	select {
	case n.outWake <- struct{}{}:
	default:
	}
}
func (n *Node) requestSync() {
	select {
	case n.syncWake <- struct{}{}:
	default:
	}
}
func (n *Node) publish(kind string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for c := range n.observers {
		select {
		case c <- Event{Type: kind}:
		default:
			delete(n.observers, c)
			close(c)
		}
	}
}
func (n *Node) ready(agent string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := n.receivers[agent]
	return r != nil && r.ctx.Err() == nil
}

func nodeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func nodeError(w http.ResponseWriter, e error) {
	var a *APIError
	if errors.As(e, &a) {
		nodeJSON(w, a.Status, a)
		return
	}
	if errors.Is(e, sql.ErrNoRows) {
		nodeJSON(w, 404, &APIError{Code: "not_found", ErrorText: "record not found"})
		return
	}
	log.Printf("request error: %v", e)
	nodeJSON(w, 500, &APIError{Code: "internal_error", ErrorText: "operation failed"})
}
func nodeDecode(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2*MaxHistory)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return problem(400, "bad_json", "invalid JSON request")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return problem(400, "bad_json", "one JSON value required")
	}
	return nil
}

func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		nodeJSON(w, 200, map[string]any{"version": Version, "api_version": ProtocolVersion, "machine_id": n.Store.Identity.MachineID, "public_key": n.Store.Identity.PublicKey, "identity": n.Store.Identity, "name": n.Store.Setting("name"), "broker_enabled": n.broker != nil, "broker_connected": n.brokerOnline.Load(), "broker_service_key_configured": n.cfg.BrokerServiceKey != "", "broker_url": n.Store.Setting("broker_url"), "data_dir": n.cfg.DataDir})
	})
	mux.HandleFunc("PUT /v1/name", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Name string `json:"name"`
		}
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		if !validAlias(q.Name) {
			nodeError(w, problem(400, "bad_alias", "invalid machine name"))
			return
		}
		if e := n.Store.SetSetting("name", q.Name); e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, map[string]string{"name": q.Name})
		n.publish("identity")
	})
	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		v, e := n.Store.Agents(r.URL.Query().Get("retired") == "true")
		if e != nil {
			nodeError(w, e)
			return
		}
		out := []Agent{}
		for _, a := range v {
			a.Online = n.ready(a.ID)
			if r.URL.Query().Get("persistent") != "true" || a.Persistent {
				out = append(out, a)
			}
		}
		nodeJSON(w, 200, out)
	})
	mux.HandleFunc("DELETE /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, e := n.Store.Agent(r.PathValue("id"))
		if e == nil {
			n.dropReceiver(a.ID)
			e = n.Store.RetireAgent(a.ID)
		}
		if e != nil {
			nodeError(w, e)
			return
		}
		n.notify()
		n.requestSync()
		n.publish("agents")
		nodeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		v, e := n.Store.Sessions()
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var q OpenRequest
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		if q.Harness == "codex" {
			if e := codex.ValidateSession(r.Context(), q.Target, q.HarnessID); e != nil {
				nodeError(w, problem(412, "receiver_setup_required", e.Error()))
				return
			}
		}
		if q.Takeover {
			if a, e := n.Store.Agent(q.Alias); e == nil {
				n.dropReceiver(a.ID)
			}
		}
		v, e := n.Store.OpenAgent(q)
		if e != nil {
			nodeError(w, e)
			return
		}
		n.startNative(v.Session)
		n.notify()
		if q.Scope == "global" || q.Persistent {
			n.requestSync()
		}
		n.publish("agents")
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/sessions/{id}/lease", func(w http.ResponseWriter, r *http.Request) {
		if e := n.Store.Renew(r.PathValue("id")); e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, map[string]int64{"lease_expires_at": Now() + n.cfg.Lease.Milliseconds()})
		n.notify()
	})
	mux.HandleFunc("DELETE /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s, e := n.Store.Session(id)
		if e == nil {
			n.dropReceiver(s.AgentID)
			e = n.Store.CloseSession(id)
		}
		if e != nil {
			nodeError(w, e)
			return
		}
		n.notify()
		n.requestSync()
		n.publish("agents")
		nodeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/sessions/{id}/stream", n.stream)
	mux.HandleFunc("POST /v1/sessions/{id}/handoffs", n.handoff)
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var q SendRequest
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		m, e := n.Send(r.Context(), q)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 202, m)
	})
	mux.HandleFunc("POST /v1/messages/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Status string `json:"status"`
			Sender string `json:"sender_machine_id"`
		}
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		m, e := n.Store.FindMessage(r.PathValue("id"))
		if q.Sender != "" {
			m, e = n.Store.Message(q.Sender, r.PathValue("id"))
		}
		if e != nil {
			nodeError(w, e)
			return
		}
		if e = n.Store.ResolveUncertain(m.SenderMachine, m.ID, q.Status, "", ""); e != nil {
			nodeError(w, e)
			return
		}
		n.notify()
		n.publish("messages")
		m, e = n.Store.Message(m.SenderMachine, m.ID)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, m)
	})
	mux.HandleFunc("GET /v1/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		m, e := n.Store.FindMessage(r.PathValue("id"))
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, m)
	})
	mux.HandleFunc("GET /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		select {
		case n.querySlots <- struct{}{}:
			defer func() { <-n.querySlots }()
		default:
			nodeError(w, problem(429, "query_busy", "history query capacity is busy"))
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		agent := q.Get("agent_id")
		if agent != "" {
			a, e := n.Store.Agent(agent)
			if e != nil {
				nodeError(w, e)
				return
			}
			agent = a.ID
		}
		page, e := n.Store.History(agent, limit, q.Get("cursor"), q.Get("pending") == "true", false)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, page)
	})
	mux.HandleFunc("POST /v1/prune", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Before int64 `json:"before"`
		}
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		v, e := n.Store.Prune(q.Before)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, map[string]int64{"pruned": v})
	})
	mux.HandleFunc("GET /v1/peers", func(w http.ResponseWriter, r *http.Request) {
		v, e := n.Store.Peers()
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/peers/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p Peer
		if e := nodeDecode(w, r, &p); e != nil {
			nodeError(w, e)
			return
		}
		if p.MachineID == "" {
			p.MachineID = r.PathValue("id")
		}
		if p.MachineID != r.PathValue("id") {
			nodeError(w, problem(400, "identity_mismatch", "peer ID differs from path"))
			return
		}
		if e := n.Store.PutPeer(p); e != nil {
			nodeError(w, e)
			return
		}
		n.notify()
		nodeJSON(w, 200, p)
	})
	mux.HandleFunc("GET /v1/grants", func(w http.ResponseWriter, r *http.Request) {
		v, e := n.Store.Grants()
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/grants/{id}", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Messages bool `json:"allow_messages"`
			History  bool `json:"allow_history"`
		}
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		g, e := n.Store.SetGrant(r.PathValue("id"), q.Messages, q.History)
		if e == nil && !g.Messages {
			e = n.Store.FailRevoked(g.Grantee)
		}
		if e != nil {
			nodeError(w, e)
			return
		}
		n.notify()
		n.requestSync()
		n.publish("grants")
		nodeJSON(w, 200, g)
	})
	mux.HandleFunc("GET /v1/who", func(w http.ResponseWriter, r *http.Request) {
		v, e := n.Who(r.Context())
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, r *http.Request) {
		select {
		case n.querySlots <- struct{}{}:
			defer func() { <-n.querySlots }()
		default:
			nodeError(w, problem(429, "query_busy", "history/stat query capacity is busy"))
			return
		}
		v, e := n.Store.Stats()
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/broker", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			URL string `json:"url"`
		}
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		if e := n.ConfigureBroker(r.Context(), q.URL); e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /v1/events", n.events)
	mux.HandleFunc("POST /v1/history", func(w http.ResponseWriter, r *http.Request) {
		select {
		case n.querySlots <- struct{}{}:
			defer func() { <-n.querySlots }()
		default:
			nodeError(w, problem(429, "query_busy", "history query capacity is busy"))
			return
		}
		var q HistoryRequest
		if e := nodeDecode(w, r, &q); e != nil {
			nodeError(w, e)
			return
		}
		page, e := n.History(r.Context(), q)
		if e != nil {
			nodeError(w, e)
			return
		}
		nodeJSON(w, 200, page)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.closing.Load() && r.Method != "GET" && !strings.HasSuffix(r.URL.Path, "/handoffs") {
			nodeError(w, problem(503, "draining", "node is draining; retry after reconnect"))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (n *Node) dropReceiver(agent string) {
	n.mu.Lock()
	if r := n.receivers[agent]; r != nil {
		r.cancel()
		delete(n.receivers, agent)
	}
	n.mu.Unlock()
}
func (n *Node) stream(w http.ResponseWriter, r *http.Request) {
	s, e := n.Store.Session(r.PathValue("id"))
	if e != nil || s.EndedAt != nil {
		nodeError(w, problem(410, "attachment_ended", "attachment ended"))
		return
	}
	if s.Harness == "codex" {
		nodeError(w, problem(409, "native_receiver", "Codex delivery uses its native tool-output receiver"))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	recv := &receiver{s, make(chan Event, 1), ctx, cancel}
	n.mu.Lock()
	if old := n.receivers[s.AgentID]; old != nil {
		old.cancel()
	}
	n.receivers[s.AgentID] = recv
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		if n.receivers[s.AgentID] == recv {
			delete(n.receivers, s.AgentID)
			n.Store.Disconnect(s.ID)
		}
		n.mu.Unlock()
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
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	controller := http.NewResponseController(w)
	controller.Flush()
	ping := time.NewTicker(n.cfg.Heartbeat)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.ctx.Done():
			return
		case ev := <-recv.events:
			controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if e := json.NewEncoder(w).Encode(ev); e != nil {
				return
			}
			if e := controller.Flush(); e != nil {
				return
			}
			controller.SetWriteDeadline(time.Time{})
		case <-ping.C:
			current, e := n.Store.Session(s.ID)
			if e != nil || current.EndedAt != nil {
				return
			}
			controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if e := json.NewEncoder(w).Encode(Event{Type: "heartbeat"}); e != nil {
				return
			}
			if e := controller.Flush(); e != nil {
				return
			}
			controller.SetWriteDeadline(time.Time{})
		}
	}
}
func (n *Node) handoff(w http.ResponseWriter, r *http.Request) {
	var h Handoff
	if e := nodeDecode(w, r, &h); e != nil {
		nodeError(w, e)
		return
	}
	if h.Status != "handed_off" && h.Status != "retry" && h.Status != "uncertain" {
		nodeError(w, problem(400, "bad_handoff", "invalid handoff outcome"))
		return
	}
	if !validReceiptFailure(h.Failure) {
		h.Failure = "adapter_error"
	}
	if e := n.recordHandoff(r.PathValue("id"), h); e != nil {
		nodeError(w, e)
		return
	}
	nodeJSON(w, 200, map[string]bool{"ok": true})
}
func (n *Node) events(w http.ResponseWriter, r *http.Request) {
	c := make(chan Event, 32)
	n.mu.Lock()
	n.observers[c] = struct{}{}
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.observers, c); n.mu.Unlock() }()
	w.Header().Set("Content-Type", "application/x-ndjson")
	controller := http.NewResponseController(w)
	controller.Flush()
	ping := time.NewTicker(n.cfg.Heartbeat)
	defer ping.Stop()
	for {
		var ev Event
		select {
		case <-r.Context().Done():
			return
		case <-n.ctx.Done():
			return
		case v, ok := <-c:
			if !ok {
				return
			}
			ev = v
		case <-ping.C:
			ev = Event{Type: "heartbeat"}
		}
		controller.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if e := json.NewEncoder(w).Encode(ev); e != nil {
			return
		}
		if e := controller.Flush(); e != nil {
			return
		}
	}
}

func (n *Node) Send(ctx context.Context, q SendRequest) (Message, error) {
	var zero Message
	if q.To == "" {
		return zero, problem(400, "missing_recipient", "--to is required; there is no implicit broadcast")
	}
	if len([]byte(q.Body)) > MaxBody {
		return zero, problem(413, "message_too_large", "message body exceeds 64 KiB")
	}
	s, e := n.Store.Session(q.SessionID)
	if e != nil || s.EndedAt != nil {
		return zero, problem(403, "no_attachment", "sender requires a current attachment")
	}
	if q.ID == "" {
		q.ID = NewID("msg_")
	}
	if len(q.ID) > 80 || !validWireID(q.ID) {
		return zero, problem(400, "bad_id", "message ID too long")
	}
	hash := digest(marshal([]string{s.AgentID, q.To, q.Body}))
	if old, e := n.Store.Message(n.Store.Identity.MachineID, q.ID); e == nil {
		if old.Hash != hash {
			return zero, problem(409, "id_conflict", "ID belongs to a different send request")
		}
		return old, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return zero, e
	}
	machine, agent, e := n.resolve(ctx, q.To, s.Scope == "global")
	if e != nil {
		return zero, e
	}
	m := Message{ID: q.ID, SenderMachine: n.Store.Identity.MachineID, SenderAgent: s.AgentID, RecipientMachine: machine, RecipientAgent: agent, Kind: "message", Body: q.Body, State: "queued", CreatedAt: Now(), ExpiresAt: time.Now().Add(MessageTTL).UnixMilli(), Hash: hash}
	if machine != n.Store.Identity.MachineID {
		if s.Scope != "global" {
			return zero, problem(403, "local_only", "local attachment cannot send remotely")
		}
		peer, e := n.Store.Peer(machine)
		if e != nil {
			return zero, e
		}
		env, e := Encrypt(n.Store.Identity, peer, payloadFor(m))
		if e != nil {
			return zero, e
		}
		m.Wire = marshal(env)
		m.Body = ""
	} else {
		a, e := n.Store.Agent(agent)
		if e != nil || a.RetiredAt != nil {
			return zero, problem(410, "agent_ended", "recipient identity is retired")
		}
		now := Now()
		m.ReceivedAt = &now
		m.State = "received"
	}
	m, _, e = n.Store.Insert(m, false)
	if e == nil {
		n.notify()
		n.publish("messages")
	}
	return m, e
}
func payloadFor(m Message) Payload {
	p := Payload{Routing: Routing{ProtocolVersion, m.ID, m.SenderMachine, m.RecipientMachine, m.Kind, m.ExpiresAt}, SenderAgent: m.SenderAgent, RecipientAgent: m.RecipientAgent, CreatedAt: m.CreatedAt, Body: m.Body}
	if m.Kind == "receipt" {
		var r Receipt
		json.Unmarshal([]byte(m.Body), &r)
		p.Receipt = &r
		p.Body = ""
	}
	return p
}
func (n *Node) resolve(ctx context.Context, ref string, remoteAllowed bool) (string, string, error) {
	parts := strings.SplitN(ref, ":", 2)
	if len(parts) == 1 || parts[0] == "self" || parts[0] == n.Store.Setting("name") || parts[0] == n.Store.Identity.MachineID {
		aRef := parts[len(parts)-1]
		a, e := n.Store.Agent(aRef)
		if e != nil {
			return "", "", problem(404, "unknown_agent", "local agent not found")
		}
		if a.RetiredAt != nil {
			return "", "", problem(410, "agent_ended", "agent identity has ended")
		}
		return n.Store.Identity.MachineID, a.ID, nil
	}
	if !remoteAllowed {
		return "", "", problem(403, "local_only", "local attachment cannot contact another machine")
	}
	p, e := n.Store.Peer(parts[0])
	if e != nil {
		return "", "", problem(404, "unknown_peer", "peer has not been paired")
	}
	if agentIDPattern.MatchString(parts[1]) {
		return p.MachineID, parts[1], nil
	}
	var all []Presence
	if e = n.brokerRequest(ctx, "GET", "/v1/who", nil, &all); e != nil {
		return "", "", e
	}
	for _, v := range all {
		if v.MachineID == p.MachineID && v.Alias == parts[1] {
			return p.MachineID, v.AgentID, nil
		}
	}
	return "", "", problem(404, "unknown_agent", "remote agent is not discoverable; check its global scope and grants")
}
func (n *Node) Who(ctx context.Context) ([]Presence, error) {
	a, e := n.Store.Agents(false)
	if e != nil {
		return nil, e
	}
	out := []Presence{}
	for _, v := range a {
		online := n.ready(v.ID)
		if online || v.Persistent {
			out = append(out, Presence{n.Store.Identity.MachineID, n.Store.Setting("name"), v.ID, v.Alias, v.Persistent, online, 0})
		}
	}
	if n.currentBrokerURL() == "" {
		return out, nil
	}
	var remote []Presence
	if e = n.brokerRequest(ctx, "GET", "/v1/who", nil, &remote); e != nil {
		return out, nil
	}
	for _, v := range remote {
		if v.MachineID == n.Store.Identity.MachineID {
			continue
		}
		if p, e := n.Store.Peer(v.MachineID); e == nil {
			v.PeerAlias = p.Alias
			out = append(out, v)
		}
	}
	return out, nil
}

func (n *Node) scheduler() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	last := ""
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.wake:
		case <-ticker.C:
		}
		if n.closing.Load() {
			continue
		}
		n.mu.Lock()
		ids := []string{}
		for id, r := range n.receivers {
			if !n.busy[id] && r.ctx.Err() == nil {
				ids = append(ids, id)
			}
		}
		n.mu.Unlock()
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		start := sort.SearchStrings(ids, last)
		for start < len(ids) && ids[start] <= last {
			start++
		}
		if start >= len(ids) {
			start = 0
		}
		ids = append(ids[start:], ids[:start]...)
		all, e := n.Store.PendingForAgents(ids)
		if e != nil {
			log.Printf("scheduler: %v", e)
			continue
		}
		byAgent := map[string]Message{}
		for _, m := range all {
			byAgent[m.RecipientAgent] = m
		}
		for _, id := range ids {
			m, exists := byAgent[id]
			if !exists {
				continue
			}
			n.mu.Lock()
			r := n.receivers[id]
			if r == nil || n.busy[id] {
				n.mu.Unlock()
				continue
			}
			n.busy[id] = true
			n.mu.Unlock()
			select {
			case n.jobs <- m:
				last = id
			default:
				n.mu.Lock()
				delete(n.busy, id)
				n.mu.Unlock()
			}
		}
	}
}
func retryAt(attempt int) int64 {
	if attempt > 5 {
		attempt = 5
	}
	delay := time.Second * time.Duration(1<<attempt)
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Now().Add(delay*4/5 + time.Duration(rand.Int64N(int64(delay/5)+1))).UnixMilli()
}
func (n *Node) deliveryWorker() {
	defer n.workers.Done()
	for {
		select {
		case <-n.ctx.Done():
			return
		case m := <-n.jobs:
			n.deliver(m)
			n.mu.Lock()
			delete(n.busy, m.RecipientAgent)
			n.mu.Unlock()
			n.notify()
		}
	}
}
func (n *Node) deliver(m Message) {
	a, e := n.Store.Agent(m.RecipientAgent)
	if e != nil || a.RetiredAt != nil {
		n.Store.MarkIncomingFailure(m, "agent_ended")
		return
	}
	s, e := n.Store.ActiveSession(a.ID)
	if e != nil || s.LeaseExpiresAt <= Now() {
		return
	}
	if m.SenderMachine != n.Store.Identity.MachineID {
		g, e := n.Store.Grant(m.SenderMachine)
		if e != nil || !g.Messages {
			n.Store.MarkIncomingFailure(m, "grant_revoked")
			return
		}
		if s.Scope != "global" {
			n.Store.MarkIncomingFailure(m, "local_only")
			return
		}
	}
	n.mu.Lock()
	r := n.receivers[a.ID]
	n.mu.Unlock()
	if r == nil || r.session.ID != s.ID || r.ctx.Err() != nil {
		return
	}
	m, e = n.Store.Claim(m, s.ID)
	if e != nil {
		return
	}
	wait := &handoffWait{s.ID, m, make(chan Handoff, 1)}
	n.mu.Lock()
	n.waits[m.AttemptID] = wait
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.waits, m.AttemptID); n.mu.Unlock() }()
	select {
	case r.events <- Event{Type: "message", Message: &m}:
	case <-r.ctx.Done():
		n.Store.Finish(m, "received", "receiver_disconnected", retryAt(max(0, m.Attempts-1)))
		return
	case <-n.ctx.Done():
		n.Store.Finish(m, "received", "node_stopping", 0)
		return
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	var status, code string
	var next int64
	select {
	case <-wait.result:
		// The HTTP handler committed the outcome and receipt obligation before
		// acknowledging the receiver. Do not perform a second state transition.
		return
	case <-r.ctx.Done():
		status = "uncertain"
		code = "receiver_disconnected_during_handoff"
	case <-n.ctx.Done():
		status = "uncertain"
		code = "node_stopped_during_handoff"
	case <-timer.C:
		status = "uncertain"
		code = "handoff_timeout"
		r.cancel()
	}
	if e = n.Store.Finish(m, status, code, next); e != nil {
		log.Printf("handoff persist: %v", e)
	}
	n.publish("messages")
}
func (n *Node) maintenance() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-tick.C:
		}
		if time.Since(last) >= 15*time.Second {
			if e := n.Store.Expire(); e != nil {
				log.Printf("expiry: %v", e)
			}
			last = time.Now()
		}
		sessions, e := n.Store.Sessions()
		if e != nil {
			continue
		}
		for _, s := range sessions {
			n.startNative(s)
			if s.ProcessID > 0 && s.ProcessStarted != "" && confirmedProcessEnded(s.ProcessID, s.ProcessStarted) {
				n.dropReceiver(s.AgentID)
				if e := n.Store.CloseSession(s.ID); e != nil {
					log.Printf("session end: %v", e)
				}
				n.notify()
				n.publish("agents")
			} else if s.LeaseExpiresAt > 0 && s.LeaseExpiresAt <= Now() {
				n.dropReceiver(s.AgentID)
			}
		}
	}
}
