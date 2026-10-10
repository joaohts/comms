package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joaohts/comms/internal/client"
	"github.com/joaohts/comms/internal/comms"
)

var version = comms.Version

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := app{in: os.Stdin, out: os.Stdout, errOut: os.Stderr, getenv: os.Getenv}
	if err := a.run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		var exit exitError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		code := "command_failed"
		var api *client.Error
		if errors.As(err, &api) {
			code = api.Code
		}
		if a.jsonOutput {
			_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"code": code, "error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "comms:", err)
		}
		os.Exit(1)
	}
}
