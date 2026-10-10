package porter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestErrorStatusDetailDurationsAndPush(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(0)})
	before := *s.Agents["a"]
	a := mustApply(t, s, Event{Agent: "a", Kind: KindError, At: at(10), Error: &ErrorInfo{Kind: "rate_limit"}})
	if a.Status != StatusError || a.Error == nil || a.Error.Kind != "rate_limit" || a.RunningMS != 10_000 {
		t.Fatalf("error: %+v", a)
	}
	if PushWanted(&before, *a) {
		t.Fatal("normal error pushed")
	}
	b, _ := json.Marshal(a.Published(false))
	if !strings.Contains(string(b), `"error":{"kind":"rate_limit"}`) {
		t.Fatalf("wire: %s", b)
	}
	if n := NoticeFor("m", *a); n.Error == nil || n.Error.Kind != "rate_limit" {
		t.Fatalf("notice: %+v", n)
	}
	// Error time counts as neither running nor waiting; a prompt resumes.
	a = mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(40)})
	if a.Status != StatusRunning || a.Error != nil || a.RunningMS != 10_000 || a.WaitingMS != 0 {
		t.Fatalf("resume: %+v", a)
	}
	// Special agents push on error as on stop; a missing kind is "unknown".
	yes := true
	mustApply(t, s, Event{Agent: "a", Kind: KindUpdate, At: at(41), Special: &yes})
	before = *s.Agents["a"]
	a = mustApply(t, s, Event{Agent: "a", Kind: KindError, At: at(50)})
	if !PushWanted(&before, *a) || a.Error.Kind != "unknown" {
		t.Fatalf("special error: %+v", a)
	}
}

func TestAwayCountsAsRunningAndTranscriptStaysLocal(t *testing.T) {
	s := NewState()
	mustApply(t, s, Event{Agent: "a", Kind: KindPrompt, At: at(0), Transcript: "/t/a.jsonl"})
	if s.Transcripts["a"] != "/t/a.jsonl" {
		t.Fatalf("transcript: %+v", s.Transcripts)
	}
	b, _ := json.Marshal(s.Agents["a"].Published(true))
	if strings.Contains(string(b), "a.jsonl") {
		t.Fatalf("transcript published: %s", b)
	}
	if !s.SetAway("a", true, at(70)) || s.Agents["a"].Status != StatusAway || s.Running() != 1 {
		t.Fatalf("away: %+v", s.Agents["a"])
	}
	if s.SetAway("a", true, at(80)) {
		t.Fatal("away twice")
	}
	if !s.SetAway("a", false, at(100)) || s.Agents["a"].Status != StatusRunning || !s.Agents["a"].LastActiveAt.Equal(at(100)) {
		t.Fatalf("resume: %+v", s.Agents["a"])
	}
	s.SetAway("a", true, at(170))
	a := mustApply(t, s, Event{Agent: "a", Kind: KindStop, At: at(200)})
	if a.RunningMS != 200_000 {
		t.Fatalf("running_ms=%d", a.RunningMS)
	}
	if s.SetAway("a", true, at(300)) || s.SetAway("a", false, at(300)) {
		t.Fatal("only running/away flip")
	}
	mustApply(t, s, Event{Agent: "a", Kind: KindEnd, At: at(400)})
	if _, ok := s.Transcripts["a"]; ok {
		t.Fatal("ended agent keeps its transcript path")
	}
}

func TestAwayVerdict(t *testing.T) {
	pending, idle := func() bool { return true }, func() bool { return false }
	run := Agent{Status: StatusRunning, StatusSince: at(0)}
	if st, ok := AwayVerdict(run, at(0), at(61), idle); !ok || st != StatusAway {
		t.Fatal("silent 61s → away")
	}
	if _, ok := AwayVerdict(run, at(0), at(59), idle); ok {
		t.Fatal("59s is not away")
	}
	if _, ok := AwayVerdict(run, at(0), at(120), pending); ok {
		t.Fatal("a pending tool call is not away")
	}
	// An old transcript does not count before the running period began.
	if _, ok := AwayVerdict(Agent{Status: StatusRunning, StatusSince: at(100)}, at(0), at(130), idle); ok {
		t.Fatal("away measured from the prompt")
	}
	away := Agent{Status: StatusAway, StatusSince: at(100)}
	if _, ok := AwayVerdict(away, at(100), at(200), idle); ok {
		t.Fatal("no fresh write")
	}
	if st, ok := AwayVerdict(away, at(105), at(200), idle); !ok || st != StatusRunning {
		t.Fatal("fresh write → running")
	}
}

func TestToolPending(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	use := `{"type":"assistant","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t1","name":"Bash"}]}}`
	result := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}`
	text := `{"type":"user","message":{"content":"hello"}}`
	if !ToolPending(write(text, use)) {
		t.Fatal("tool_use without result")
	}
	if ToolPending(write(use, result, `{"partial`)) {
		t.Fatal("matched tool_use")
	}
	if !ToolPending(write(`{"type":"response_item","payload":{"type":"function_call","call_id":"c1"}}`)) {
		t.Fatal("codex call pending")
	}
	if ToolPending(write(`{"type":"response_item","payload":{"type":"function_call","call_id":"c1"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c1"}}`)) {
		t.Fatal("codex call answered")
	}
	if ToolPending(filepath.Join(dir, "missing")) {
		t.Fatal("missing file")
	}
	// Only the tail is read: a pending call far back is beyond it, a recent one is not.
	pad := `{"type":"user","message":{"content":"` + strings.Repeat("x", 1000) + `"}}`
	lines := []string{use}
	for i := 0; i < TranscriptTail/1000+10; i++ {
		lines = append(lines, pad)
	}
	if ToolPending(write(lines...)) {
		t.Fatal("read beyond the tail")
	}
	if !ToolPending(write(append(lines, use)...)) {
		t.Fatal("recent call in a big file")
	}
}
