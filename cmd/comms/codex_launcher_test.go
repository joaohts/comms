package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/joaohts/comms/internal/codex"
	"github.com/joaohts/comms/internal/comms"
	"golang.org/x/sys/unix"
)

// A real child process emulates only app-server initialization and the TUI.
// No model call or real Codex config/authentication change occurs in unit tests.
func TestCodexLauncherHelperProcess(t *testing.T) {
	mode := os.Getenv("COMMS_LAUNCHER_HELPER")
	if mode == "" {
		return
	}
	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	if mode == "launcher" {
		a := app{in: os.Stdin, out: os.Stdout, errOut: os.Stderr, getenv: os.Getenv}
		if err := a.run(context.Background(), args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(args) == 1 && args[0] == "--version" {
		v := os.Getenv("COMMS_LAUNCHER_TEST_VERSION")
		if v == "" {
			v = "0.154.0"
		}
		fmt.Println("codex-cli", v)
		os.Exit(0)
	}
	if len(args) > 0 && args[0] == "app-server" {
		if os.Getenv("COMMS_LAUNCHER_TEST_FAIL") == "1" {
			os.Exit(17)
		}
		path := strings.TrimPrefix(args[len(args)-1], "unix://")
		l, err := net.Listen("unix", path)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if err := os.Chmod(path, 0600); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("TEST_SERVER_STARTED", os.Getpid())
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			for {
				_, b, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				var req struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if json.Unmarshal(b, &req) != nil {
					return
				}
				if req.Method == "initialize" && os.Getenv("COMMS_LAUNCHER_TEST_STALL") != "1" {
					response, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]string{"userAgent": "codex/0.154.0"}})
					if ws.Write(r.Context(), websocket.MessageText, response) != nil {
						return
					}
				}
			}
		})
		if err := http.Serve(l, handler); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	values := map[string]string{}
	for _, key := range []string{"COMMS_HARNESS_PID", "COMMS_CODEX_TARGET", "COMMS_DATA_DIR", "COMMS_SOCKET", "COMMS_BIN", "COMMS_AGENT", "CODEX_THREAD_ID", "COMMS_CLAUDE_RECEIVER"} {
		values[key] = os.Getenv(key)
	}
	cwd, _ := os.Getwd()
	out, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "args": args, "environment": values, "cwd": cwd})
	if err := os.WriteFile(os.Getenv("COMMS_LAUNCHER_TEST_CAPTURE"), out, 0600); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	os.Exit(0)
}

func launcherFixture(t *testing.T) (string, string, []string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "comms-launch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	bin := filepath.Join(dir, "codex-bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	exe := filepath.Join(bin, "codex")
	script := "#!/bin/sh\nCOMMS_LAUNCHER_HELPER=codex exec " + quote(testBin) + " -test.run='^TestCodexLauncherHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	env := setEnvironment(os.Environ(), "PATH", bin+":"+os.Getenv("PATH"))
	return dir, exe, env
}

func cleanupOwnedLauncher(t *testing.T, data string) {
	t.Helper()
	t.Cleanup(func() {
		target := "unix://" + filepath.Join(data, "codex", "control.sock")
		record, err := readCodexRecord(filepath.Join(data, "codex", "server.json"), target)
		if err == nil && record != nil && comms.ProcessStamp(record.PID) == record.Started {
			unix.Kill(record.PID, unix.SIGTERM)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && !codexProcessEnded(*record) {
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
}

func TestCodexLauncherPreservesArgumentsAndControlsOnlyTransport(t *testing.T) {
	args := []string{"resume", "01a0a4cb-2147-7231-a8a5-d1e17b40c048", "-m", "user-selected-model", "-c", `model_reasoning_effort="high"`, "--", "a prompt with `literal` $(text)"}
	argv, env := codexLaunchArguments("/bin/codex", "/bin/comms", "/data with spaces", "/custom/node.sock", "unix:///data/control.sock", "/project", 123, args, []string{"PATH=/bin", "COMMS_AGENT=old", "COMMS_SESSION_ID=old", "CODEX_THREAD_ID=old", "AUTH_TEST_VALUE=preserved"})
	if !reflect.DeepEqual(argv[5:11], args[:6]) {
		t.Fatalf("Codex arguments changed: %#v", argv)
	}
	if !reflect.DeepEqual(argv[len(argv)-2:], args[len(args)-2:]) {
		t.Fatalf("prompt changed: %#v", argv)
	}
	for _, value := range []string{"-a", "-s", "--dangerously-bypass-approvals-and-sandbox", "--approve-for-me"} {
		for _, arg := range argv {
			if arg == value {
				t.Fatalf("launcher injected permission override %q", value)
			}
		}
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"COMMS_HARNESS_PID=123", "COMMS_CODEX_TARGET=unix:///data/control.sock", "COMMS_SOCKET=/custom/node.sock", "COMMS_BIN=/bin/comms", "AUTH_TEST_VALUE=preserved"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing env %q", want)
		}
	}
	for _, forbidden := range []string{"COMMS_AGENT=old", "COMMS_SESSION_ID=old", "CODEX_THREAD_ID=old"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("inherited stale identity %s", forbidden)
		}
	}
	if err := validateCodexArguments([]string{"resume", "x", "--remote=ws://elsewhere"}); err == nil {
		t.Fatal("allowed remote target override")
	}
	if err := validateCodexArguments([]string{"--", "--remote=literal prompt"}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexLauncherWorkingDirectory(t *testing.T) {
	const cwd = "/project with spaces and $literal"
	for _, tc := range []struct {
		name        string
		args        []string
		wantDefault bool
	}{
		{"new", nil, true},
		{"resume", []string{"resume", "thread"}, true},
		{"short", []string{"-C", "../chosen"}, false},
		{"short attached", []string{"-C../chosen"}, false},
		{"short equals", []string{"-C=../chosen"}, false},
		{"long", []string{"--cd", "/chosen project"}, false},
		{"long equals", []string{"--cd=/chosen project"}, false},
		{"resume override", []string{"resume", "thread", "-C", "/chosen"}, false},
		{"literal prompt", []string{"--", "--cd=/literal"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv, _ := codexLaunchArguments("/bin/codex", "/bin/comms", "/data", "/data/node.sock", "unix:///data/control.sock", cwd, 123, tc.args, nil)
			offset := 3
			if tc.wantDefault {
				if !reflect.DeepEqual(argv[offset:offset+2], []string{"--cd", cwd}) {
					t.Fatalf("missing launch directory: %#v", argv)
				}
				offset += 2
			}
			if len(tc.args) > 0 && tc.args[0] == "--" {
				if !reflect.DeepEqual(argv[len(argv)-len(tc.args):], tc.args) {
					t.Fatalf("literal prompt changed: %#v", argv)
				}
			} else if len(tc.args) > 0 && !reflect.DeepEqual(argv[offset:offset+len(tc.args)], tc.args) {
				t.Fatalf("explicit arguments changed: %#v", argv)
			}
		})
	}
}

func TestCodexLauncherExecKeepsPIDAndServiceSurvivesTUIExit(t *testing.T) {
	dir, _, environment := launcherFixture(t)
	data := filepath.Join(dir, "data")
	workspace := filepath.Join(dir, "project with spaces")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	cleanupOwnedLauncher(t, data)
	capture := filepath.Join(dir, "capture.json")
	environment = setEnvironment(environment, "COMMS_LAUNCHER_HELPER", "launcher")
	environment = setEnvironment(environment, "COMMS_LAUNCHER_TEST_CAPTURE", capture)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCodexLauncherHelperProcess$", "--", "--data-dir", data, "codex", "resume", "owned-thread", "--json", "-c", `model_reasoning_effort="high"`)
	cmd.Env = setEnvironment(environment, "PWD", workspace)
	cmd.Dir = workspace
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("launcher failed: %v %s", err, out.String())
	}
	var got struct {
		PID         int               `json:"pid"`
		Args        []string          `json:"args"`
		Environment map[string]string `json:"environment"`
	}
	b, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.PID != pid || got.Environment["COMMS_HARNESS_PID"] != fmt.Sprint(pid) {
		t.Fatalf("launcher did not exec with preserved PID: %+v", got)
	}
	if !strings.Contains(strings.Join(got.Args, " "), "--json") {
		t.Fatal("comms global parser stole a Codex argument")
	}
	if got.Environment["COMMS_DATA_DIR"] != data {
		t.Fatalf("wrong node data directory: %+v", got)
	}
	wantDirectory := []string{"--cd", workspace}
	if len(got.Args) < 4 || !reflect.DeepEqual(got.Args[2:4], wantDirectory) {
		t.Fatalf("launcher did not forward the invoking directory: %#v", got.Args)
	}
	target := got.Environment["COMMS_CODEX_TARGET"]
	if err := codex.ValidateServer(context.Background(), target); err != nil {
		t.Fatalf("server died with TUI: %v", err)
	}
	record, err := readCodexRecord(filepath.Join(data, "codex", "server.json"), target)
	if err != nil || record == nil {
		t.Fatalf("owner record: %+v %v", record, err)
	}
	pgid, err := unix.Getpgid(record.PID)
	if err != nil || pgid != record.PID {
		t.Fatalf("server not detached into own process group: pgid=%d pid=%d err=%v", pgid, record.PID, err)
	}
	if record.PID == pid {
		t.Fatal("server and TUI share lifecycle PID")
	}

	// A later launch from another folder must use that folder even though the
	// shared server was already started from its separate data directory.
	otherWorkspace := filepath.Join(dir, "another project")
	if err := os.Mkdir(otherWorkspace, 0700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestCodexLauncherHelperProcess$", "--", "--data-dir", data, "codex")
	cmd.Env = setEnvironment(environment, "PWD", otherWorkspace)
	cmd.Dir = otherWorkspace
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second launcher failed: %v %s", err, output)
	}
	b, err = os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	wantDirectory = []string{"--cd", otherWorkspace}
	if len(got.Args) < 4 || !reflect.DeepEqual(got.Args[2:4], wantDirectory) {
		t.Fatalf("reused server lost the new invoking directory: %#v", got.Args)
	}
	reused, err := readCodexRecord(filepath.Join(data, "codex", "server.json"), target)
	if err != nil || reused == nil || *reused != *record {
		t.Fatalf("second launcher replaced the server: %+v %v", reused, err)
	}
}

func TestCodexLauncherConcurrentStartupReusesOneServer(t *testing.T) {
	dir, exe, env := launcherFixture(t)
	data := filepath.Join(dir, "data")
	cleanupOwnedLauncher(t, data)
	var wg sync.WaitGroup
	errors := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ensureCodexServer(context.Background(), data, exe, env)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(data, "codex", "app-server.log"))
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(b, []byte("TEST_SERVER_STARTED")); count != 1 {
		t.Fatalf("started %d servers: %s", count, b)
	}
}

func TestCodexLauncherStaleSocketRequiresEndedOwner(t *testing.T) {
	dir, _, _ := launcherFixture(t)
	path := filepath.Join(dir, "control.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	live := &codexServerRecord{PID: os.Getpid(), Started: comms.ProcessStamp(os.Getpid()), Target: "unix://" + path}
	if err := removeStaleCodexSocket(context.Background(), path, live); err == nil {
		t.Fatal("removed a live listener")
	}
	l.Close()
	if err := removeStaleCodexSocket(context.Background(), path, nil); err == nil {
		t.Fatal("removed an unowned socket")
	}
	if err := removeStaleCodexSocket(context.Background(), path, live); err == nil {
		t.Fatal("removed a socket whose recorded owner is alive")
	}
	child := exec.Command("sleep", "0.1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	dead := &codexServerRecord{PID: child.Process.Pid, Started: comms.ProcessStamp(child.Process.Pid), Target: "unix://" + path}
	if dead.Started == "" {
		t.Fatal("missing test process start identity")
	}
	child.Wait()
	if err := removeStaleCodexSocket(context.Background(), path, dead); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket remains: %v", err)
	}
}

func TestCodexLauncherPrivateStateAndCancelledLock(t *testing.T) {
	dir, exe, env := launcherFixture(t)
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := privateCodexFile(filepath.Join(data, "codex", "start.lock"), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := ensureCodexServer(ctx, data, exe, env); err == nil {
		t.Fatal("ignored lock cancellation")
	}
	if _, err := os.Stat(filepath.Join(data, "codex", "server.json")); !os.IsNotExist(err) {
		t.Fatal("started through held lock")
	}
	other := filepath.Join(dir, "other")
	os.WriteFile(other, []byte("keep"), 0600)
	link := filepath.Join(dir, "symlink")
	os.Symlink(other, link)
	if f, err := privateCodexFile(link, unix.O_WRONLY); err == nil {
		f.Close()
		t.Fatal("followed runtime symlink")
	}
	if err := privateCodexDirectory(link, true); err == nil {
		t.Fatal("accepted non-directory runtime")
	}
}

func TestCodexLauncherStartupTimeoutDoesNotReplaceLiveServer(t *testing.T) {
	dir, exe, env := launcherFixture(t)
	data := filepath.Join(dir, "data")
	cleanupOwnedLauncher(t, data)
	env = setEnvironment(env, "COMMS_LAUNCHER_TEST_STALL", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	_, err := ensureCodexServer(ctx, data, exe, env)
	cancel()
	if err == nil {
		t.Fatal("stalled server reported ready")
	}
	target := "unix://" + filepath.Join(data, "codex", "control.sock")
	first, err := readCodexRecord(filepath.Join(data, "codex", "server.json"), target)
	if err != nil || first == nil {
		t.Fatalf("missing launched process record: %+v %v", first, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	_, err = ensureCodexServer(ctx, data, exe, env)
	cancel()
	if err == nil {
		t.Fatal("stalled server reported ready on retry")
	}
	second, err := readCodexRecord(filepath.Join(data, "codex", "server.json"), target)
	if err != nil {
		t.Fatal(err)
	}
	if *first != *second {
		t.Fatalf("readiness failure replaced a live process: %+v %+v", first, second)
	}
}

func TestCodexLauncherVersionAndHelpDoNotStartServer(t *testing.T) {
	dir, exe, _ := launcherFixture(t)
	if err := checkCodexVersion(context.Background(), exe); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMS_LAUNCHER_TEST_VERSION", "0.153.0")
	if err := checkCodexVersion(context.Background(), exe); err == nil {
		t.Fatal("accepted unsupported Codex version")
	}
	t.Setenv("COMMS_LAUNCHER_TEST_VERSION", "0.154.0")
	data := filepath.Join(dir, "help-data")
	capture := filepath.Join(dir, "help.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCodexLauncherHelperProcess$", "--", "--data-dir", data, "codex", "--help")
	cmd.Env = setEnvironment(setEnvironment(setEnvironment(os.Environ(), "COMMS_LAUNCHER_HELPER", "launcher"), "COMMS_LAUNCHER_TEST_CAPTURE", capture), "PATH", filepath.Dir(exe)+":"+os.Getenv("PATH"))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(data, "codex")); !os.IsNotExist(err) {
		t.Fatalf("help started runtime: %v", err)
	}
}

func TestCodexOpenRequiresLiveExplicitOwner(t *testing.T) {
	for _, mode := range []string{"missing", "ended", "mismatched", "valid"} {
		t.Run(mode, func(t *testing.T) {
			called := false
			a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				var req comms.OpenRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.ProcessID != os.Getpid() || req.ProcessStarted != comms.ProcessStamp(os.Getpid()) {
					t.Errorf("owner identity was not verified: %+v", req)
				}
				json.NewEncoder(w).Encode(comms.OpenResponse{})
			}))
			a.getenv = func(key string) string {
				switch key {
				case "CODEX_THREAD_ID":
					return "01a0a4cb-2147-7231-a8a5-d1e17b40c048"
				case "COMMS_CODEX_TARGET":
					return "unix:///test/control.sock"
				}
				return ""
			}
			args := []string{"open", "test"}
			switch mode {
			case "ended":
				args = append(args, "--process-id", "2147483646")
			case "mismatched":
				args = append(args, "--process-id", fmt.Sprint(os.Getpid()), "--process-started", "wrong")
			case "valid":
				args = append(args, "--process-id", fmt.Sprint(os.Getpid()))
			}
			err := a.run(context.Background(), args)
			if mode == "valid" {
				if err != nil || !called {
					t.Fatalf("valid owner rejected: %v", err)
				}
			} else if err == nil || called {
				t.Fatalf("unverified owner reached node: %v called=%t", err, called)
			}
		})
	}
}
