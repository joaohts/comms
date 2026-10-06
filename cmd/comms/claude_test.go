package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestClaudeLauncherArguments(t *testing.T) {
	args := []string{"--resume", "thread", "--model", "user-model", "--mcp-config", "/other/config.json", "--", "prompt with `literal` $(text)"}
	for _, mode := range []string{"monitor", "channel"} {
		t.Run(mode, func(t *testing.T) {
			argv, env, err := claudeLaunchArguments("/bin/claude", "/path with spaces/comms", "/data", "/data/node.sock", mode, 321, args,
				[]string{"PATH=/bin", "CLAUDECODE=1", "COMMS_AGENT=parent", "CODEX_THREAD_ID=parent", "CLAUDE_CODE_SESSION_ID=parent", "COMMS_CHANNEL_ALIAS=parent"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(argv[1:7], args[:6]) || !reflect.DeepEqual(argv[len(argv)-2:], args[6:]) {
				t.Fatalf("changed caller args: %#v", argv)
			}
			if mode == "monitor" && !reflect.DeepEqual(argv[1:], args) {
				t.Fatalf("Monitor launch injected flags: %#v", argv)
			}
			if mode == "channel" {
				if !reflect.DeepEqual(argv[9:11], []string{"--dangerously-load-development-channels", "server:comms"}) {
					t.Fatalf("missing channel flags: %#v", argv)
				}
				var config struct {
					MCPServers map[string]struct {
						Command string
						Args    []string
					} `json:"mcpServers"`
				}
				if err := json.Unmarshal([]byte(argv[8]), &config); err != nil {
					t.Fatal(err)
				}
				server := config.MCPServers["comms"]
				if server.Command != "/path with spaces/comms" || !reflect.DeepEqual(server.Args, []string{"--data-dir", "/data", "--socket", "/data/node.sock", "channel"}) {
					t.Fatalf("wrong MCP config: %+v", server)
				}
			}
			for _, expected := range []string{"COMMS_CLAUDE_RECEIVER=" + mode, "COMMS_HARNESS_PID=321", "CLAUDECODE=1"} {
				if !slices.Contains(env, expected) {
					t.Fatalf("missing %s: %v", expected, env)
				}
			}
			if strings.Contains(strings.Join(env, "\n"), "=parent") {
				t.Fatalf("inherited parent identity: %v", env)
			}
			if slices.Contains(argv, "--dangerously-skip-permissions") || slices.Contains(argv, "--strict-mcp-config") {
				t.Fatal("launcher changed permissions or other MCPs")
			}
		})
	}
}

func TestClaudeLauncherUsesNodeSettingAndCallingDirectory(t *testing.T) {
	for _, mode := range []string{"monitor", "channel"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/claude" {
					t.Errorf("unexpected node request: %s", r.URL.Path)
				}
				json.NewEncoder(w).Encode(map[string]string{"receiver": mode})
			}))
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			calling := filepath.Join(dir, "notes with spaces")
			os.Mkdir(calling, 0700)
			testBin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
			script := "#!/bin/sh\nCOMMS_LAUNCHER_HELPER=claude exec " + quote(testBin) + " -test.run='^TestCodexLauncherHelperProcess$' -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(dir, "capture.json")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, testBin, "-test.run=^TestCodexLauncherHelperProcess$", "--", "--data-dir", dir, "--socket", a.c.Socket, "claude", "--resume", "thread", "--", "--json literal prompt")
			cmd.Dir = calling
			cmd.Env = setEnvironment(os.Environ(), "PATH", bin+":"+os.Getenv("PATH"))
			cmd.Env = setEnvironment(cmd.Env, "COMMS_LAUNCHER_HELPER", "launcher")
			cmd.Env = setEnvironment(cmd.Env, "COMMS_LAUNCHER_TEST_CAPTURE", capture)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("launch failed: %v: %s", err, out)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				PID         int
				Cwd         string
				Args        []string
				Environment map[string]string
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			wantCwd, _ := filepath.EvalSymlinks(calling)
			if got.Cwd != wantCwd || got.PID != cmd.Process.Pid || got.Environment["COMMS_CLAUDE_RECEIVER"] != mode {
				t.Fatalf("launcher changed cwd/PID/mode: %s", data)
			}
			if slices.Contains(got.Args, "--dangerously-load-development-channels") != (mode == "channel") {
				t.Fatalf("wrong mode flags: %v", got.Args)
			}
			if got.Args[len(got.Args)-1] != "--json literal prompt" {
				t.Fatalf("global flags consumed prompt: %v", got.Args)
			}
		})
	}
}

func TestClaudeAdministrativeAndNonInteractivePassthrough(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"mcp", "list"}, {"auth", "status"}, {"-p", "text"}, {"--safe-mode"}} {
		if !claudePassthrough(args) {
			t.Fatalf("expected passthrough: %v", args)
		}
	}
	for _, args := range [][]string{nil, {"--resume", "thread"}, {"--", "--help"}} {
		if claudePassthrough(args) {
			t.Fatalf("expected managed launch: %v", args)
		}
	}
	if _, _, err := claudeLaunchArguments("claude", "comms", "data", "socket", "channel", 1, []string{"--bg"}, nil); err == nil {
		t.Fatal("background launcher must not capture the wrong owner PID")
	}
}

func TestClaudeReceiverCLIAndChannelOpenGuidance(t *testing.T) {
	a, out := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/claude" {
			t.Fatalf("unexpected route %s", r.URL.Path)
		}
		if r.Method == "PUT" {
			var value map[string]string
			json.NewDecoder(r.Body).Decode(&value)
			if value["receiver"] != "channel" {
				t.Errorf("wrong setting: %v", value)
			}
		}
		w.Write([]byte(`{"receiver":"channel"}`))
	}))
	if err := a.run(context.Background(), []string{"claude-receiver", "channel"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "channel") {
		t.Fatal(out.String())
	}
	if err := a.run(context.Background(), []string{"claude-receiver", "unknown"}); err == nil {
		t.Fatal("invalid receiver accepted")
	}
	a.getenv = func(key string) string {
		switch key {
		case "COMMS_CLAUDE_RECEIVER":
			return "channel"
		case "CLAUDE_CODE_SESSION_ID":
			return "thread"
		}
		return ""
	}
	if err := a.open(context.Background(), []string{"worker"}); err == nil || !strings.Contains(err.Error(), "comms_open") {
		t.Fatalf("missing channel guidance: %v", err)
	}
}
