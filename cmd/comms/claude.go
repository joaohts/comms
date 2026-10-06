package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
)

func (a *app) claudeReceiver(ctx context.Context, args []string) error {
	switch len(args) {
	case 0:
		var settings comms.ClaudeSettings
		if err := a.c.Do(ctx, "GET", "/v1/claude", nil, &settings); err != nil {
			return err
		}
		return a.output(settings)
	case 1:
		if comms.ValidClaudeReceiver(args[0]) {
			return a.mutation(ctx, "PUT", "/v1/claude", comms.ClaudeSettings{Receiver: args[0]})
		}
	}
	return usageError("usage: comms claude-receiver [monitor|channel]")
}

func (a *app) claude(ctx context.Context, cfg comms.Config, socket string, args []string) error {
	executable, err := exec.LookPath("claude")
	if err != nil {
		return &client.Error{Code: "claude_unavailable", Message: "install Claude Code before running comms claude"}
	}
	if claudePassthrough(args) {
		return syscall.Exec(executable, append([]string{executable}, args...), cleanClaudeEnvironment(os.Environ()))
	}
	var settings comms.ClaudeSettings
	if err := a.c.Do(ctx, "GET", "/v1/claude", nil, &settings); err != nil {
		return fmt.Errorf("read Claude receiver setting (requires a node with Claude receiver support): %w", err)
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return err
	}
	socket, err = filepath.Abs(socket)
	if err != nil {
		return err
	}
	commsBin, err := os.Executable()
	if err != nil {
		return err
	}
	argv, environment, err := claudeLaunchArguments(executable, commsBin, dataDir, socket, settings.Receiver, os.Getpid(), args, os.Environ())
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Replacing this process preserves the calling directory and normal TUI
	// signal handling. The MCP server is owned by Claude, not a detached daemon.
	return syscall.Exec(executable, argv, environment)
}

func claudePassthrough(args []string) bool {
	if len(args) > 0 {
		switch args[0] {
		case "agents", "attach", "auth", "auto-mode", "doctor", "gateway", "import", "install", "logs", "mcp", "plugin", "plugins", "project", "respawn", "rm", "setup-token", "stop", "kill", "ultrareview", "update", "upgrade":
			return true
		}
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		switch arg {
		case "-h", "--help", "-v", "--version", "-p", "--print", "--safe-mode":
			return true
		}
	}
	return false
}

func cleanClaudeEnvironment(environment []string) []string {
	clean := cleanCodexEnvironment(environment)
	result := make([]string, 0, len(clean))
	for _, entry := range clean {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "COMMS_CLAUDE_RECEIVER", "COMMS_CHANNEL_ALIAS", "COMMS_CHANNEL_PERSISTENT":
			continue
		}
		result = append(result, entry)
	}
	return result
}

func claudeLaunchArguments(executable, commsBin, dataDir, socket, receiver string, pid int, args, environment []string) ([]string, []string, error) {
	if !comms.ValidClaudeReceiver(receiver) {
		return nil, nil, usageError("Claude receiver must be monitor or channel")
	}
	boundary := len(args)
	for i, arg := range args {
		if arg == "--" {
			boundary = i
			break
		}
		if receiver == comms.ClaudeReceiverChannel {
			switch strings.SplitN(arg, "=", 2)[0] {
			case "--bg", "--background", "--cloud", "--environment", "--teleport", "--tmux":
				return nil, nil, usageError("comms channel mode supports local foreground Claude sessions; use command claude for background/cloud launchers")
			}
		}
	}
	environment = cleanClaudeEnvironment(environment)
	for key, value := range map[string]string{
		"COMMS_HARNESS_PID": strconv.Itoa(pid), "COMMS_CLAUDE_RECEIVER": receiver,
		"COMMS_DATA_DIR": dataDir, "COMMS_SOCKET": socket, "COMMS_BIN": commsBin,
	} {
		environment = setEnvironment(environment, key, value)
	}
	argv := append([]string{executable}, args[:boundary]...)
	if receiver == comms.ClaudeReceiverChannel {
		config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"comms": map[string]any{
			"command": commsBin, "args": []string{"--data-dir", dataDir, "--socket", socket, "channel"},
		}}})
		if err != nil {
			return nil, nil, err
		}
		// Variadic Claude options go after the caller's prompt, before --, so
		// neither a prompt nor another MCP configuration becomes a flag value.
		argv = append(argv, "--mcp-config", string(config), "--dangerously-load-development-channels", "server:comms")
	}
	argv = append(argv, args[boundary:]...)
	return argv, environment, nil
}
