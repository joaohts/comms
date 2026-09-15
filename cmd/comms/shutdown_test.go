package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
)

// This deliberately starts the compiled command and sends a real OS signal.
// Testing Node.Close alone cannot detect serve accidentally giving Node.Start
// the signal-cancelled context, which aborts the handoff before draining begins.
func TestServeSIGTERMDrainsActiveHandoff(t *testing.T) {
	directory, err := os.MkdirTemp("", "comms-drain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	binary := filepath.Join(directory, "comms")
	buildCtx, stopBuild := context.WithTimeout(context.Background(), 90*time.Second)
	defer stopBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real CLI: %v\n%s", err, output)
	}

	dataDir := filepath.Join(directory, "data")
	logPath := filepath.Join(directory, "serve.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "serve", "--data-dir", dataDir, "--workers", "1", "--drain", "5s")
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var exitErr error
	go func() {
		exitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = command.Process.Kill()
			<-exited
		}
		log.Close()
		if t.Failed() {
			if output, err := os.ReadFile(logPath); err == nil {
				t.Logf("serve process output:\n%s", output)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := client.New(filepath.Join(dataDir, "node.sock"))
	defer c.Close()
	for {
		if err := c.Do(ctx, http.MethodGet, "/v1/status", nil, nil); err == nil {
			break
		}
		select {
		case <-exited:
			t.Fatalf("serve exited before readiness: %v", exitErr)
		case <-ctx.Done():
			t.Fatal("serve never became ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	open := func(alias string) comms.OpenResponse {
		t.Helper()
		var result comms.OpenResponse
		err := c.Do(ctx, http.MethodPost, "/v1/sessions", comms.OpenRequest{
			Alias: alias, Scope: "local", Harness: "service", HarnessID: "shutdown-test:" + alias,
		}, &result)
		if err != nil {
			t.Fatalf("open %s: %v", alias, err)
		}
		return result
	}
	sender, recipient := open("sender"), open("recipient")

	// Hold the receiver's successful handoff report until after SIGTERM. Reading
	// the event establishes that a real worker has claimed this exact attempt.
	receiverCtx, stopReceiver := context.WithCancel(ctx)
	defer stopReceiver()
	messageEvents := make(chan comms.Message, 1)
	streamEnded := make(chan error, 1)
	go func() {
		streamEnded <- c.Stream(receiverCtx, "/v1/sessions/"+recipient.Session.ID+"/stream", func(raw json.RawMessage) error {
			var event comms.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if event.Type == "message" && event.Message != nil {
				select {
				case messageEvents <- *event.Message:
				case <-receiverCtx.Done():
					return receiverCtx.Err()
				}
			}
			return nil
		})
	}()
	var sent comms.Message
	if err := c.Do(ctx, http.MethodPost, "/v1/messages", comms.SendRequest{
		SessionID: sender.Session.ID, To: recipient.Agent.ID, Body: "finish this handoff during shutdown", ID: "sigterm_in_flight",
	}, &sent); err != nil {
		t.Fatal(err)
	}
	var delivery comms.Message
	select {
	case delivery = <-messageEvents:
	case err := <-streamEnded:
		t.Fatalf("receiver ended before handoff: %v", err)
	case <-ctx.Done():
		t.Fatal("no delivery attempt reached the receiver")
	}
	if delivery.ID != sent.ID || delivery.AttemptID == "" || delivery.State != "delivering" {
		t.Fatalf("expected active correlated handoff, got %+v", delivery)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	// OS signal handling is asynchronous. A nonexistent mutation is a harmless
	// probe: it returns 404 before closing, and the middleware's 503 after closing.
	for {
		err := c.Do(ctx, http.MethodPost, "/v1/shutdown-test-probe", nil, nil)
		var api *client.Error
		if errors.As(err, &api) && api.Status == http.StatusServiceUnavailable && api.Code == "draining" {
			break
		}
		if err != nil && (!errors.As(err, &api) || api.Status != http.StatusNotFound) {
			t.Fatalf("node stopped before receiver could complete its handoff: %v", err)
		}
		select {
		case <-exited:
			t.Fatalf("serve exited with an unacknowledged handoff: %v", exitErr)
		case <-ctx.Done():
			t.Fatal("serve did not enter its drain phase")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Completion is deliberately delayed into the drain window. An immediate
	// report can win the scheduling race even when a cancelled node context is
	// incorrectly aborting its worker and receiver; a real adapter may take time.
	select {
	case <-exited:
		t.Fatalf("serve exited instead of waiting for the active handoff: %v", exitErr)
	case err := <-streamEnded:
		t.Fatalf("receiver was cancelled before the drain deadline: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	const rejectedID = "sigterm_new_work_rejected"
	err = c.Do(ctx, http.MethodPost, "/v1/messages", comms.SendRequest{
		SessionID: sender.Session.ID, To: recipient.Agent.ID, Body: "must not be accepted while draining", ID: rejectedID,
	}, nil)
	var api *client.Error
	if !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable || api.Code != "draining" {
		t.Fatalf("new send was not rejected during drain: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/sessions/"+recipient.Session.ID+"/handoffs", comms.Handoff{
		SenderMachine: delivery.SenderMachine, MessageID: delivery.ID, AttemptID: delivery.AttemptID, Status: "handed_off",
	}, nil); err != nil {
		t.Fatalf("existing handoff could not finish during drain: %v", err)
	}
	select {
	case <-exited:
		if exitErr != nil {
			t.Fatalf("serve failed after draining: %v", exitErr)
		}
	case <-ctx.Done():
		t.Fatal("serve did not exit after its active handoff completed")
	}
	stopReceiver()

	// Inspect the stopped process's actual database read-only, without opening
	// Store (whose startup recovery could otherwise change the state under test).
	dbURL := url.URL{Scheme: "file", Path: filepath.Join(dataDir, "node.db"), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", dbURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state string
	var handedOffAt sql.NullInt64
	if err := db.QueryRow(`SELECT state,handed_off_at FROM messages WHERE sender_machine_id=? AND id=?`, sent.SenderMachine, sent.ID).Scan(&state, &handedOffAt); err != nil {
		t.Fatal(err)
	}
	if state != "handed_off" || !handedOffAt.Valid || handedOffAt.Int64 <= 0 {
		t.Fatalf("shutdown did not preserve successful durable handoff: state=%q handed_off_at=%v", state, handedOffAt)
	}
	var rejectedRows int
	if err := db.QueryRow(`SELECT count(*) FROM messages WHERE id=?`, rejectedID).Scan(&rejectedRows); err != nil {
		t.Fatal(err)
	}
	if rejectedRows != 0 {
		t.Fatal("a rejected send was persisted while draining")
	}
}
