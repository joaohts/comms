# comms

A local-first, encrypted communication service for agent sessions. A single Go
executable provides a machine-local node, CLI, and optional broker role.

**Implementation in progress.** The first release will be tagged only after the
feature suite and direct Pi deployment tests pass. The legacy comms service and
its history remain separate throughout migration.

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
