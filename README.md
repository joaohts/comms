# comms

A local-first, encrypted communication service for agent sessions. A single Go
executable provides a machine-local node, CLI, and optional broker role. Claude
Code and Codex sessions, and long-running agents such as the brain, use it to find
and message each other on one machine or across machines.

## Part of a three-repo stack

| Repo | What it is | Runs on |
|---|---|---|
| [brain](https://github.com/joaohts/brain) | A long-running personal agent: LLM loop, tools, memory, and channels (WhatsApp, comms, CLI) | Linux / Raspberry Pi |
| **comms** (this repo) | Encrypted messaging between agent sessions and machines: node, CLI, optional broker | macOS, Linux |
| [agent-monitor](https://github.com/joaohts/agent-monitor) | macOS app: live view of every Claude Code / Codex / Cursor session, plus a comms dashboard | macOS |

Each works alone. Together: install **comms** on every machine, **agent-monitor**
on your Mac (it bundles a pinned comms release), and **brain** on an always-on box
with its comms channel enabled. Pair the machines once and every session — and the
brain — can reach every other.

How the parts connect:

- **agent-monitor ↔ comms** — the app bundles a pinned comms release, installs it as a per-user service, and shows its agents, history, pairing and grants. See [agent-monitor: comms connection](https://github.com/joaohts/agent-monitor/blob/main/docs/comms-connection.md).
- **brain ↔ comms** — the brain joins comms as a persistent agent (alias `brain`), so any session can message it and it can message back. See [comms: brain integration](https://github.com/joaohts/comms/blob/main/docs/BRAIN.md).
- **Claude Code / Codex ↔ comms** — sessions launched through `comms claude` / `comms codex` receive peer messages. See [comms: Claude](https://github.com/joaohts/comms/blob/main/docs/CLAUDE.md) and [Codex](https://github.com/joaohts/comms/blob/main/docs/CODEX.md).

## Install

Download a platform archive and `SHA256SUMS` from a tagged release, verify the
checksum, extract, and run `scripts/install.sh`. The installer sets up a per-user
background node independently of Agent Monitor. Linux ARM64 and macOS ARM64/x64
artifacts include the executable and integration skill; no Go compiler is needed.
The CLI is installed to `~/.local/bin/comms`; make sure `~/.local/bin` is on your
`PATH`.

Packaged Agent Monitor applications include the verified binary and need no
runtime GitHub token.

Releases use `vMAJOR.MINOR.PATCH` tags. Patch releases preserve API and database
compatibility; an incompatible change during the 0.x series requires a new minor
version and documented migration. The wire API is versioned separately (`/v1`).
Published assets are immutable. Agent Monitor pins a tag and archive digests;
updating the viewer's pin is an explicit reviewed change, with no automatic
download of a moving `latest` binary.

```sh
version=v0.1.7
archive=comms_Linux_arm64.tar.gz   # or comms_Linux_amd64 / comms_Darwin_arm64 / comms_Darwin_amd64
base=https://github.com/joaohts/comms/releases/download/$version
curl -fLO "$base/$archive" -fLO "$base/SHA256SUMS"
grep " $archive\$" SHA256SUMS | shasum -a 256 -c -
tar -xzf "$archive"
bash scripts/install.sh
```

See [installation and rollback](deploy/README.md) for defaults, updates, and
running your own broker.

### Shell aliases

The installer adds two managed aliases to your zsh (`~/.zshrc`) or bash
(`~/.bashrc`) configuration so new terminal sessions launch through comms and can
receive peer messages:

- `claude` runs `comms claude`
- `codex` runs `comms codex`

Existing custom `claude`/`codex` aliases or functions are left alone. Bypass the
alias for one command with `command claude ...` or `command codex ...`. Opt out at
install time with `--skip-claude-alias` and/or `--skip-codex-alias`. To remove an
alias later, delete its `# BEGIN COMMS CLAUDE ALIAS` / `# BEGIN COMMS CODEX ALIAS`
block (through the matching `# END` line) from the shell config, then run
`unalias claude` or `unalias codex` in open shells.

### Build from source

Requires Go (version from `go.mod`, currently 1.27.1). The installer also needs
Python 3.

```sh
go build ./cmd/comms
go test ./...
bash scripts/install.sh --binary ./comms
```

`--binary` skips the release checksum check, which applies only to release bundles.

## Local agents

```sh
comms open worker                         # ephemeral, local-only
comms open brain --persistent            # create/resume identity by name
comms identities
comms who
comms post --to worker "Hello"
comms inbox                              # inspection never consumes mail
comms log
comms status MESSAGE_ID
comms close
```

The sender is inferred from its harness attachment. Services can use an explicit
`--from ALIAS`; command bodies also support `--stdin` and `--file PATH`.
Use `--json` for machine-readable output.

For smaller agent-facing JSON, use `--compact` on routine commands:

```sh
comms who --compact
comms post --to pi:brain --compact "Hello"
comms log --compact
```

This keeps exact reply identities, message state and history pagination while
omitting delivery bookkeeping. `--json` still returns full records for diagnostics
and existing integrations. Native Codex messages omit the redundant recipient ID;
both receivers retain private attempt/attachment IDs for reliable acknowledgments.

Claude's node setting selects a 30-minute Monitor (default) or an experimental
MCP channel. `comms claude-receiver monitor|channel` saves the preference;
`comms claude` applies it at the next launch/resume. See [Claude setup](docs/CLAUDE.md)
and [open-comms](integration/open-comms/SKILL.md). Codex starts or resumes through
`comms codex [resume THREAD_UUID]`; the node delivers native tool output to the
exact thread. It never types peer messages as user input. See [Codex](docs/CODEX.md).

## Cross-machine agents

Configure one HTTPS broker, exchange public identity bundles over an authenticated
channel outside it, then grant access explicitly:

```sh
comms broker https://YOUR_BROKER
comms export --alias mac --json > mac-public.json
comms pair --file pi-public.json
comms grant pi                            # Pi may discover/send to this node
comms grant pi --read-history             # additionally allow history reads
comms open worker --global
comms post --to pi:brain "Hello"
comms log pi:brain
```

The reverse direction requires a separate grant. Pairing alone grants nothing.
Once granted, a peer's messages to your agents are delivered into your live Claude
Code and Codex sessions (those launched through `comms claude` / `comms codex`).
An operator may also require a shared service API key, including for registration.
Configure its private file on both broker and consumer nodes with the installer's
`--broker-service-key-file PATH`; updates preserve it. This additional gate does
not replace machine-key verification or grants. See [deployment](deploy/README.md).
Receipts are correlated protocol statuses and do not need a reverse messaging
grant. Remote history requires its own permission and excludes same-machine
traffic. Human/operator history queries can use `--operator` without claiming an
agent identity.

## Delivery and history

- `received`: the destination node durably accepted the message.
- `handed_off`: the receiving adapter accepted it; this is not proof of model
  understanding or successful task execution.
- `undeliverable`: permanent failure, including an ended ephemeral recipient.
- `uncertain`: the adapter may have accepted it; automatic replay is stopped.

```sh
comms resolve MESSAGE_ID --status handed_off
comms resolve MESSAGE_ID --status retry          # explicit duplicate risk
comms resolve MESSAGE_ID --status undeliverable
comms prune --before 2026-08-01
```

Undelivered messages expire after seven days. Completed local history is retained
until pruned. Pruning removes content and derived statistics while retaining
minimal replay records until authenticated expiry. No separate statistics store
or permanent broker traffic history is used.

The brain's comms channel keeps its own bounded journal so accepted messages
survive restarts. See [brain integration](docs/BRAIN.md).

## Contract

- Same-machine messages never leave the machine.
- Fresh ephemeral agent identities by default; `--persistent` explicitly saves
  and resumes identities by alias. `--global` enables authorized remote traffic.
- Machine IDs are random strings, separate from out-of-band verified public keys.
- Directional grants control discovery/messages and optional history reads.
- The broker stores ciphertext queues, never message plaintext or peer private keys.
- SQLite `node.db` and optional `broker.db` have independent lifecycles.
- Confirmed-ended ephemeral agents produce undeliverable receipts. Idle or
  disconnected agents are not inferred dead solely from a lease timeout.
- Process-level handoff does not mean a model understood or acted on a message.
- History inspection does not consume messages. Completed local history remains
  until explicitly pruned; minimal replay records survive until message expiry.

See [API](docs/API.md) and [broker contract](docs/BROKER-CONTRACT.md).
