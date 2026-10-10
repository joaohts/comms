package porter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Approval modes.
const (
	ApprovalsAuto     = "auto"
	ApprovalsPhone    = "phone"
	ApprovalsTerminal = "terminal"
)

// Topics.
const (
	TopicAgents = "agents"
	TopicStatus = "status"
	TopicTrusts = "trusts"
)

var Topics = []string{TopicAgents, TopicStatus, TopicTrusts}

// Config is ~/.config/porter/config.toml. Share maps a topic to nil for "*"
// (every granted peer) or to the machine ids allowed to receive it.
type Config struct {
	Approvers     []string
	Approvals     string
	AskTimeout    time.Duration
	IdleThreshold time.Duration
	Share         map[string][]string
	Recap         bool // publish agent recaps set by integrations; off: never stored or sent
	Exists        bool
}

func DefaultConfig() Config {
	return Config{Approvals: ApprovalsAuto, AskTimeout: 2 * time.Minute, IdleThreshold: 2 * time.Minute,
		Share: map[string][]string{TopicAgents: nil, TopicStatus: nil}}
}

// ConfigPath is $PORTER_CONFIG or ~/.config/porter/config.toml.
func ConfigPath() string {
	if p := os.Getenv("PORTER_CONFIG"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "porter", "config.toml")
}

func (c Config) IsApprover(machine string) bool {
	for _, a := range c.Approvers {
		if a == machine {
			return true
		}
	}
	return false
}

// Shares reports whether topic may go to machine. trusts goes to approvers only.
func (c Config) Shares(topic, machine string) bool {
	if topic == TopicTrusts {
		return c.IsApprover(machine)
	}
	peers, ok := c.Share[topic]
	if !ok {
		return false
	}
	if peers == nil {
		return true
	}
	for _, p := range peers {
		if p == "*" || p == machine {
			return true
		}
	}
	return false
}

// LoadConfig reads the config. A missing file yields defaults with Exists false.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.Exists = true
	c.Share = map[string][]string{}
	if err := parseConfig(string(b), &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// parseConfig reads the small TOML subset porter writes: comments, one
// [share] table, and key = "string" | ["list", ...] values.
func parseConfig(src string, c *Config) error {
	section := ""
	for n, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(stripComment(line))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section != "share" {
				return fmt.Errorf("line %d: unknown table [%s]", n+1, section)
			}
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: expected key = value", n+1)
		}
		key = strings.Trim(strings.TrimSpace(key), `"`)
		if section == "" && key == "recap" {
			v, err := strconv.ParseBool(strings.Trim(strings.TrimSpace(raw), `"`))
			if err != nil {
				return fmt.Errorf("line %d: recap must be true or false", n+1)
			}
			c.Recap = v
			continue
		}
		list, str, isList, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("line %d: %v", n+1, err)
		}
		if section == "share" {
			if !isList && str != "*" {
				list, isList = []string{str}, true
			}
			if isList {
				c.Share[key] = list
			} else {
				c.Share[key] = nil
			}
			continue
		}
		switch key {
		case "approvers":
			if !isList {
				list = []string{str}
			}
			c.Approvers = list
		case "approvals":
			if str != ApprovalsAuto && str != ApprovalsPhone && str != ApprovalsTerminal {
				return fmt.Errorf("line %d: approvals must be auto, phone or terminal", n+1)
			}
			c.Approvals = str
		case "ask_timeout", "idle_threshold":
			d, err := time.ParseDuration(str)
			if err != nil || d <= 0 {
				return fmt.Errorf("line %d: %s must be a positive duration like \"2m\"", n+1, key)
			}
			if key == "ask_timeout" {
				c.AskTimeout = d
			} else {
				c.IdleThreshold = d
			}
		}
		// Unknown keys are ignored so newer configs keep working.
	}
	return nil
}

func stripComment(line string) string {
	in := false
	for i, r := range line {
		switch {
		case r == '"':
			in = !in
		case r == '#' && !in:
			return line[:i]
		}
	}
	return line
}

func parseValue(raw string) (list []string, str string, isList bool, err error) {
	if strings.HasPrefix(raw, "[") {
		if !strings.HasSuffix(raw, "]") {
			return nil, "", false, fmt.Errorf("unterminated list")
		}
		list = []string{}
		for _, part := range strings.Split(raw[1:len(raw)-1], ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			s, err := strconv.Unquote(part)
			if err != nil {
				return nil, "", false, fmt.Errorf("list items must be quoted strings")
			}
			list = append(list, s)
		}
		return list, "", true, nil
	}
	s, err := strconv.Unquote(raw)
	if err != nil {
		return nil, "", false, fmt.Errorf("value must be a quoted string or list")
	}
	return nil, s, false, nil
}

// Encode renders the config as TOML.
func (c Config) Encode() string {
	q := func(v []string) string {
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	var b strings.Builder
	b.WriteString("# porter config; see comms docs/PORTER.md. Peers are immutable machine ids.\n")
	fmt.Fprintf(&b, "approvers = %s\n", q(c.Approvers))
	fmt.Fprintf(&b, "approvals = %q\n", c.Approvals)
	fmt.Fprintf(&b, "ask_timeout = %q\n", shortDuration(c.AskTimeout))
	fmt.Fprintf(&b, "idle_threshold = %q\n", shortDuration(c.IdleThreshold))
	if c.Recap {
		b.WriteString("recap = true\n")
	}
	b.WriteString("\n[share]\n")
	keys := make([]string, 0, len(c.Share))
	for k := range c.Share {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if c.Share[k] == nil {
			fmt.Fprintf(&b, "%s = \"*\"\n", k)
		} else {
			fmt.Fprintf(&b, "%s = %s\n", k, q(c.Share[k]))
		}
	}
	return b.String()
}

func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// SaveConfig writes c to path with owner-only permissions.
func SaveConfig(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(c.Encode()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveApprover drops machine from the approvers of the config at path,
// rewriting only that line (comments, unknown keys and layout are kept) and
// replacing the file atomically. It reports whether machine was an approver.
// Nothing in porter ever adds an approver from a message; that is local setup.
func RemoveApprover(path, machine string) (bool, error) {
	l, err := lockFile(path)
	if err != nil {
		return false, err
	}
	defer l.Close()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(b), "\n")
	removed := false
	section := ""
	for i, line := range lines {
		code := stripComment(line)
		trimmed := strings.TrimSpace(code)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			continue
		}
		key, raw, ok := strings.Cut(trimmed, "=")
		if !ok || section != "" || strings.Trim(strings.TrimSpace(key), `"`) != "approvers" {
			continue
		}
		list, str, isList, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return false, fmt.Errorf("%s: approvers: %v", path, err)
		}
		if !isList {
			list = []string{str}
		}
		kept := []string{}
		for _, v := range list {
			if v == machine {
				removed = true
				continue
			}
			kept = append(kept, strconv.Quote(v))
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = indent + "approvers = [" + strings.Join(kept, ", ") + "]"
		if comment := line[len(code):]; comment != "" {
			lines[i] += " " + comment
		}
	}
	if !removed {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}
