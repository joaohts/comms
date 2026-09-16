package main

import (
	"encoding/json"

	"github.com/joaohts/comms/internal/comms"
)

func compactOutcome(m comms.Message) map[string]any {
	result := map[string]any{"id": m.ID, "state": m.State}
	if m.Failure != "" {
		result["failure_code"] = m.Failure
	}
	return result
}

func (a *app) compactList(kind string, raw json.RawMessage) error {
	switch kind {
	case "who":
		var items []comms.Presence
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(items))
		for _, p := range items {
			out = append(out, map[string]any{"address": p.Address(), "recipient": p.MachineID + ":" + p.AgentID, "online": p.Online, "persistent": p.Persistent})
		}
		return a.output(out)
	case "agents":
		var items []comms.Agent
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(items))
		for _, p := range items {
			item := map[string]any{"id": p.ID, "alias": p.Alias, "persistent": p.Persistent, "scope": p.Scope, "online": p.Online}
			if p.RetiredAt != nil {
				item["retired"] = true
			}
			out = append(out, item)
		}
		return a.output(out)
	case "status":
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if _, message := fields["id"]; message {
			var m comms.Message
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			return a.output(compactOutcome(m))
		}
		out := map[string]json.RawMessage{}
		for _, key := range []string{"version", "api_version", "name", "machine_id", "broker_enabled", "broker_connected"} {
			if value, ok := fields[key]; ok {
				out[key] = value
			}
		}
		return a.output(out)
	default:
		return usageError("compact output is unavailable for " + kind)
	}
}
