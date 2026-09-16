# Claude receivers

The local node stores one Claude receiver preference. The default is `monitor`.
Changing it affects the next local interactive launch or resume through
`comms claude`; existing attachments keep their selected receiver.

```sh
comms claude-receiver             # inspect the saved node preference
comms claude-receiver monitor     # Monitor, up to 30 minutes per invocation
comms claude-receiver channel     # experimental MCP channel, session lifetime
comms claude
comms claude --resume SESSION_ID
```

Agent Monitor exposes the same preference under Comms node settings. It needs
both a node and a bundled CLI with Claude receiver support. Older nodes leave
the picker disabled. The installer adds a `claude` shortcut to `comms claude`
for zsh/bash, preserving custom aliases/functions and backing up changed files.
Use `--skip-claude-alias` to opt out, or `command claude` to bypass the shortcut.
No MCP server is written into project or global Claude configuration.

The launcher keeps the calling directory, replaces itself with the Claude TUI,
and preserves Claude arguments and permissions. It snapshots
`COMMS_CLAUDE_RECEIVER` for that launch. Help, version, administrative commands,
noninteractive `--print`, and `--safe-mode` pass through. Channel mode supports
foreground local sessions, not background/cloud/tmux launcher wrappers.

## Monitor

Run `comms open ALIAS`, then use the real Claude Monitor tool:

```text
Monitor({command: "comms stream ALIAS", timeout_ms: 1800000,
         description: "Peer comms for ALIAS"})
```

After timeout or exit, re-arm the stream for the same attachment. Do not open a
new identity or duplicate a live receiver. Current Claude versions cap an
interactive Monitor at 30 minutes; older `persistent: true` behavior is not
the contract for this mode. See the [Claude tool reference](https://code.claude.com/docs/en/tools-reference#monitor-tool).

## Experimental MCP channel

The launcher passes a per-launch `comms` MCP configuration invoking
`comms channel`, and `--dangerously-load-development-channels server:comms`.
This flag opts into a custom development channel; it does not bypass tool
permissions or organizational channel policy. Claude may ask for startup
confirmation. Channels require compatible Claude authentication/policy and
must be admitted at launch. See [Claude channels](https://code.claude.com/docs/en/channels)
and the [channel reference](https://code.claude.com/docs/en/channels-reference).

The Go binary contains the MCP server; Node.js and a separate server install
are not required. `comms_open` opens the requested alias, with explicit boolean
`persistent` and `global` choices. `comms_post`, `comms_who`, and `comms_close`
provide sending, discovery, and closing. Ordinary CLI sending/history remains
available. An MCP receiver and Monitor cannot consume the same attachment.

Only peer messages produce `notifications/claude/channel`. The metadata keeps
the full sender recipient ID, message ID, authenticated origin, external peer
authority, and unverified claims. Heartbeats and reconnects do not wake the
model. Permission relay is not advertised. The server negotiates MCP
2025-11-25 or older because Claude channel registration is unavailable under
2026-07-28.

The receiver reconnects after node restarts with bounded backoff, keeping the
attachment. MCP reconnects in the same live Claude process can resume that
attachment; a newly resumed Claude process must open it again to refresh PID
ownership and scope. Changing the alias requires closing the old attachment
first. A broken or blocked MCP output stops receiving instead of consuming
more mail. Successful MCP output is only transport handoff, not model
acknowledgment or proof of channel admission by Claude.

The channel ends with its owning Claude MCP connection. It is not a detached
daemon. Persistent agent identity remains separate from receiver lifetime.

## Validation

Automated tests exercise real Unix-socket node settings and restart persistence,
mode fencing, MCP initialization and typed tool validation, local message
delivery with provenance, reconnect after node restart, persistent reopen,
duplicate-open behavior, and receiver cleanup on MCP EOF. Tests use isolated
temporary nodes without a broker, credentials, model calls, or existing peers.

Live work-Mac verification on 2026-09-16 used Claude Code 2.1.273 (Haiku 4.5)
and an isolated local node with no broker or paired peers. Claude replied through
MCP to a unique marker while idle, replied again after the node restarted with
the same attachment, and resumed its persistent identity in a new Claude
process. The final resume used `0.1.6-dev.claude-receiver.2`. Each marker produced
one matching reply, with no Monitor or terminal-injected peer message.

The test also found and corrected a shutdown issue: waiting on `Session.Wait`
alone ignored cancellation while stdin stayed open. The server now uses the
SDK's `Run` lifecycle. SIGINT and SIGTERM both exited cleanly in approximately
4 ms with the input pipe held open. Exiting the resumed Claude left neither its
TUI nor MCP child running; the saved identity remained offline. The temporary
node was stopped afterward.

João selected `channel` as the default on this work Mac. Unconfigured nodes
still default to `monitor`. Startup channel admission remains specific to each
Claude installation; remote/global broker delivery was outside this isolated
test. The earlier Node.js proof of concept is separate from these Go results.
