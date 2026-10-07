package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joaohts/comms/internal/comms"
	"github.com/joaohts/comms/internal/porter"
)

// porterTestEnv isolates HOME, the porter config and the data dir.
func porterTestEnv(t *testing.T) (dataDir, configPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath = filepath.Join(home, ".config", "porter", "config.toml")
	t.Setenv("PORTER_CONFIG", configPath)
	return filepath.Join(home, "data"), configPath
}

func writeConfig(t *testing.T, path string, change func(*porter.Config)) {
	t.Helper()
	c := porter.DefaultConfig()
	c.Approvers = []string{"m_phone"}
	if change != nil {
		change(&c)
	}
	if err := porter.SaveConfig(path, c); err != nil {
		t.Fatal(err)
	}
}

func runHook(t *testing.T, a *app, dataDir, harness, input string) string {
	t.Helper()
	out := &bytes.Buffer{}
	a.in, a.out = strings.NewReader(input), out
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "hook", "--harness", harness}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func loadAgents(t *testing.T, dataDir string) map[string]*porter.Agent {
	t.Helper()
	s, err := porter.Store{Path: filepath.Join(dataDir, "porter", "state.json")}.Load()
	if err != nil {
		t.Fatal(err)
	}
	return s.Agents
}

func TestHookEventMapping(t *testing.T) {
	cases := []struct {
		in   hookInput
		kind string
		ok   bool
	}{
		{hookInput{SessionID: "s", Event: "SessionStart", SessionTitle: "pager", Cwd: "/x/notes"}, porter.KindStart, true},
		{hookInput{SessionID: "s", Event: "UserPromptSubmit"}, porter.KindPrompt, true},
		{hookInput{SessionID: "s", Event: "Notification", NotificationType: "idle_prompt"}, "", false},
		{hookInput{SessionID: "s", Event: "Notification", NotificationType: "permission_prompt", Message: "m"}, porter.KindNeeds, true},
		{hookInput{SessionID: "s", Event: "Elicitation"}, porter.KindNeeds, true},
		{hookInput{SessionID: "s", Event: "PermissionRequest", ToolName: "Bash", ToolInput: map[string]any{"command": "npm   install"}}, porter.KindNeeds, true},
		{hookInput{SessionID: "s", Event: "StopFailure"}, porter.KindStop, true},
		{hookInput{SessionID: "s", Event: "SessionEnd"}, porter.KindEnd, true},
		{hookInput{SessionID: "s", Event: "SubagentStart", AgentID: "sub", AgentType: "Explore"}, porter.KindPrompt, true},
		{hookInput{SessionID: "s", Event: "SubagentStop"}, "", false},
		{hookInput{SessionID: "s", Event: "SubagentStop", AgentID: "sub"}, porter.KindStop, true},
		{hookInput{SessionID: "s", Event: "PreToolUse"}, "", false},
		{hookInput{Event: "Stop"}, porter.KindStop, false},
	}
	for _, c := range cases {
		e, ok := hookEvent(c.in, "claude")
		if ok != c.ok || (ok && e.Kind != c.kind) {
			t.Fatalf("%+v → %+v %v", c.in, e, ok)
		}
	}
	e, _ := hookEvent(cases[0].in, "claude")
	if e.Title != "pager" || e.Project != "notes" {
		t.Fatalf("start: %+v", e)
	}
	e, _ = hookEvent(cases[5].in, "claude")
	if e.Needs.Kind != "permission" || e.Needs.Text != "Bash: npm install" {
		t.Fatalf("permission: %+v", e.Needs)
	}
	e, _ = hookEvent(cases[8].in, "claude")
	if e.Agent != "sub" || e.Parent != "s" || e.AgentType != "Explore" {
		t.Fatalf("subagent: %+v", e)
	}
	long := hookInput{ToolName: "Write", ToolInput: map[string]any{"file_path": strings.Repeat("x", 500), "content": "secret"}}
	if got := toolText(long, 120); len([]rune(got)) != 120 || strings.Contains(got, "secret") {
		t.Fatalf("tool text must be short and omit content: %q", got)
	}
}

func TestHookRecordsLifecycleAndRepliesPerHarness(t *testing.T) {
	dataDir, cfg := porterTestEnv(t)
	writeConfig(t, cfg, func(c *porter.Config) { c.Approvals = porter.ApprovalsTerminal })
	a, _ := testApp(t, http.NotFoundHandler())
	if out := runHook(t, a, dataDir, "claude", `{"session_id":"c1","hook_event_name":"SessionStart","cwd":"/w/notes","session_title":"pager"}`); out != "" {
		t.Fatalf("claude lifecycle hooks print nothing: %q", out)
	}
	runHook(t, a, dataDir, "claude", `{"session_id":"c1","hook_event_name":"UserPromptSubmit","user_prompt":"never stored"}`)
	runHook(t, a, dataDir, "claude", `{"session_id":"c1","hook_event_name":"SubagentStart","agent_id":"sub1","agent_type":"Explore"}`)
	if out := runHook(t, a, dataDir, "codex", `{"session_id":"x1","hook_event_name":"Stop"}`); strings.TrimSpace(out) != "{}" {
		t.Fatalf("codex hooks reply {}: %q", out)
	}
	runHook(t, a, dataDir, "claude", `not json`)
	agents := loadAgents(t, dataDir)
	c1, sub, x1 := agents["c1"], agents["sub1"], agents["x1"]
	if c1 == nil || c1.Status != porter.StatusRunning || c1.Title != "pager" || c1.Project != "notes" || c1.Harness != "claude" {
		t.Fatalf("c1: %+v", c1)
	}
	if sub == nil || sub.Parent != "c1" || sub.AgentType != "Explore" || x1 == nil || x1.Harness != "codex" {
		t.Fatalf("sub/x1: %+v %+v", sub, x1)
	}
	raw, _ := os.ReadFile(filepath.Join(dataDir, "porter", "state.json"))
	if strings.Contains(string(raw), "never stored") {
		t.Fatal("prompt text was stored")
	}

	// terminal → ask at once, state needs_you, audited.
	out := runHook(t, a, dataDir, "claude", `{"session_id":"c1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf build"}}`)
	var reply struct {
		Out struct {
			Event    string `json:"hookEventName"`
			Decision struct {
				Behavior string `json:"behavior"`
			} `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(out), &reply) != nil || reply.Out.Event != "PermissionRequest" || reply.Out.Decision.Behavior != "ask" {
		t.Fatalf("terminal reply: %q", out)
	}
	if a := loadAgents(t, dataDir)["c1"]; a.Status != porter.StatusNeedsYou || a.Needs.Text != "Bash: rm -rf build" {
		t.Fatalf("needs: %+v", a)
	}
	// Codex PermissionRequest is tracked but never decided by porter.
	if out := runHook(t, a, dataDir, "codex", `{"session_id":"x1","hook_event_name":"PermissionRequest","tool_name":"shell"}`); strings.TrimSpace(out) != "{}" {
		t.Fatalf("codex permission: %q", out)
	}
	// Agent Monitor's internal subprocesses are never reported.
	a.getenv = func(k string) string {
		if k == "AGENT_MONITOR_INTERNAL" {
			return "1"
		}
		return ""
	}
	runHook(t, a, dataDir, "claude", `{"session_id":"internal","hook_event_name":"SessionStart"}`)
	if loadAgents(t, dataDir)["internal"] != nil {
		t.Fatal("internal job reported")
	}
	auditLog, _ := os.ReadFile(filepath.Join(dataDir, "porter", "audit.jsonl"))
	if !strings.Contains(string(auditLog), `"result":"ask"`) || !strings.Contains(string(auditLog), `"mode":"terminal"`) {
		t.Fatalf("audit: %s", auditLog)
	}
}

// fakeQuestions serves the porter question API with a scripted outcome.
func fakeQuestions(t *testing.T, outcome comms.PorterQuestion, asked *[]comms.PorterQuestionRequest) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/porter/questions":
			var q comms.PorterQuestionRequest
			json.NewDecoder(r.Body).Decode(&q)
			*asked = append(*asked, q)
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(comms.PorterQuestion{ID: "ntf_1", State: "pending"})
		case r.Method == "GET" && r.URL.Path == "/v1/porter/questions/ntf_1":
			json.NewEncoder(w).Encode(outcome)
		case bindings(w, r):
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"code":"not_found","error":"x"}`)
		}
	})
}

func TestHookAsksPhoneWhenAwayAndFallsBackToAsk(t *testing.T) {
	dataDir, cfg := porterTestEnv(t)
	writeConfig(t, cfg, nil) // auto
	idle := 10 * time.Minute
	present := true
	old := hidIdle
	hidIdle = func() (time.Duration, bool) { return idle, present }
	t.Cleanup(func() { hidIdle = old })
	var asked []comms.PorterQuestionRequest
	a, _ := testApp(t, fakeQuestions(t, comms.PorterQuestion{ID: "ntf_1", State: "answered", Choice: "Allow", By: "m_phone"}, &asked))
	in := `{"session_id":"c1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"npm install"}}`
	if out := runHook(t, a, dataDir, "claude", in); !strings.Contains(out, `"behavior":"allow"`) {
		t.Fatalf("away → phone allow: %q", out)
	}
	if len(asked) != 1 || asked[0].Kind != "permission" || asked[0].Text != "Bash: npm install" || asked[0].Agent != "c1" || asked[0].TimeoutMS != 120000 {
		t.Fatalf("question: %+v", asked)
	}
	if a := loadAgents(t, dataDir)["c1"]; a.Status != porter.StatusRunning {
		t.Fatalf("allowed agent resumes: %+v", a)
	}
	// Present at the keyboard → terminal prompt, no question.
	idle = 5 * time.Second
	if out := runHook(t, a, dataDir, "claude", in); !strings.Contains(out, `"behavior":"ask"`) || len(asked) != 1 {
		t.Fatalf("present: %q %d", out, len(asked))
	}
	// No display (headless/Linux) → phone, even with auto.
	present = false
	a2, _ := testApp(t, fakeQuestions(t, comms.PorterQuestion{ID: "ntf_1", State: "answered", Choice: "Deny"}, &asked))
	if out := runHook(t, a2, dataDir, "claude", in); !strings.Contains(out, `"behavior":"deny"`) {
		t.Fatalf("headless deny: %q", out)
	}
	a3, _ := testApp(t, fakeQuestions(t, comms.PorterQuestion{ID: "ntf_1", State: "timeout"}, &asked))
	if out := runHook(t, a3, dataDir, "claude", in); !strings.Contains(out, `"behavior":"ask"`) {
		t.Fatalf("timeout → ask: %q", out)
	}
	// Node down / any error → ask.
	a4, _ := testApp(t, http.NotFoundHandler())
	if out := runHook(t, a4, dataDir, "claude", in); !strings.Contains(out, `"behavior":"ask"`) {
		t.Fatalf("error → ask: %q", out)
	}
	// Unconfigured porter never asks the phone.
	os.Remove(cfg)
	n := len(asked)
	if out := runHook(t, a2, dataDir, "claude", in); !strings.Contains(out, `"behavior":"ask"`) || len(asked) != n {
		t.Fatalf("unconfigured: %q", out)
	}
}

func TestPorterAskPrintsResultAndExitCode(t *testing.T) {
	dataDir, _ := porterTestEnv(t)
	for _, c := range []struct {
		outcome comms.PorterQuestion
		want    string
		code    int
	}{
		{comms.PorterQuestion{State: "answered", Choice: "Allow"}, "allow", 0},
		{comms.PorterQuestion{State: "answered", Choice: "Deny"}, "deny", exitDeny},
		{comms.PorterQuestion{State: "timeout"}, "timeout", exitTimeout},
	} {
		var asked []comms.PorterQuestionRequest
		a, out := testApp(t, fakeQuestions(t, c.outcome, &asked))
		err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "ask", "Deploy pager-api?", "--timeout", "30s"})
		var exit exitError
		code := 0
		if errors.As(err, &exit) {
			code = exit.code
		} else if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != c.want || code != c.code || asked[0].TimeoutMS != 30000 || asked[0].Text != "Deploy pager-api?" {
			t.Fatalf("%s: out=%q code=%d asked=%+v", c.want, out, code, asked)
		}
	}
	a, _ := testApp(t, http.NotFoundHandler())
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "ask"}); err == nil {
		t.Fatal("ask without text accepted")
	}
	auditLog, _ := os.ReadFile(filepath.Join(dataDir, "porter", "audit.jsonl"))
	if strings.Count(string(auditLog), "\n") != 3 {
		t.Fatalf("audit: %s", auditLog)
	}
}

func TestPorterNotifyFromAndToAlias(t *testing.T) {
	dataDir, _ := porterTestEnv(t)
	var got comms.PorterNotifyRequest
	a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bindings(w, r) {
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"ntf_1","to":"m_phone:a_app"}`)
	}))
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "notify", "--to", "phone", "--title", "Hi"}); err == nil {
		t.Fatal("notify without reason accepted")
	}
	err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "notify", "--to", "phone", "--title", "Deploy finished", "--body", "live", "--reason", "you asked", "--ask", "Nice, Roll back", "--from", "worker", "--priority", "high"})
	if err != nil {
		t.Fatal(err)
	}
	if got.To != "phone" || got.SessionID != "s_1" || got.Reason != "you asked" || len(got.Ask) != 2 || got.Priority != "high" || !strings.Contains(out.String(), "ntf_1") {
		t.Fatalf("request: %+v", got)
	}
}

func TestPorterTrustListRevokeAndGet(t *testing.T) {
	dataDir, _ := porterTestEnv(t)
	path := filepath.Join(dataDir, "porter", "trusts.json")
	porter.UpdateJSON(path, func(ts *porter.Trusts) (bool, error) {
		ts.Put(porter.Trust{ID: "tr_1", Receiver: "pi:joana", ReceiverID: "a_1", Sender: "work:sec", SenderID: "m_w:a_2", Scope: porter.TrustAlways})
		return true, nil
	})
	a, out := testApp(t, http.NotFoundHandler())
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "trust", "list"}); err != nil || !strings.Contains(out.String(), "tr_1") || !strings.Contains(out.String(), "work:sec") {
		t.Fatalf("list: %v %s", err, out)
	}
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "trust", "revoke", "tr_1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "trust", "revoke", "tr_1"}); err == nil {
		t.Fatal("revoking twice succeeded")
	}
	cache := porter.Cache{Dir: filepath.Join(dataDir, "porter", "cache")}
	item, _ := json.Marshal(porter.Agent{ID: "j1", Harness: "claude", Title: "joana", Status: porter.StatusNeedsYou})
	if err := cache.Apply("m_pi", porter.Inbound{Type: porter.TypeSnapshot, Machine: "pi", Topic: porter.TopicAgents, Items: []json.RawMessage{item}}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "get", "pi", "agents"}); err != nil || !strings.Contains(out.String(), `"joana"`) {
		t.Fatalf("get: %v %s", err, out)
	}
	out.Reset()
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "status", "--all"}); err != nil || !strings.Contains(out.String(), "pi") || !strings.Contains(out.String(), "joana") {
		t.Fatalf("status --all: %v %s", err, out)
	}
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "get", "nowhere", "agents"}); err == nil {
		t.Fatal("uncached get succeeded")
	}
	out.Reset()
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "get", "local", "status"}); err != nil || !strings.Contains(out.String(), `"disk_pct"`) {
		t.Fatalf("local status: %v %s", err, out)
	}
}

func TestPorterSetupResolvesApprovers(t *testing.T) {
	dataDir, cfg := porterTestEnv(t)
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]comms.Peer{{MachineID: "m_phone", Alias: "phone"}})
	}))
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "setup", "--approver", "ghost"}); err == nil {
		t.Fatal("unknown approver accepted")
	}
	if err := a.run(context.Background(), []string{"--data-dir", dataDir, "porter", "setup", "--approver", "phone", "--approvals", "phone", "--ask-timeout", "90s"}); err != nil {
		t.Fatal(err)
	}
	c, err := porter.LoadConfig(cfg)
	if err != nil || len(c.Approvers) != 1 || c.Approvers[0] != "m_phone" || c.Approvals != "phone" || c.AskTimeout != 90*time.Second || !c.Shares("agents", "m_x") || !c.Shares("status", "m_x") {
		t.Fatalf("config: %+v %v", c, err)
	}
}

const otherSettings = `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/x/agent-monitor-hook.sh"
          }
        ]
      }
    ]
  },
  "permissions": {
    "defaultMode": "bypassPermissions"
  }
}
`

func TestInstallHooksIdempotentPreservesAndUninstalls(t *testing.T) {
	dataDir, cfg := porterTestEnv(t)
	writeConfig(t, cfg, nil)
	home := os.Getenv("HOME")
	settings := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o700)
	os.WriteFile(settings, []byte(otherSettings), 0o600)
	codexHooks := filepath.Join(home, ".codex", "hooks.json")
	a, out := testApp(t, http.NotFoundHandler())
	install := func(extra ...string) map[string]any {
		out.Reset()
		if err := a.run(context.Background(), append([]string{"--data-dir", dataDir, "porter", "install-hooks", "--bin", "/opt/comms bin/comms"}, extra...)); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		json.Unmarshal(out.Bytes(), &v)
		return v
	}
	install()
	if _, err := os.Stat(codexHooks); err == nil {
		t.Fatal("codex hooks written without ~/.codex")
	}
	raw, _ := os.ReadFile(settings)
	var s struct {
		Model string                                   `json:"model"`
		Hooks map[string][]struct{ Hooks []hookEntry } `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.Model != "opus" || !strings.HasPrefix(string(raw), "{\n  \"model\"") {
		t.Fatalf("settings: %v\n%s", err, raw)
	}
	cmd := "'/opt/comms bin/comms' --data-dir " + dataDir + " porter hook --harness claude"
	if len(s.Hooks["Stop"]) != 2 || s.Hooks["Stop"][0].Hooks[0].Command != "/x/agent-monitor-hook.sh" || s.Hooks["Stop"][1].Hooks[0].Command != cmd {
		t.Fatalf("Stop hooks: %+v", s.Hooks["Stop"])
	}
	for _, ev := range claudeHookEvents {
		if len(s.Hooks[ev]) == 0 {
			t.Fatalf("missing %s", ev)
		}
	}
	if p := s.Hooks["PermissionRequest"][0].Hooks[0]; p.Timeout != 180 {
		t.Fatalf("permission timeout must exceed ask_timeout: %+v", p)
	}
	backups, _ := filepath.Glob(settings + ".bak-porter-*")
	if len(backups) != 1 {
		t.Fatalf("backups: %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != otherSettings {
		t.Fatal("backup differs from original")
	}
	// Idempotent: a second run changes nothing.
	if v := install(); v["hooks"].([]any)[0].(map[string]any)["changed"] != false {
		t.Fatalf("second install changed: %v", v)
	}
	// Codex is configured once ~/.codex exists.
	os.MkdirAll(filepath.Dir(codexHooks), 0o700)
	install()
	craw, _ := os.ReadFile(codexHooks)
	if !strings.Contains(string(craw), "--harness codex") || strings.Contains(string(craw), "timeout") || strings.Contains(string(craw), "Notification") {
		t.Fatalf("codex hooks: %s", craw)
	}
	install("--uninstall")
	raw, _ = os.ReadFile(settings)
	if canon(raw) != canon([]byte(otherSettings)) {
		t.Fatalf("uninstall did not restore other hooks:\n%s", raw)
	}
	craw, _ = os.ReadFile(codexHooks)
	if strings.Contains(string(craw), "porter") {
		t.Fatalf("codex uninstall: %s", craw)
	}
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func TestParseHIDIdle(t *testing.T) {
	d, ok := parseHIDIdle([]byte(`    | |   "HIDIdleTime" = 5300000000` + "\n"))
	if !ok || d != 5300*time.Millisecond {
		t.Fatal(d, ok)
	}
	if _, ok := parseHIDIdle([]byte("nothing")); ok {
		t.Fatal("parsed nothing")
	}
}

func TestPairBundleToleratesPastedText(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	exported, _ := json.MarshalIndent(comms.Peer{MachineID: "m_phone", Alias: "phone", PublicKey: key}, "", "  ")
	std := base64.StdEncoding.EncodeToString(key)
	wrapped := std[:20] + "\n  " + std[20:]
	for _, in := range []string{
		string(exported),
		"\ufeff  " + string(exported) + "\n\n",
		"Here is my identity:\n```json\n" + string(exported) + "\n```\n",
		`{"machine_id":" m_phone ","alias":"phone","public_key":"` + strings.ReplaceAll(wrapped, "\n", `\n`) + `"}`,
		`{"machine_id":"m_phone","alias":"phone","public_key":"` + base64.RawURLEncoding.EncodeToString(key) + `"}`,
	} {
		p, err := parseBundle([]byte(in))
		if err != nil || p.MachineID != "m_phone" || p.Alias != "phone" || !bytes.Equal(p.PublicKey, key) {
			t.Fatalf("%q → %+v %v", in, p, err)
		}
	}
	if _, err := parseBundle([]byte("garbage")); err == nil {
		t.Fatal("garbage accepted")
	}
	// export → pair --stdin round trip through the CLI.
	var put comms.Peer
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&put)
		json.NewEncoder(w).Encode(put)
	}))
	a.in = strings.NewReader("\n" + string(exported) + "\n")
	if err := a.run(context.Background(), []string{"pair", "--stdin"}); err != nil || put.MachineID != "m_phone" || !bytes.Equal(put.PublicKey, key) {
		t.Fatalf("pair: %v %+v", err, put)
	}
}

func TestHookIgnoresSubagentStopWithoutTranscript(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "sess.jsonl")
	os.MkdirAll(filepath.Join(dir, "sess", "subagents"), 0o700)
	os.WriteFile(filepath.Join(dir, "sess", "subagents", "agent-real.jsonl"), []byte("{}"), 0o600)
	explicit := filepath.Join(dir, "explicit.jsonl")
	os.WriteFile(explicit, []byte("{}"), 0o600)
	for _, c := range []struct {
		in hookInput
		ok bool
	}{
		{hookInput{SessionID: "s", Event: "SubagentStop", AgentID: "real", TranscriptPath: parent}, true},
		{hookInput{SessionID: "s", Event: "SubagentStop", AgentID: "helper", TranscriptPath: parent}, false},
		{hookInput{SessionID: "s", Event: "SubagentStop", AgentID: "x", TranscriptPath: parent, AgentTranscript: explicit}, true},
		{hookInput{SessionID: "s", Event: "SubagentStop", AgentID: "x", AgentTranscript: filepath.Join(dir, "missing.jsonl")}, false},
		{hookInput{SessionID: "s", Event: "SubagentStart", AgentID: "helper", TranscriptPath: parent}, true},
	} {
		if _, ok := hookEvent(c.in, "claude"); ok != c.ok {
			t.Fatalf("%+v: ok=%v", c.in, ok)
		}
	}
}
