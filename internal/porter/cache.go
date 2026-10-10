package porter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// CacheEntry is the latest value of one topic from one peer machine.
type CacheEntry struct {
	MachineID string            `json:"machine_id"`
	Machine   string            `json:"machine"`
	Topic     string            `json:"topic"`
	UpdatedAt time.Time         `json:"updated_at"`
	Items     []json.RawMessage `json:"items,omitempty"`
	Value     json.RawMessage   `json:"value,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
}

// Cache keeps peers' topic values in <dir>/<machine id>/<topic>.json. Only
// the node writes it; readers see atomically replaced files.
type Cache struct{ Dir string }

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (c Cache) path(machineID, topic string) (string, error) {
	if !safeName.MatchString(machineID) || !safeName.MatchString(topic) {
		return "", fmt.Errorf("invalid cache key")
	}
	return filepath.Join(c.Dir, machineID, topic+".json"), nil
}

// Apply folds a snapshot, update or remove from machineID into the cache.
func (c Cache) Apply(machineID string, in Inbound) error {
	path, err := c.path(machineID, in.Topic)
	if err != nil {
		return err
	}
	e, err := readJSON[CacheEntry](path)
	if err != nil {
		e = &CacheEntry{}
	}
	e.MachineID, e.Topic, e.UpdatedAt = machineID, in.Topic, time.Now().UTC()
	if in.Machine != "" {
		e.Machine = in.Machine
	}
	switch in.Type {
	case TypeSnapshot:
		e.Items, e.Value, e.Truncated = in.Items, in.Value, false
		if in.Value == nil && e.Items == nil {
			e.Items = []json.RawMessage{}
		}
	case TypeUpdate:
		if in.Value != nil {
			e.Value = in.Value
			break
		}
		id := itemID(in.Item)
		if id == "" {
			return fmt.Errorf("update item has no id")
		}
		replaced := false
		for i, raw := range e.Items {
			if itemID(raw) == id {
				e.Items[i], replaced = in.Item, true
			}
		}
		if !replaced {
			e.Items = append(e.Items, in.Item)
		}
	case TypeRemove:
		kept := e.Items[:0]
		for _, raw := range e.Items {
			if itemID(raw) != in.ID {
				kept = append(kept, raw)
			}
		}
		e.Items = kept
	default:
		return fmt.Errorf("not a topic message")
	}
	return writeJSON(path, e)
}

func itemID(raw json.RawMessage) string {
	var v struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &v)
	return v.ID
}

// Load returns one cached topic, or nil when nothing is cached.
func (c Cache) Load(machineID, topic string) (*CacheEntry, error) {
	path, err := c.path(machineID, topic)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	return readJSON[CacheEntry](path)
}

// Machines lists cached machine ids.
func (c Cache) Machines() []string {
	entries, _ := os.ReadDir(c.Dir)
	out := []string{}
	for _, e := range entries {
		if e.IsDir() && safeName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
