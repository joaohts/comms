package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/codex"
	"github.com/joaohts/comms/internal/comms"
	"golang.org/x/sys/unix"
)

const codexStartupTimeout = 20 * time.Second

type codexServerRecord struct {
	PID     int    `json:"pid"`
	Started string `json:"process_started"`
	Target  string `json:"target"`
}

func (a *app) codex(ctx context.Context, cfg comms.Config, socket string, args []string) error {
	executable, err := exec.LookPath("codex")
	if err != nil {
		return &client.Error{Code: "codex_unavailable", Message: "install Codex CLI 0.154.0 or newer before running comms codex"}
	}
	if err := validateCodexArguments(args); err != nil {
		return err
	}
	// Reading help/version must not create a background service.
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "--version" || args[0] == "-V") {
		return syscall.Exec(executable, append([]string{executable}, args...), os.Environ())
	}
	if err := checkCodexVersion(ctx, executable); err != nil {
		return err
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return err
	}
	if socket == "" {
		socket = filepath.Join(dataDir, "node.sock")
	}
	socket, err = filepath.Abs(socket)
	if err != nil {
		return err
	}
	target, err := ensureCodexServer(ctx, dataDir, executable, cleanCodexEnvironment(os.Environ()))
	if err != nil {
		return err
	}
	commsBin, err := os.Executable()
	if err != nil {
		return err
	}
	argv, environment := codexLaunchArguments(executable, commsBin, dataDir, socket, target, os.Getpid(), args, os.Environ())
	if err := ctx.Err(); err != nil {
		return err
	}
	// No CommandContext or child wait loop: this PID becomes the real TUI.
	// The detached app-server has its own session and survives TUI/node exit.
	return syscall.Exec(executable, argv, environment)
}

func validateCodexArguments(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--remote" || strings.HasPrefix(arg, "--remote=") || arg == "--remote-auth-token-env" || strings.HasPrefix(arg, "--remote-auth-token-env=") {
			return usageError("comms codex manages its private local --remote target; use codex directly for other remote endpoints")
		}
	}
	if len(args) > 0 {
		switch args[0] {
		case "app-server", "exec", "e", "login", "logout", "mcp", "mcp-server", "queue":
			return usageError("comms codex launches the interactive terminal or resume; use codex directly for " + args[0])
		}
	}
	return nil
}

func checkCodexVersion(ctx context.Context, executable string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, executable, "--version").Output()
	if err != nil {
		return fmt.Errorf("read Codex version: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return errors.New("cannot identify installed Codex version")
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(fields[len(fields)-1], "%d.%d.%d", &major, &minor, &patch); err != nil || major == 0 && minor < 154 {
		return errors.New("native comms delivery requires Codex CLI 0.154.0 or newer")
	}
	return nil
}

func codexLaunchArguments(executable, commsBin, dataDir, socket, target string, pid int, args, environment []string) ([]string, []string) {
	values := []struct{ key, value string }{
		{"COMMS_HARNESS_PID", strconv.Itoa(pid)},
		{"COMMS_CODEX_TARGET", target},
		{"COMMS_DATA_DIR", dataDir},
		{"COMMS_SOCKET", socket},
		{"COMMS_BIN", commsBin},
	}
	// Place our overrides after user config overrides but before an optional
	// end-of-options marker. Preserve every original argument verbatim.
	boundary := len(args)
	for i, arg := range args {
		if arg == "--" {
			boundary = i
			break
		}
	}
	argv := []string{executable, "--remote", target}
	argv = append(argv, args[:boundary]...)
	environment = cleanCodexEnvironment(environment)
	for _, entry := range values {
		value, _ := json.Marshal(entry.value)
		argv = append(argv, "-c", "shell_environment_policy.set."+entry.key+"="+string(value))
		environment = setEnvironment(environment, entry.key, entry.value)
	}
	argv = append(argv, args[boundary:]...)
	return argv, environment
}

func cleanCodexEnvironment(environment []string) []string {
	clean := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "CODEX_THREAD_ID", "CLAUDE_CODE_SESSION_ID", "COMMS_AGENT", "COMMS_SESSION_ID", "COMMS_ALIAS", "COMMS_HARNESS_PID", "COMMS_CODEX_TARGET":
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func setEnvironment(environment []string, key, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func ensureCodexServer(parent context.Context, dataDir, executable string, environment []string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, codexStartupTimeout)
	defer cancel()
	if err := privateCodexDirectory(dataDir, false); err != nil {
		return "", err
	}
	dir := filepath.Join(dataDir, "codex")
	if err := privateCodexDirectory(dir, true); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "control.sock")
	// sockaddr_un's portable limit includes the trailing NUL on macOS.
	if len(path) > 103 {
		return "", errors.New("Codex socket path is too long; choose a shorter COMMS_DATA_DIR")
	}
	target := "unix://" + path
	lock, err := privateCodexFile(filepath.Join(dir, "start.lock"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return "", err
		}
		if err := pause(ctx, 50*time.Millisecond); err != nil {
			return "", fmt.Errorf("waiting for Codex startup lock: %w", err)
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	if probeCodexServer(ctx, target) == nil {
		return target, nil
	}
	record, err := readCodexRecord(filepath.Join(dir, "server.json"), target)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := removeStaleCodexSocket(ctx, path, record); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if record != nil && !codexProcessEnded(*record) {
		// A recorded process may still be starting. Never create a second one
		// merely because a protocol readiness deadline elapsed.
		return waitCodexServer(ctx, target, nil)
	}
	logFile, err := privateCodexFile(filepath.Join(dir, "app-server.log"), unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND)
	if err != nil {
		return "", err
	}
	defer logFile.Close()
	cmd := exec.Command(executable, "app-server", "--listen", target)
	cmd.Dir = dataDir
	cmd.Env = environment
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start Codex app-server: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stamp := comms.ProcessStamp(cmd.Process.Pid)
	if stamp == "" {
		return "", fmt.Errorf("started Codex PID %d but cannot verify its process identity; inspect %s", cmd.Process.Pid, filepath.Join(dir, "app-server.log"))
	}
	record = &codexServerRecord{PID: cmd.Process.Pid, Started: stamp, Target: target}
	if err := writeCodexRecord(dir, *record); err != nil {
		return "", fmt.Errorf("Codex started but its owner record could not be saved: %w", err)
	}
	return waitCodexServer(ctx, target, done)
}

func probeCodexServer(parent context.Context, target string) error {
	ctx, cancel := context.WithTimeout(parent, 750*time.Millisecond)
	defer cancel()
	return codex.ValidateServer(ctx, target)
}

func waitCodexServer(ctx context.Context, target string, done <-chan error) (string, error) {
	for {
		if err := probeCodexServer(ctx, target); err == nil {
			return target, nil
		}
		select {
		case err := <-done:
			return "", fmt.Errorf("Codex app-server exited during startup (%v); inspect codex/app-server.log", err)
		default:
		}
		if err := pause(ctx, 100*time.Millisecond); err != nil {
			return "", fmt.Errorf("Codex app-server did not become ready: %w; inspect codex/app-server.log", err)
		}
	}
}

func privateCodexDirectory(path string, strict bool) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("Codex runtime directory must be owned by the current user and not writable by others: %s", path)
	}
	if strict && info.Mode().Perm()&0077 != 0 {
		return os.Chmod(path, 0700)
	}
	return nil
}

func privateCodexFile(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, fmt.Errorf("Codex runtime file must be private and owned by the current user: %s", path)
	}
	return f, nil
}

func readCodexRecord(path, target string) (*codexServerRecord, error) {
	f, err := privateCodexFile(path, unix.O_RDONLY)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var record codexServerRecord
	if err := json.NewDecoder(io.LimitReader(f, 16<<10)).Decode(&record); err != nil {
		return nil, fmt.Errorf("invalid Codex owner record: %w", err)
	}
	if record.PID <= 1 || record.Started == "" || record.Target != target {
		return nil, errors.New("Codex owner record does not match this runtime")
	}
	return &record, nil
}

func writeCodexRecord(dir string, record codexServerRecord) error {
	f, err := os.CreateTemp(dir, ".server-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(record); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "server.json"))
}

func codexProcessEnded(record codexServerRecord) bool {
	if errors.Is(unix.Kill(record.PID, 0), unix.ESRCH) {
		return true
	}
	current := comms.ProcessStamp(record.PID)
	return current != "" && current != record.Started
}

func removeStaleCodexSocket(ctx context.Context, path string, record *codexServerRecord) error {
	if err := codex.ValidateTarget("unix://" + path); err != nil {
		return err
	}
	probe, err := (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext(ctx, "unix", path)
	if err == nil {
		probe.Close()
		return errors.New("existing Codex listener is reachable but not ready; refusing to replace it")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot establish that Codex socket is stale: %w", err)
	}
	if record == nil || record.Target != "unix://"+path || !codexProcessEnded(*record) {
		return errors.New("Codex socket has no confirmed ended owner; refusing to replace it")
	}
	return os.Remove(path)
}
