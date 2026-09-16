package comms

import (
	"database/sql"
	"errors"
)

const (
	ClaudeReceiverMonitor = "monitor"
	ClaudeReceiverChannel = "channel"
	ClaudeChannelTarget   = "claude-channel"
)

type ClaudeSettings struct {
	Receiver string `json:"receiver"`
}

func ValidClaudeReceiver(receiver string) bool {
	return receiver == ClaudeReceiverMonitor || receiver == ClaudeReceiverChannel
}

func (s *Store) ClaudeSettings() (ClaudeSettings, error) {
	var receiver string
	err := s.DB.QueryRow(`SELECT value FROM settings WHERE key='claude_receiver'`).Scan(&receiver)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaudeSettings{Receiver: ClaudeReceiverMonitor}, nil
	}
	if err != nil {
		return ClaudeSettings{}, err
	}
	if !ValidClaudeReceiver(receiver) {
		return ClaudeSettings{}, problem(500, "invalid_claude_receiver", "stored Claude receiver must be monitor or channel")
	}
	return ClaudeSettings{Receiver: receiver}, nil
}
