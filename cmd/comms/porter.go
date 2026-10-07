package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joaohts/comms/internal/porter"
)

// porter is local-only for now: it records agent state reported by
// integrations and does not need the node.
func (a *app) porter(dataDir string, args []string) error {
	if len(args) == 0 {
		return usageError("porter requires a subcommand: event | status")
	}
	st := porter.Store{Path: filepath.Join(dataDir, "porter", "state.json")}
	switch args[0] {
	case "event":
		return a.porterEvent(st, args[1:])
	case "status":
		return a.porterStatus(st, args[1:])
	}
	return usageError("unknown porter subcommand " + strconv.Quote(args[0]))
}

func (a *app) porterEvent(st porter.Store, args []string) error {
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
	var out *porter.Agent
	err := st.Update(func(s *porter.State) error {
		ag, err := s.Apply(e)
		if ag != nil {
			c := *ag
			out = &c
		}
		return err
	})
	if err != nil || !a.jsonOutput {
		return err
	}
	if out == nil {
		return a.output(map[string]any{"removed": e.Agent})
	}
	return a.output(out)
}

func (a *app) porterStatus(st porter.Store, args []string) error {
	if len(args) > 0 {
		return usageError("porter status takes no arguments")
	}
	s, err := st.Load()
	if err != nil {
		return err
	}
	list := s.List()
	if a.jsonOutput {
		return a.output(map[string]any{"agents": list})
	}
	now := time.Now()
	w := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tAGENT\tHARNESS\tPROJECT\tFOR\tRUN\tWAIT\tSUMMARY")
	for _, ag := range list {
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
		mark := ""
		if ag.Special {
			mark = "*"
		}
		summary := ag.Summary
		if ag.Needs != nil {
			summary = strings.TrimSpace(ag.Needs.Kind + ": " + ag.Needs.Text)
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ag.Status, mark, name, ag.Harness, ag.Project,
			dur(cur), dur(run), dur(wait), summary)
	}
	return w.Flush()
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
