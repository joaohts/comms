package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
	"github.com/joaohts/comms/internal/porter"
)

// hookInput is the subset of Claude Code / Codex hook stdin porter reads.
type hookInput struct {
	SessionID        string         `json:"session_id"`
	Cwd              string         `json:"cwd"`
	Event            string         `json:"hook_event_name"`
	NotificationType string         `json:"notification_type"`
	Message          string         `json:"message"`
	ToolName         string         `json:"tool_name"`
	ToolInput        map[string]any `json:"tool_input"`
	AgentID          string         `json:"agent_id"`
	AgentType        string         `json:"agent_type"`
	SessionTitle     string         `json:"session_title"`
	TranscriptPath   string         `json:"transcript_path"`
	AgentTranscript  string         `json:"agent_transcript_path"`
}

// subagentTranscript is where Claude keeps a real subagent's transcript.
// Claude's internal helpers fire SubagentStop without one.
func subagentTranscript(in hookInput) string {
	if in.AgentTranscript != "" {
		return in.AgentTranscript
	}
	if in.TranscriptPath == "" || in.AgentID == "" {
		return ""
	}
	return filepath.Join(strings.TrimSuffix(in.TranscriptPath, ".jsonl"), "subagents", "agent-"+in.AgentID+".jsonl")
}

// hookEvent maps a hook to a porter event; ok is false for hooks porter
// does not track. Subagent hooks fire in the parent and get their own row.
func hookEvent(in hookInput, harness string) (porter.Event, bool) {
	e := porter.Event{Agent: in.SessionID, Harness: harness, Title: in.SessionTitle}
	if in.Cwd != "" {
		e.Project = filepath.Base(in.Cwd)
	}
	switch in.Event {
	case "SessionStart":
		e.Kind = porter.KindStart
	case "UserPromptSubmit":
		e.Kind = porter.KindPrompt
	case "Notification":
		switch in.NotificationType {
		case "permission_prompt":
			e.Kind, e.Needs = porter.KindNeeds, &porter.Needs{Kind: "permission", Text: clipText(in.Message, 120)}
		case "elicitation_dialog", "elicitation_url_dialog", "agent_needs_input":
			e.Kind, e.Needs = porter.KindNeeds, &porter.Needs{Kind: "input", Text: clipText(in.Message, 120)}
		default:
			return e, false
		}
	case "Elicitation":
		e.Kind, e.Needs = porter.KindNeeds, &porter.Needs{Kind: "input", Text: clipText(in.Message, 120)}
	case "PermissionRequest":
		e.Kind, e.Needs = porter.KindNeeds, &porter.Needs{Kind: "permission", Text: toolText(in, 120)}
	case "Stop", "StopFailure":
		e.Kind = porter.KindStop
	case "SessionEnd":
		e.Kind = porter.KindEnd
	case "SubagentStart", "SubagentStop":
		if in.AgentID == "" {
			return e, false
		}
		e.Parent, e.Agent, e.AgentType, e.Title = in.SessionID, in.AgentID, in.AgentType, ""
		e.Kind = porter.KindPrompt
		if in.Event == "SubagentStop" {
			e.Kind = porter.KindStop
			if harness == "claude" {
				if p := subagentTranscript(in); p != "" {
					if _, err := os.Stat(p); err != nil {
						return e, false
					}
				}
			}
		}
	default:
		return e, false
	}
	return e, e.Agent != ""
}

// toolText is a short description of a tool call, never its full input.
func toolText(in hookInput, n int) string {
	detail := ""
	for _, k := range []string{"command", "file_path", "url", "pattern", "query", "description", "path"} {
		if v, ok := in.ToolInput[k].(string); ok && v != "" {
			detail = v
			break
		}
	}
	name := in.ToolName
	if name == "" {
		name = "tool"
	}
	if detail == "" {
		return clipText(name, n)
	}
	return clipText(name+": "+strings.Join(strings.Fields(detail), " "), n)
}

// porterHook is the Claude Code / Codex hook. It never fails the harness:
// errors are swallowed, and a PermissionRequest falls back to "ask".
func (a *app) porterHook(ctx context.Context, env porterEnv, args []string) error {
	f := flags("porter hook")
	harness := f.String("harness", "claude", "")
	if err := parse(f, args); err != nil {
		return err
	}
	if *harness != "claude" && *harness != "codex" {
		return usageError("--harness must be claude or codex")
	}
	reply := func(v any) error {
		if v == nil {
			if *harness != "codex" {
				return nil
			}
			v = map[string]any{} // Codex lifecycle hooks expect a JSON reply.
		}
		return json.NewEncoder(a.out).Encode(v)
	}
	if a.getenv("AGENT_MONITOR_INTERNAL") != "" {
		return reply(nil)
	}
	raw, _ := io.ReadAll(io.LimitReader(a.in, 4<<20))
	var in hookInput
	if json.Unmarshal(raw, &in) != nil {
		return reply(nil)
	}
	e, ok := hookEvent(in, *harness)
	if !ok {
		return reply(nil)
	}
	if _, err := applyEvent(env.state, e); err != nil {
		fmt.Fprintln(a.errOut, "porter hook:", err)
	}
	if in.Event != "PermissionRequest" || *harness != "claude" {
		return reply(nil)
	}
	decision := a.approval(ctx, env, in)
	if decision != "ask" {
		// Answered remotely: the agent resumes (or reports the denial).
		applyEvent(env.state, porter.Event{Agent: in.SessionID, Harness: *harness, Kind: porter.KindPrompt})
	}
	return reply(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": map[string]string{"behavior": decision}}})
}

// approval decides a PermissionRequest: allow | deny | ask.
func (a *app) approval(ctx context.Context, env porterEnv, in hookInput) string {
	cfg, err := porter.LoadConfig(porter.ConfigPath())
	text := toolText(in, 300)
	record := auditRecord{Kind: porter.NotifyPermission, Agent: in.SessionID, Text: text, Mode: cfg.Approvals}
	if err != nil || !cfg.Exists {
		record.Result, record.Mode = "ask", "unconfigured"
		audit(env, record)
		return "ask"
	}
	switch cfg.Approvals {
	case porter.ApprovalsTerminal:
		record.Result = "ask"
		audit(env, record)
		return "ask"
	case porter.ApprovalsAuto:
		if idle, ok := hidIdle(); ok && idle < cfg.IdleThreshold {
			record.Result, record.Mode = "ask", "auto:present"
			audit(env, record)
			return "ask"
		}
		record.Mode = "auto:away"
	}
	if a.c == nil {
		a.c = client.New(env.socket)
		defer a.c.Close()
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.AskTimeout+5*time.Second)
	defer cancel()
	result, err := a.ask(ctx, env, comms.PorterQuestionRequest{Kind: porter.NotifyPermission, Text: text, Agent: in.SessionID, TimeoutMS: cfg.AskTimeout.Milliseconds()}, record.Mode)
	switch {
	case err != nil:
		return "ask"
	case result == "allow":
		return "allow"
	case result == "deny":
		return "deny"
	}
	return "ask"
}

var hidIdleTime = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// hidIdle reports time since the last keyboard/mouse input. It is false on
// machines without a display session (Linux, headless), which ask the phone.
var hidIdle = func() (time.Duration, bool) {
	if runtime.GOOS != "darwin" {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOHIDSystem", "-d", "4").Output()
	if err != nil {
		return 0, false
	}
	return parseHIDIdle(out)
}

func parseHIDIdle(out []byte) (time.Duration, bool) {
	m := hidIdleTime.FindSubmatch(out)
	if m == nil {
		return 0, false
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(ns), true
}

// Hook events registered per harness.
var (
	claudeHookEvents = []string{"SessionStart", "UserPromptSubmit", "Notification", "PermissionRequest", "Elicitation", "Stop", "StopFailure", "SessionEnd", "SubagentStart", "SubagentStop"}
	codexHookEvents  = []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop", "SessionEnd", "SubagentStart", "SubagentStop"}
)

const hookMarker = " porter hook"

func (a *app) porterInstallHooks(env porterEnv, args []string) error {
	f := flags("porter install-hooks")
	uninstall := f.Bool("uninstall", false, "")
	bin := f.String("bin", "", "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 0 {
		return usageError("usage: comms porter install-hooks [--uninstall] [--bin PATH]")
	}
	if *bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		*bin = exe
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := porter.LoadConfig(porter.ConfigPath())
	if err != nil {
		return err
	}
	base := shellQuote(*bin)
	if def := comms.DefaultConfig().DataDir; filepath.Clean(env.dataDir) != filepath.Clean(def) {
		base += " --data-dir " + shellQuote(env.dataDir)
	}
	// Claude's command-hook default timeout is 600 s; a PermissionRequest
	// hook must outlive ask_timeout so the hook, not Claude, decides "ask".
	permTimeout := int((cfg.AskTimeout + 60*time.Second).Seconds())
	results := []map[string]any{}
	targets := []struct {
		path, harness string
		events        []string
		timeout       bool
		create        bool
	}{
		{filepath.Join(home, ".claude", "settings.json"), "claude", claudeHookEvents, true, true},
		{filepath.Join(home, ".codex", "hooks.json"), "codex", codexHookEvents, false, dirExists(filepath.Join(home, ".codex"))},
	}
	for _, t := range targets {
		if _, err := os.Stat(t.path); err != nil && !t.create {
			continue
		}
		cmd := base + " porter hook --harness " + t.harness
		var timeouts map[string]int
		if t.timeout {
			timeouts = map[string]int{"PermissionRequest": permTimeout}
		}
		changed, backup, err := editHooks(t.path, cmd, t.events, timeouts, *uninstall)
		if err != nil {
			return fmt.Errorf("%s: %w", t.path, err)
		}
		results = append(results, map[string]any{"file": t.path, "changed": changed, "backup": backup})
	}
	return a.output(map[string]any{"hooks": results, "uninstall": *uninstall})
}

func dirExists(p string) bool { i, err := os.Stat(p); return err == nil && i.IsDir() }

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// editHooks removes porter's hook entries from a Claude/Codex hooks file and,
// unless uninstalling, adds one entry per event. Other content keeps its
// order and formatting of values; the file is backed up before any change.
func editHooks(path, cmd string, events []string, timeouts map[string]int, uninstall bool) (bool, string, error) {
	orig, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, "", err
	}
	top := &orderedObject{}
	if len(bytes.TrimSpace(orig)) > 0 {
		if top, err = parseOrdered(orig); err != nil {
			return false, "", err
		}
	}
	hooks := &orderedObject{}
	if raw, ok := top.get("hooks"); ok {
		if hooks, err = parseOrdered(raw); err != nil {
			return false, "", fmt.Errorf("hooks: %w", err)
		}
	}
	for _, name := range slices.Clone(hooks.keys) {
		raw, _ := hooks.get(name)
		var groups []json.RawMessage
		if json.Unmarshal(raw, &groups) != nil {
			continue
		}
		kept := []json.RawMessage{}
		for _, g := range groups {
			if !bytes.Contains(g, []byte(hookMarker)) {
				kept = append(kept, g)
				continue
			}
			if g = withoutPorter(g); g != nil {
				kept = append(kept, g)
			}
		}
		if len(kept) == 0 {
			hooks.del(name)
		} else {
			hooks.set(name, marshalJSON(kept))
		}
	}
	if !uninstall {
		for _, name := range events {
			entry := map[string]any{"type": "command", "command": cmd}
			if t := timeouts[name]; t > 0 {
				entry["timeout"] = t
			} else if timeouts != nil {
				entry["timeout"] = 30
			}
			var groups []json.RawMessage
			if raw, ok := hooks.get(name); ok {
				json.Unmarshal(raw, &groups)
			}
			groups = append(groups, marshalJSON(map[string]any{"hooks": []any{entry}}))
			hooks.set(name, marshalJSON(groups))
		}
	}
	if len(hooks.keys) == 0 {
		top.del("hooks")
	} else {
		top.set("hooks", hooks.encode(""))
	}
	out := top.encode("")
	out = append(out, '\n')
	if bytes.Equal(out, orig) || (len(orig) == 0 && uninstall) {
		return false, "", nil
	}
	if canon(out) == canon(orig) {
		return false, "", nil
	}
	backup := ""
	if len(orig) > 0 {
		backup = path + ".bak-porter-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, orig, 0o600); err != nil {
			return false, "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, "", err
	}
	tmp := path + ".porter-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return false, "", err
	}
	return true, backup, os.Rename(tmp, path)
}

// withoutPorter drops porter's commands from one matcher group; nil when
// nothing else remains in it.
func withoutPorter(g json.RawMessage) json.RawMessage {
	obj, err := parseOrdered(g)
	if err != nil {
		return g
	}
	raw, _ := obj.get("hooks")
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return g
	}
	kept := []json.RawMessage{}
	for _, e := range entries {
		var v struct {
			Command string `json:"command"`
		}
		json.Unmarshal(e, &v)
		if !strings.Contains(v.Command, hookMarker) {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	obj.set("hooks", marshalJSON(kept))
	return obj.encode("")
}

func canon(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	return string(marshalJSON(v))
}

func marshalJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// orderedObject is a JSON object that keeps its key order.
type orderedObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseOrdered(b []byte) (*orderedObject, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if t != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	o := &orderedObject{vals: map[string]json.RawMessage{}}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		o.set(k, v)
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *orderedObject) get(k string) (json.RawMessage, bool) { v, ok := o.vals[k]; return v, ok }
func (o *orderedObject) set(k string, v json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}
func (o *orderedObject) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, v := range o.keys {
		if v == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// encode writes the object indented by two spaces per level.
func (o *orderedObject) encode(prefix string) json.RawMessage {
	if len(o.keys) == 0 {
		return json.RawMessage("{}")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range o.keys {
		var v bytes.Buffer
		if json.Indent(&v, o.vals[k], prefix+"  ", "  ") != nil {
			v.Write(o.vals[k])
		}
		fmt.Fprintf(&b, "%s  %s: %s", prefix, marshalJSON(k), v.Bytes())
		if i < len(o.keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(prefix + "}")
	return b.Bytes()
}
