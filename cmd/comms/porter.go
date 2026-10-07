package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
	"github.com/joaohts/comms/internal/porter"
)

// exitError ends the process with a specific status after the command has
// printed its own result (porter ask: 2 = deny, 3 = timeout).
type exitError struct{ code int }

func (e exitError) Error() string { return "exit status " + strconv.Itoa(e.code) }

// Exit statuses of porter ask and ask-trust.
const (
	exitDeny    = 2
	exitTimeout = 3
)

type porterEnv struct {
	dataDir string
	socket  string
	state   porter.Store
}

func (e porterEnv) path(name string) string { return filepath.Join(e.dataDir, "porter", name) }

// porter records agent state reported by integrations without the node. A
// node started with serve --porter publishes it, carries notifications and
// questions, and caches peers' topics.
func (a *app) porter(ctx context.Context, dataDir, socket string, args []string) error {
	if len(args) == 0 {
		return usageError("porter requires a subcommand: setup | install-hooks | hook | event | status | get | notify | ask | ask-trust | trust")
	}
	if socket == "" {
		socket = filepath.Join(dataDir, "node.sock")
	}
	env := porterEnv{dataDir: dataDir, socket: socket, state: porter.Store{Path: filepath.Join(dataDir, "porter", "state.json")}}
	sub, rest := args[0], args[1:]
	switch sub {
	case "event":
		return a.porterEvent(env, rest)
	case "status":
		return a.porterStatus(env, rest)
	case "get":
		return a.porterGet(env, rest)
	case "trust":
		return a.porterTrust(env, rest)
	case "hook":
		return a.porterHook(ctx, env, rest)
	case "install-hooks":
		return a.porterInstallHooks(env, rest)
	}
	if a.c == nil {
		a.c = client.New(socket)
		defer a.c.Close()
	}
	switch sub {
	case "setup":
		return a.porterSetup(ctx, rest)
	case "notify":
		return a.porterNotify(ctx, rest)
	case "ask":
		return a.porterAsk(ctx, env, rest)
	case "ask-trust":
		return a.porterAskTrust(ctx, env, rest)
	}
	return usageError("unknown porter subcommand " + strconv.Quote(sub))
}

func (a *app) porterEvent(env porterEnv, args []string) error {
	fs := flag.NewFlagSet("porter event", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var e porter.Event
	var at, needsKind, needsText, special string
	fs.StringVar(&e.Agent, "agent", "", "agent id")
	fs.StringVar(&e.Harness, "harness", "", "harness name")
	fs.StringVar(&e.Kind, "kind", "", "start|prompt|needs|stop|end|update")
	fs.StringVar(&at, "at", "", "RFC3339 time (default now)")
	fs.StringVar(&e.Title, "title", "", "title")
	fs.StringVar(&e.Project, "project", "", "project")
	fs.StringVar(&e.Summary, "summary", "", "summary")
	fs.StringVar(&e.Parent, "parent", "", "parent agent id (subagents)")
	fs.StringVar(&e.AgentType, "agent-type", "", "subagent type")
	fs.StringVar(&needsKind, "needs-kind", "", "why the agent needs the user")
	fs.StringVar(&needsText, "needs-text", "", "short description of what is needed")
	fs.StringVar(&special, "special", "", "true|false")
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() > 0 {
		return usageError("porter event takes flags only")
	}
	if e.Agent == "" || e.Kind == "" {
		return usageError("porter event requires --agent and --kind")
	}
	if at != "" {
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return usageError("--at must be RFC3339")
		}
		e.At = t
	}
	if needsKind != "" || needsText != "" {
		e.Needs = &porter.Needs{Kind: needsKind, Text: needsText}
	}
	if special != "" {
		v, err := strconv.ParseBool(special)
		if err != nil {
			return usageError("--special must be true or false")
		}
		e.Special = &v
	}
	out, err := applyEvent(env.state, e)
	if err != nil || !a.jsonOutput {
		return err
	}
	if out == nil {
		return a.output(map[string]any{"ignored": e.Agent})
	}
	return a.output(out)
}

func applyEvent(st porter.Store, e porter.Event) (*porter.Agent, error) {
	var out *porter.Agent
	err := st.Update(func(s *porter.State) error {
		ag, err := s.Apply(e)
		if ag != nil {
			c := ag.View()
			out = &c
		}
		return err
	})
	return out, err
}

type machineAgents struct {
	machine string
	agents  []porter.Agent
}

func (a *app) porterStatus(env porterEnv, args []string) error {
	f := flags("porter status")
	all := f.Bool("all", false, "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 0 {
		return usageError("usage: comms porter status [--all]")
	}
	s, err := env.state.Load()
	if err != nil {
		return err
	}
	groups := []machineAgents{{"local", s.List()}}
	if *all {
		cache := porter.Cache{Dir: env.path("cache")}
		for _, id := range cache.Machines() {
			e, err := cache.Load(id, porter.TopicAgents)
			if err != nil || e == nil {
				continue
			}
			g := machineAgents{machine: e.Machine}
			if g.machine == "" {
				g.machine = id
			}
			for _, raw := range e.Items {
				var ag porter.Agent
				if json.Unmarshal(raw, &ag) == nil {
					g.agents = append(g.agents, ag)
				}
			}
			groups = append(groups, g)
		}
	}
	if a.jsonOutput {
		if !*all {
			return a.output(map[string]any{"agents": groups[0].agents})
		}
		out := map[string][]porter.Agent{}
		for _, g := range groups {
			out[g.machine] = g.agents
		}
		return a.output(map[string]any{"machines": out})
	}
	now := time.Now()
	w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	head := "STATUS\tAGENT\tHARNESS\tPROJECT\tFOR\tRUN\tWAIT\tSUMMARY"
	if *all {
		head = "MACHINE\t" + head
	}
	fmt.Fprintln(w, head)
	for _, g := range groups {
		for _, ag := range g.agents {
			if *all {
				fmt.Fprintf(w, "%s\t", g.machine)
			}
			fmt.Fprintln(w, agentRow(ag, now))
		}
	}
	return w.Flush()
}

func agentRow(ag porter.Agent, now time.Time) string {
	run, wait := ag.RunningMS, ag.WaitingMS
	cur := now.Sub(ag.StatusSince).Milliseconds()
	switch ag.Status {
	case porter.StatusRunning:
		run += cur
	case porter.StatusNeedsYou:
		wait += cur
	}
	name := ag.Title
	if name == "" {
		name = ag.ID
	}
	if ag.Parent != "" {
		name = "  └ " + name
		if ag.AgentType != "" {
			name += " (" + ag.AgentType + ")"
		}
	}
	mark := ""
	if ag.Special {
		mark = "*"
	}
	summary := ag.Summary
	if ag.Needs != nil {
		summary = strings.TrimSpace(ag.Needs.Kind + ": " + ag.Needs.Text)
	}
	if ag.Status == porter.StatusEnded {
		summary = strings.TrimSpace(ag.EndReason + " " + summary)
	}
	return fmt.Sprintf("%s%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s", ag.Status, mark, name, ag.Harness, ag.Project, dur(cur), dur(run), dur(wait), summary)
}

func dur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// porterGet prints the cached value of a peer's topic (MACHINE is its alias
// as published, or its machine id). "local" reads this machine.
func (a *app) porterGet(env porterEnv, args []string) error {
	if len(args) != 2 {
		return usageError("usage: comms porter get MACHINE TOPIC")
	}
	machine, topic := args[0], args[1]
	if machine == "local" || machine == "self" {
		switch topic {
		case porter.TopicAgents:
			s, err := env.state.Load()
			if err != nil {
				return err
			}
			return a.output(map[string]any{"machine": "local", "topic": topic, "items": s.List()})
		case porter.TopicStatus:
			s, _ := env.state.Load()
			return a.output(map[string]any{"machine": "local", "topic": topic, "value": porter.CollectStatus("", version, env.dataDir, s.Running())})
		case porter.TopicTrusts:
			t, err := porter.LoadJSON[porter.Trusts](env.path("trusts.json"))
			if err != nil {
				return err
			}
			items := []porter.TrustItem{}
			for _, v := range t.List() {
				items = append(items, v.Item())
			}
			return a.output(map[string]any{"machine": "local", "topic": topic, "items": items})
		}
		return usageError("unknown topic " + strconv.Quote(topic))
	}
	cache := porter.Cache{Dir: env.path("cache")}
	for _, id := range cache.Machines() {
		e, err := cache.Load(id, topic)
		if err == nil && e != nil && (id == machine || e.Machine == machine) {
			return a.output(e)
		}
	}
	return &client.Error{Code: "not_cached", Message: "nothing cached for " + machine + " " + topic + "; is porter on there, and are you granted and in its share list?"}
}

func (a *app) porterTrust(env porterEnv, args []string) error {
	path := env.path("trusts.json")
	if len(args) == 0 || args[0] == "list" {
		if len(args) > 1 {
			return usageError("usage: comms porter trust [list|revoke ID]")
		}
		t, err := porter.LoadJSON[porter.Trusts](path)
		if err != nil {
			return err
		}
		if a.jsonOutput {
			return a.output(map[string]any{"trusts": t.List()})
		}
		w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSCOPE\tRECEIVER\tSENDER\tSENDER ID\tAPPROVED")
		for _, v := range t.List() {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Scope, v.Receiver, v.Sender, v.SenderID, v.ApprovedAt.Format(time.RFC3339))
		}
		return w.Flush()
	}
	if args[0] != "revoke" || len(args) != 2 {
		return usageError("usage: comms porter trust [list|revoke ID]")
	}
	found := false
	err := porter.UpdateJSON(path, func(t *porter.Trusts) (bool, error) {
		_, found = t.Trusts[args[1]]
		delete(t.Trusts, args[1])
		return found, nil
	})
	if err != nil {
		return err
	}
	if !found {
		return &client.Error{Code: "not_found", Message: "no trust " + args[1]}
	}
	return a.output(map[string]any{"revoked": args[1]})
}

func (a *app) porterSetup(ctx context.Context, args []string) error {
	f := flags("porter setup")
	var approvers multiFlag
	f.Var(&approvers, "approver", "")
	approvals := f.String("approvals", "", "")
	askTimeout := f.Duration("ask-timeout", 0, "")
	idle := f.Duration("idle-threshold", 0, "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 0 || len(approvers) == 0 {
		return usageError("usage: comms porter setup --approver PEER [--approver PEER] [--approvals auto|phone|terminal] [--ask-timeout 2m] [--idle-threshold 2m]")
	}
	path := porter.ConfigPath()
	c, err := porter.LoadConfig(path)
	if err != nil {
		return err
	}
	var peers []comms.Peer
	if err := a.c.Do(ctx, "GET", "/v1/peers", nil, &peers); err != nil {
		return err
	}
	c.Approvers = nil
	for _, ref := range approvers {
		id := ""
		for _, p := range peers {
			if p.Alias == ref || p.MachineID == ref {
				id = p.MachineID
			}
		}
		if id == "" {
			return &client.Error{Code: "unknown_peer", Message: "approver " + ref + " is not paired; run comms pair first"}
		}
		c.Approvers = append(c.Approvers, id)
	}
	if *approvals != "" {
		if *approvals != porter.ApprovalsAuto && *approvals != porter.ApprovalsPhone && *approvals != porter.ApprovalsTerminal {
			return usageError("--approvals must be auto, phone or terminal")
		}
		c.Approvals = *approvals
	}
	if *askTimeout > 0 {
		c.AskTimeout = *askTimeout
	}
	if *idle > 0 {
		c.IdleThreshold = *idle
	}
	if err := porter.SaveConfig(path, c); err != nil {
		return err
	}
	return a.output(map[string]any{"config": path, "approvers": c.Approvers, "approvals": c.Approvals, "ask_timeout": c.AskTimeout.String(), "idle_threshold": c.IdleThreshold.String()})
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// sender resolves the comms session a notification comes from: --from when
// given, else the current harness session if it has one. A sessionless
// caller still gets machine context, and harness context when known.
func (a *app) sender(ctx context.Context, from string) (string, string, error) {
	agent := a.getenv("CLAUDE_CODE_SESSION_ID")
	if agent == "" {
		agent = a.getenv("CODEX_THREAD_ID")
	}
	s, err := a.session(ctx, from)
	if err != nil {
		var api *client.Error
		if from != "" || !errors.As(err, &api) || (api.Code != "usage" && api.Code != "session_not_found") {
			return "", "", err
		}
		return "", agent, nil
	}
	return s.ID, agent, nil
}

func (a *app) porterNotify(ctx context.Context, args []string) error {
	f := flags("porter notify")
	var q comms.PorterNotifyRequest
	f.StringVar(&q.To, "to", "", "")
	f.StringVar(&q.Title, "title", "", "")
	f.StringVar(&q.Body, "body", "", "")
	f.StringVar(&q.Reason, "reason", "", "")
	f.StringVar(&q.Priority, "priority", "normal", "")
	ask := f.String("ask", "", "")
	f.StringVar(&q.Link, "link", "", "")
	from := f.String("from", "", "")
	expires := f.Duration("expires", 0, "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 0 || q.To == "" || q.Title == "" {
		return usageError(`usage: comms porter notify --to PEER --title T --body B --reason R [--priority normal|high] [--ask "A,B"] [--link L] [--from ALIAS] [--expires 1h]`)
	}
	if strings.TrimSpace(q.Reason) == "" {
		return usageError("--reason is required: say why you are notifying")
	}
	if *ask != "" {
		q.Ask = strings.Split(*ask, ",")
	}
	q.ExpiresMS = expires.Milliseconds()
	var err error
	if q.SessionID, q.Agent, err = a.sender(ctx, *from); err != nil {
		return err
	}
	var out map[string]any
	if err := a.c.Do(ctx, "POST", "/v1/porter/notify", q, &out); err != nil {
		return err
	}
	return a.output(out)
}

// question asks every approver and waits for the outcome. Cancelling ctx
// withdraws the question (resolution terminal).
func (a *app) question(ctx context.Context, q comms.PorterQuestionRequest) (comms.PorterQuestion, error) {
	var out comms.PorterQuestion
	if err := a.c.Do(ctx, "POST", "/v1/porter/questions", q, &out); err != nil {
		return out, err
	}
	id := out.ID
	for out.State == "pending" {
		if ctx.Err() != nil {
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			a.c.Do(wctx, "DELETE", "/v1/porter/questions/"+url.PathEscape(id), nil, &out)
			cancel()
			return out, ctx.Err()
		}
		var next comms.PorterQuestion
		if err := a.c.Do(ctx, "GET", "/v1/porter/questions/"+url.PathEscape(id)+"?wait_ms=20000", nil, &next); err != nil {
			if ctx.Err() != nil {
				continue
			}
			return out, err
		}
		out = next
	}
	return out, nil
}

type auditRecord struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	ID     string    `json:"id,omitempty"`
	Agent  string    `json:"agent,omitempty"`
	Text   string    `json:"text,omitempty"`
	Mode   string    `json:"mode,omitempty"`
	Result string    `json:"result"`
	By     string    `json:"by,omitempty"`
	Error  string    `json:"error,omitempty"`
}

func audit(env porterEnv, r auditRecord) {
	r.At = time.Now().UTC().Truncate(time.Second)
	path := env.path("audit.jsonl")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	f.Write(append(b, '\n'))
}

func (a *app) porterAsk(ctx context.Context, env porterEnv, args []string) error {
	f := flags("porter ask")
	kind := f.String("kind", porter.NotifyPermission, "")
	timeout := f.Duration("timeout", 0, "")
	agent := f.String("agent", "", "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() != 1 || *kind != porter.NotifyPermission {
		return usageError(`usage: comms porter ask "TEXT" [--kind permission] [--timeout 2m]; prints allow|deny|timeout (exit 0|2|3)`)
	}
	q := comms.PorterQuestionRequest{Kind: *kind, Text: f.Arg(0), TimeoutMS: timeout.Milliseconds(), Agent: *agent}
	if q.Agent == "" {
		q.Agent = a.getenv("CLAUDE_CODE_SESSION_ID")
	}
	result, err := a.ask(ctx, env, q, "cli")
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, result)
	switch result {
	case "deny":
		return exitError{exitDeny}
	case "timeout":
		return exitError{exitTimeout}
	}
	return nil
}

// ask runs a permission question and audits it: allow | deny | timeout.
func (a *app) ask(ctx context.Context, env porterEnv, q comms.PorterQuestionRequest, mode string) (string, error) {
	out, err := a.question(ctx, q)
	r := auditRecord{Kind: q.Kind, ID: out.ID, Agent: q.Agent, Text: clipText(q.Text, 300), Mode: mode, By: out.By}
	result := "timeout"
	switch {
	case err != nil:
		r.Result, r.Error = "error", err.Error()
		audit(env, r)
		return "", err
	case out.State == "answered" && out.Choice == "Allow":
		result = "allow"
	case out.State == "answered" && out.Choice == "Deny":
		result = "deny"
	case out.State == "cancelled":
		result = "cancelled"
	}
	r.Result = result
	audit(env, r)
	return result, nil
}

func (a *app) porterAskTrust(ctx context.Context, env porterEnv, args []string) error {
	f := flags("porter ask-trust")
	sender := f.String("sender", "", "")
	from := f.String("from", "", "")
	text := f.String("text", "", "")
	timeout := f.Duration("timeout", 0, "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 0 || *sender == "" {
		return usageError("usage: comms porter ask-trust --sender MACHINE_ID:AGENT_ID [--from ALIAS] [--text ORDER] [--timeout 2m]; prints once|session|always|deny|timeout (exit 0|0|0|2|3)")
	}
	s, err := a.session(ctx, *from)
	if err != nil {
		return err
	}
	q := comms.PorterQuestionRequest{Kind: porter.NotifyTrust, SessionID: s.ID, Sender: *sender, Text: clipText(*text, 300), TimeoutMS: timeout.Milliseconds()}
	out, err := a.question(ctx, q)
	r := auditRecord{Kind: q.Kind, ID: out.ID, Agent: s.AgentID, Text: *sender, Mode: "cli", By: out.By}
	if err != nil {
		r.Result, r.Error = "error", err.Error()
		audit(env, r)
		return err
	}
	result := map[string]string{"Once": "once", "This session": "session", "Always": "always", "Deny": "deny"}[out.Choice]
	if out.State != "answered" || result == "" {
		result = "timeout"
	}
	r.Result = result
	audit(env, r)
	fmt.Fprintln(a.out, result)
	switch result {
	case "deny":
		return exitError{exitDeny}
	case "timeout":
		return exitError{exitTimeout}
	}
	return nil
}

func clipText(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
