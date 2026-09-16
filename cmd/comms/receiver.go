package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
)

// Walk ancestry rather than recording this short-lived CLI or its shell. A
// missing match simply leaves the process unverified; a lease is not a death
// detector and must not pretend that the model has exited.
func detectHarnessProcess(harness string) int {
	pid := os.Getppid()
	for i := 0; i < 12 && pid > 1; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "ppid=,comm=").Output()
		cancel()
		if err != nil {
			return 0
		}
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			return 0
		}
		name := filepath.Base(strings.Join(fields[1:], " "))
		if name == harness || strings.HasPrefix(name, harness+"-") {
			return pid
		}
		pid, err = strconv.Atoi(fields[0])
		if err != nil {
			return 0
		}
	}
	return 0
}

func terminalAPIError(err error) bool {
	var api *client.Error
	return errors.As(err, &api) && (api.Status == 404 || api.Status == 410 || api.Code == "session_ended" || api.Code == "session_not_found" || api.Code == "attachment_ended")
}

func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (a *app) stream(ctx context.Context, args []string) error {
	f := flags("stream")
	sessionFlag := f.String("session", "", "")
	once := f.Bool("once", false, "")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() > 1 {
		return usageError("usage: comms stream [ALIAS] [--session ATTACHMENT_ID] [--json] [--once]")
	}
	ref := *sessionFlag
	if f.NArg() == 1 {
		if ref != "" {
			return usageError("choose alias or --session")
		}
		ref = f.Arg(0)
	}
	s, err := a.session(ctx, ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	leaseErrors := make(chan error, 1)
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if s.ProcessID > 0 && s.ProcessStarted != "" {
					actual := comms.ProcessStamp(s.ProcessID)
					if actual != "" && actual != s.ProcessStarted {
						leaseErrors <- &client.Error{Code: "harness_ended", Message: "the original harness process is no longer running"}
						cancel()
						return
					}
				}
				err := a.c.Do(ctx, "PUT", "/v1/sessions/"+url.PathEscape(s.ID)+"/lease", nil, nil)
				if terminalAPIError(err) {
					leaseErrors <- err
					cancel()
					return
				}
			}
		}
	}()
	// Entries live only while the server acknowledgment is pending. Losing the
	// response must never print the same attempt twice in this receiver process.
	pending := map[string]comms.Handoff{}
	var outputErr error
	onceDone := errors.New("one message delivered")
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			select {
			case e := <-leaseErrors:
				return e
			default:
				return err
			}
		}
		err := a.c.Stream(ctx, "/v1/sessions/"+url.PathEscape(s.ID)+"/stream", func(raw json.RawMessage) error {
			var event comms.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if event.Type == "closed" || event.Type == "session_closed" {
				return &client.Error{Code: "session_ended", Message: "attachment was closed", Status: 410}
			}
			if event.Type != "message" || event.Message == nil {
				return nil
			} // Keepalives never wake the model.
			m := event.Message
			if m.AttachmentID != "" && m.AttachmentID != s.ID {
				return &client.Error{Code: "wrong_attachment", Message: "node streamed a delivery for a different attachment"}
			}
			if m.AttemptID == "" {
				return &client.Error{Code: "bad_stream", Message: "message is missing its delivery attempt ID"}
			}
			key := m.Key() + "/" + m.AttemptID
			h, printed := pending[key]
			if !printed {
				h = comms.Handoff{SenderMachine: m.SenderMachine, MessageID: m.ID, AttemptID: m.AttemptID, Status: "handed_off"}
				var writeErr error
				if a.compactOutput {
					writeErr = json.NewEncoder(a.out).Encode(comms.ContentForPeer(*m))
				} else if a.jsonOutput {
					writeErr = json.NewEncoder(a.out).Encode(event)
				} else {
					// Encoding the body prevents embedded terminal control sequences,
					// fake status lines, or newlines from impersonating framing.
					body, _ := json.Marshal(m.Body)
					_, writeErr = fmt.Fprintf(a.out, "COMMS PEER CONTENT from %s:%s [message=%s]: %s\n", m.SenderMachine, m.SenderAgent, m.ID, body)
				}
				if writeErr != nil {
					outputErr = writeErr
					h.Status = "uncertain"
					h.Failure = "receiver_output_failed"
					_ = a.c.Do(context.WithoutCancel(ctx), "POST", "/v1/sessions/"+url.PathEscape(s.ID)+"/handoffs", h, nil)
					return writeErr
				}
				pending[key] = h
			}
			// Retry only the durable acknowledgment, not the output operation.
			for attempt := 0; ; attempt++ {
				err := a.c.Do(ctx, "POST", "/v1/sessions/"+url.PathEscape(s.ID)+"/handoffs", h, nil)
				if err == nil {
					delete(pending, key)
					backoff = time.Second
					if *once {
						return onceDone
					}
					return nil
				}
				var api *client.Error
				if terminalAPIError(err) || errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
					return err
				}
				if err := pause(ctx, time.Duration(min(attempt+1, 5))*time.Second); err != nil {
					return err
				}
			}
		})
		if errors.Is(err, onceDone) {
			return nil
		}
		if outputErr != nil {
			return outputErr
		}
		if terminalAPIError(err) {
			return err
		}
		if ctx.Err() != nil {
			continue
		}
		var api *client.Error
		if errors.As(err, &api) && api.Code != "node_unavailable" && api.Status >= 400 && api.Status < 500 {
			return err
		}
		// An output failure must stop the receiver; reconnecting cannot repair
		// a broken pipe and could consume later messages without an observer.
		if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) {
			return err
		}
		if err := pause(ctx, backoff); err != nil {
			continue
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (a *app) events(ctx context.Context, args []string) error {
	if err := a.noArgs(args); err != nil {
		return err
	}
	for {
		err := a.c.Stream(ctx, "/v1/events", func(raw json.RawMessage) error {
			var event comms.Event
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if event.Type == "heartbeat" || event.Type == "keepalive" {
				return nil
			}
			return json.NewEncoder(a.out).Encode(event)
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminalAPIError(err) {
			return err
		}
		if err := pause(ctx, time.Second); err != nil {
			return err
		}
	}
}
