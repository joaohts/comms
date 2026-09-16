# comms

A local-first, encrypted communication service for agent sessions. A single Go
executable provides a machine-local node, CLI, and optional broker role.

The node is deployed on a Mac and Pi with direct Claude and brain
round trips. See [rollout evidence](docs/ROLLOUT.md) for the current verification
state. The legacy service and its history remain separate during migration.

## Install

Download a platform archive and `SHA256SUMS` from a tagged release, verify the
checksum, extract, and run `scripts/install.sh`. The installer sets up a per-user
background node independently of Agent Monitor. Linux ARM64 and macOS ARM64/x64
artifacts include the executable and integration skill; no Go compiler is needed.

The repository is private. Developers use their own authenticated `gh` account to
download artifacts; packaged Agent Monitor applications include the verified
binary and do not require a runtime GitHub token.

Releases use `vMAJOR.MINOR.PATCH` tags. Patch releases preserve API and database
compatibility; an incompatible change during the 0.x series requires a new minor
version and documented migration. The wire API is versioned separately (`/v1`).
Published assets are immutable. Agent Monitor pins a tag and archive digests;
updating the viewer's pin is an explicit reviewed change, with no automatic
download of a moving `latest` binary.

```sh
gh release download v0.1.2 --repo joaohts/comms \
  --pattern comms_Linux_arm64.tar.gz --pattern SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf comms_Linux_arm64.tar.gz
bash scripts/install.sh
```

For a legacy installation, use `--binary-name comms-v1 --skip-skills` first.
See [installation and rollback](deploy/README.md) before changing the existing
command or receiver. Existing plaintext history is not imported.

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

Claude runs its receiver under the actual harness Monitor tool, as described in
[open-comms](integration/open-comms/SKILL.md). Codex starts or resumes through
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

The brain integration has a separate bounded application journal and preserves
its existing model, memory, other channels, and `NO_REPLY` breaker. See [brain
installation](docs/BRAIN.md).

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

See [implementation plan](docs/IMPLEMENTATION.md), [API](docs/API.md), and
[verification matrix](docs/VERIFICATION.md).
