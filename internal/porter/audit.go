package porter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// AuditRecord is one line of <data-dir>/porter/audit.jsonl: approvals asked
// by hooks and the CLI, and approver changes made by the node.
type AuditRecord struct {
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

// AppendAudit appends r to the audit log at path, best effort.
func AppendAudit(path string, r AuditRecord) {
	r.At = time.Now().UTC().Truncate(time.Second)
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
