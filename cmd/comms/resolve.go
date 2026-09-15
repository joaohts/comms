package main

import (
	"context"
	"net/url"
)

func (a *app) resolve(ctx context.Context, args []string) error {
	f := flags("resolve")
	status := f.String("status", "", "")
	sender := f.String("sender", "", "")
	if e := parse(f, args); e != nil {
		return e
	}
	if f.NArg() != 1 || (*status != "handed_off" && *status != "retry" && *status != "undeliverable") {
		return usageError("usage: comms resolve MESSAGE_ID --status handed_off|retry|undeliverable [--sender MACHINE_ID]; retry can duplicate a previously accepted handoff")
	}
	return a.mutation(ctx, "POST", "/v1/messages/"+url.PathEscape(f.Arg(0))+"/resolve", map[string]string{"status": *status, "sender_machine_id": *sender})
}
