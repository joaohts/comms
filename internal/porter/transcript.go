package porter

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"time"
)

// Away detection, as in the Mac Agent Monitor: a running agent whose
// transcript has been silent for AwayAfter, with no tool call in flight, is
// away; a transcript write after that brings it back to running.
const (
	AwayAfter      = 60 * time.Second
	AwayCheck      = 5 * time.Second // at most one look per agent this often
	TranscriptTail = 64 << 10        // bytes read for the pending-tool check
)

// AwayVerdict decides a running/away agent's next status from its transcript
// mtime, its current status period and whether a tool call is pending (only
// consulted when the agent would otherwise go away). It returns the new
// status and whether it changes.
func AwayVerdict(a Agent, mtime, now time.Time, pending func() bool) (string, bool) {
	switch a.Status {
	case StatusRunning:
		last := mtime
		if a.StatusSince.After(last) {
			last = a.StatusSince
		}
		if now.Sub(last) > AwayAfter && !pending() {
			return StatusAway, true
		}
	case StatusAway:
		if mtime.After(a.StatusSince.Add(time.Second)) {
			return StatusRunning, true
		}
	}
	return a.Status, false
}

// ToolPending reports whether the transcript's tail ends with a tool call
// that has no result yet: a Claude tool_use without a matching tool_result,
// or a Codex function/custom tool call without its output.
func ToolPending(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	off := info.Size() - TranscriptTail
	if off < 0 {
		off = 0
	}
	b, err := io.ReadAll(io.NewSectionReader(f, off, info.Size()-off))
	if err != nil {
		return false
	}
	if off > 0 {
		// Drop the partial first line.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			return false
		}
	}
	return pendingIn(b)
}

func pendingIn(b []byte) bool {
	pending := map[string]bool{}
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var v struct {
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Type    string `json:"type"`
			Payload *struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &v) != nil {
			continue // a line caught mid-write
		}
		if v.Message != nil {
			var items []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			}
			if json.Unmarshal(v.Message.Content, &items) == nil {
				for _, it := range items {
					switch {
					case it.Type == "tool_use" && it.ID != "":
						pending[it.ID] = true
					case it.Type == "tool_result" && it.ToolUseID != "":
						delete(pending, it.ToolUseID)
					}
				}
			}
		}
		if v.Type == "response_item" && v.Payload != nil && v.Payload.CallID != "" {
			switch v.Payload.Type {
			case "function_call", "custom_tool_call", "local_shell_call":
				pending[v.Payload.CallID] = true
			case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
				delete(pending, v.Payload.CallID)
			}
		}
	}
	return len(pending) > 0
}
