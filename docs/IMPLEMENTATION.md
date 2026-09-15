# Implementation and rollout

## Authorized outcome

Implement and release `joaohts/comms`, run the released node and broker on the Pi,
configure the existing brain as a persistent agent, directly exchange messages
between the new Mac node and both a newly spawned Pi Claude session and the brain,
and open an Agent Monitor PR with version-pinned installation and migration docs.
Legacy code and history stay separate; no destructive legacy cutover is needed.

## Implementation order

1. Go module, explicit wire types, crypto, SQLite constraints and migrations.
2. Local node identities, attachments, lease/liveness, durable messages and CLI.
3. Broker authentication, grants, presence, queues, encrypted transport and receipts.
4. Read-only history and encrypted online queries, pruning and derived statistics.
5. Delivery scheduling, crash recovery, Claude stream and service integrations.
6. Codex external-tool-output adapter; never inject peer content as a user message.
7. Installer, system services, release artifacts, Agent Monitor integration.
8. Production setup and direct feature verification, release/PR evidence.

## Fixed decisions

- Go; `golang.org/x/crypto/nacl/box`; pure-Go SQLite driver for portable binaries.
- No key rotation or key recovery in v1. A lost key means a new machine identity.
- One configured broker, optional role in the same process as its local node.
- Local HTTP/JSON on a user-owned Unix socket; broker HTTPS/JSON; NDJSON streams.
- Four delivery workers; one handoff at a time per agent; availability events plus
  retries at 1/2/4/8/16 seconds then 30-second cap with jitter. No strict FIFO promise.
- Heartbeat 15 seconds, lease 60 seconds, controlled shutdown drain 10 seconds.
- Per-message content limit 64 KiB; pending local per-agent 1,000 or 32 MiB;
  pending broker per-machine 10,000 or 256 MiB. Bounded independent receipt reserve.
- History default 50, maximum 200 entries and 1 MiB; remote query deadline 10 seconds.
- Undelivered expiry seven days. Completed local history retained until pruned.
- Revocation applies before final handoff and each history query/page. No recall
  of already emitted content. Grant revisions and disabled tombstones persist.
- Receipts bypass reverse messaging grants only as authenticated, correlated,
  bounded status data. History uses a separate online query interface, not messages.
- Local history reads under the same OS user; remote reads require history grant
  and exclude same-machine traffic. No full harness-transcript access.
- GUI is optional and outside delivery's process lifecycle. Slow UI/history work
  cannot occupy all delivery capacity. No private keys or broker credentials in GUI output.

## Engineering choices to implement and test

- Purpose-bound, single-use encrypted challenge for possession of X25519 keys;
  60-second challenge TTL and 24-hour broker credentials, server stores only hashes.
- Strict authenticated metadata comparison, nonce randomness, immutable message IDs,
  expiry enforcement, duplicate collision checks and monotonic receipt transitions.
- Authenticated private history tunnel using bounded in-memory request correlation.
- Short SQLite WAL/FULL transactions; acknowledgment only after durable receipt and
  durable receipt obligation. No network/handoff inside a database transaction.
- A message-level `receipt_pending` flag durably preserves receipt obligations
  when the bounded receipt outbox is full. Flush reads current state inside its
  transaction, so a newer handoff cannot be replaced by a stale received receipt.
- Handoff failures before emission retry; potentially emitted interrupted attempts
  become uncertain. Explicit recovery never silently duplicates terminal input.
- Process identity includes enough information to reject PID reuse; transient
  disconnect and system suspend do not alone permanently retire agent identities.

## Work ownership

- Root: comms repository, node/broker/CLI, Pi rollout, releases and end-to-end audit.
- Existing `personal:worker`: isolated Agent Monitor worktree, integration PR,
  migration documentation and Codex adapter investigation/integration coordination.

## Completion gate

Every row in VERIFICATION.md needs concrete test evidence. Local test success alone
does not establish production completion. Record release digest, running binary,
system-service status, real message IDs/replies, persistent brain identity and PR URL.

Final notification (explicit user instruction): after all required work and live
tests are complete, message the Pi Claude agent and have it send João a push via
jsplayground. Verify delivery submission; this is the user's completion signal.
