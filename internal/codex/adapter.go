// Package codex delivers external peer content through Codex app-server's
// native tool-output interface. It never sends user input or controls a terminal.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Outcome describes submission to the harness, not model understanding or work.
type Outcome string

const (
	NotSent   Outcome = "not_sent"
	Uncertain Outcome = "uncertain"
	Accepted  Outcome = "accepted"

	// Payload JSON can expand a 64 KiB message body through JSON escaping.
	MaxPayloadBytes  = 512 << 10
	MaxRequestBytes  = 768 << 10
	MaxResponseBytes = 1 << 20
	DefaultTimeout   = 10 * time.Second
)

type Result struct {
	Outcome Outcome `json:"outcome"`
	TurnID  string  `json:"turn_id,omitempty"`
}

// RPCError omits server error data: it may contain private thread content.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("Codex app-server error %d: %s", e.Code, e.Message)
}

// ValidateTarget checks an explicit, private, same-user Unix socket. It does not
// start an app-server, create/resume a thread, or change filesystem permissions.
func ValidateTarget(target string) error {
	_, err := socketPath(target)
	return err
}

// ValidateServer checks the private listener and JSON-RPC initialization without
// starting or resuming a thread. Launchers use it before attaching a terminal.
func ValidateServer(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	c, err := connect(ctx, target)
	if err != nil {
		return err
	}
	c.close()
	return nil
}

func socketPath(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || !filepath.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) {
		return "", errors.New("Codex target must be unix:///absolute/path/to/control.sock")
	}
	path := filepath.Clean(u.Path)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("Codex control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", errors.New("Codex target is not a Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("Codex control socket must belong to the current OS user")
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", errors.New("Codex control socket must not be accessible to other users (expected mode 0600)")
	}
	// A private parent prevents replacement of the path by another OS user.
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	pstat, ok := parent.Sys().(*syscall.Stat_t)
	if !parent.IsDir() || !ok || pstat.Uid != uint32(os.Geteuid()) || parent.Mode().Perm()&0022 != 0 {
		return "", errors.New("Codex control socket parent must be owned by the current user and not writable by other users")
	}
	return path, nil
}

func validThreadID(threadID string) error {
	id, err := uuid.Parse(threadID)
	if err != nil || id == uuid.Nil || id.String() != threadID {
		return errors.New("Codex delivery requires an exact canonical thread UUID, not a name or alias")
	}
	return nil
}

// ValidateSession proves the exact thread is loaded and can accept input in
// this app-server. It only reads metadata; stored but unloaded threads fail.
func ValidateSession(ctx context.Context, target, threadID string) error {
	if err := validThreadID(threadID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	c, err := connect(ctx, target)
	if err != nil {
		return err
	}
	defer c.close()
	return c.validateSession(ctx, threadID)
}

// Deliver submits already-verified provenance and message JSON as tool output.
// The caller controls retry policy and MUST NOT automatically retry Uncertain.
// Accepted means app-server acknowledged turn/start; it does not mean the model
// read, understood, or completed the request. No model/policy overrides are sent.
func Deliver(ctx context.Context, target, threadID string, payload []byte) (Result, error) {
	result := Result{Outcome: NotSent}
	if err := validThreadID(threadID); err != nil {
		return result, err
	}
	if len(payload) == 0 || len(payload) > MaxPayloadBytes || !utf8.Valid(payload) || !json.Valid(payload) {
		return result, errors.New("Codex peer payload must be valid UTF-8 JSON within 512 KiB")
	}
	params := map[string]any{
		"threadId":   threadID,
		"input":      []any{},
		"toolOutput": map[string]any{"name": "comms_receive", "output": string(payload)},
	}
	request, err := json.Marshal(map[string]any{"id": 3, "method": "turn/start", "params": params})
	if err != nil || len(request) > MaxRequestBytes {
		return result, errors.New("Codex tool-output request exceeds 768 KiB")
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	c, err := connect(ctx, target)
	if err != nil {
		return result, err
	}
	defer c.close()
	if err := c.validateSession(ctx, threadID); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// From the first attempted write, a disconnect can hide a committed request.
	result.Outcome = Uncertain
	if err := c.ws.Write(ctx, websocket.MessageText, request); err != nil {
		return result, fmt.Errorf("Codex tool-output write: %w", err)
	}
	raw, err := c.response(ctx, 3)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			switch rpcErr.Code {
			case -32600, -32601, -32602, -32001:
				// Invalid request/method/params and ingress overload reject before
				// admission. Unknown/internal errors might follow a side effect.
				result.Outcome = NotSent
			}
		}
		return result, err
	}
	var reply struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || validThreadID(reply.Turn.ID) != nil {
		return result, errors.New("Codex returned an invalid turn acknowledgment")
	}
	result.TurnID = reply.Turn.ID
	switch reply.Turn.Status {
	case "inProgress", "completed":
		result.Outcome = Accepted
		return result, nil
	default:
		return result, fmt.Errorf("Codex returned ambiguous turn status %q", reply.Turn.Status)
	}
}

type connection struct {
	ws        *websocket.Conn
	transport *http.Transport
}

func connect(ctx context.Context, target string) (*connection, error) {
	path, err := socketPath(target)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		// No TCP listener or proxy is consulted, even when environment proxy
		// variables are set. The apparent HTTP host is only for WS framing.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialer.DialContext(ctx, "unix", path) },
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ws, response, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("connect Codex app-server: %w", err)
	}
	ws.SetReadLimit(MaxResponseBytes)
	c := &connection{ws: ws, transport: transport}
	if _, err := c.rpc(ctx, 1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "comms", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		c.close()
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"method":"initialized"}`)); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *connection) close() { c.ws.CloseNow(); c.transport.CloseIdleConnections() }

func (c *connection) validateSession(ctx context.Context, threadID string) error {
	raw, err := c.rpc(ctx, 2, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return err
	}
	var reply struct {
		Thread struct {
			ID        string `json:"id"`
			CanAccept *bool  `json:"canAcceptDirectInput"`
			Status    struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return fmt.Errorf("invalid Codex thread metadata: %w", err)
	}
	if reply.Thread.ID != threadID {
		return errors.New("Codex returned a different thread identity")
	}
	if reply.Thread.CanAccept == nil || !*reply.Thread.CanAccept {
		return errors.New("Codex thread cannot accept direct tool output")
	}
	switch reply.Thread.Status.Type {
	case "idle", "active":
		return nil
	default:
		return fmt.Errorf("Codex thread is not ready (status %q); open it through the configured app-server", reply.Thread.Status.Type)
	}
}

func (c *connection) rpc(ctx context.Context, id int, method string, params any) (json.RawMessage, error) {
	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if len(b) > MaxRequestBytes {
		return nil, errors.New("Codex request too large")
	}
	if err = c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, err
	}
	return c.response(ctx, id)
}

func (c *connection) response(ctx context.Context, id int) (json.RawMessage, error) {
	for {
		kind, b, err := c.ws.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("Codex response: %w", err)
		}
		if kind != websocket.MessageText {
			return nil, errors.New("Codex returned a non-text JSON-RPC frame")
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
		}
		if err := json.Unmarshal(b, &message); err != nil {
			return nil, fmt.Errorf("invalid Codex JSON-RPC frame: %w", err)
		}
		// Notifications and requests belong to the session's UI. This adapter
		// never handles approvals, tools, or user questions on its behalf.
		if message.Method != "" {
			continue
		}
		var responseID int
		if json.Unmarshal(message.ID, &responseID) != nil || responseID != id {
			continue
		}
		if message.Error != nil {
			return nil, message.Error
		}
		if len(message.Result) == 0 || string(message.Result) == "null" {
			return nil, errors.New("Codex JSON-RPC response has no result")
		}
		return message.Result, nil
	}
}
