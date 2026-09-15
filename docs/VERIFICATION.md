# Feature and production verification

Verified 2026-09-15. Full Go race suite, vet and ten Python brain tests pass.
Production message IDs and observations are in [ROLLOUT.md](ROLLOUT.md).
Unchecked final gates remain open.

- [x] Local-only startup, no broker required; same-machine global traffic stays local.
- [x] Ephemeral IDs, persistent create/resume, alias uniqueness, identity listing,
      explicit takeover, retirement and no accidental alias-based mail inheritance.
- [x] Receiver reconnect, lease expiry versus true harness death, Ctrl+C interruption
      versus exit, node restart preserving surviving sessions.
- [x] Durable send/receive/receipt; duplicate delivery, expiry, pruned replay records,
      undeliverable ephemeral mail and offline persistent mail.
- [x] Nonblocking retry/fair scheduling and per-agent handoff serialization;
      uncertain handoff recovery and controlled shutdown/crash recovery.
- [x] Encrypted registration proof, credential expiration/revocation, mismatched IDs,
      peer pinning, wrong keys, tampered metadata/body, replay and quota abuse.
- [x] Directional discovery/messaging grants, local enforcement, versioned revocation,
      pending-message failure and bounded receipts without reverse grant.
- [x] Local and remote history permissions, local-traffic exclusion, encrypted query
      relay, timeout/offline behavior, pagination, no consumption by inspection.
- [x] Pending queue quotas and backpressure, independent bounded receipt capacity.
- [x] CLI JSON/errors/stdin/files, sender inference, explicit recipient, prune/stat rules.
- [x] Claude Monitor and native Codex tool-output delivery with peer provenance;
      exact targeting, idle and busy behavior, no user-message injection.
- [x] Agent Monitor integration uses an independent headless node; existing dirty changes
      preserved; private-release bundling, migration and rollback documented.
- [x] Linux ARM64 and macOS release artifacts/checksums, tagged GitHub release.
- [x] New production Pi node/broker active; legacy service/history preserved.
- [x] Real root-session round trip with newly spawned Pi Claude agent.
- [x] Real root-session round trip with existing brain under persistent agent ID;
      brain restart resumes same ID/inbox and functionality.
- [x] Pi memory/idle CPU measured against initial <128MiB/<1%-one-core targets.
- [x] Agent Monitor PR exists and references released version and verification evidence.
- [ ] After every completion gate passes, ask the Pi Claude agent to send João a
      completion push through jsplayground; verify the actual push result before
      reporting the goal complete. Do not send this notification early.

## Evidence mapping

- `internal/comms/node_integration_test.go`: real nodes/broker, persistent offline
  delivery, ephemeral retirement, lease/death distinction, takeover, local routing,
  encrypted round trips, grants/history, prune/replay, quotas, receipts, fair
  scheduling, slow outgoing requests and restart/uncertainty.
- `internal/comms/crypto_store_test.go` and `broker_test.go`: key/challenge proof,
  credential renewal/expiry, authenticated metadata and payload validation,
  low-order keys, schema constraints, byte limits and strict receipt transitions.
- `internal/comms/store_isolation_test.go`: slow readers cannot occupy the queue
  writer; explicit uncertain resolution. Pi is running entirely without the GUI.
- `cmd/comms` and `internal/client` tests: CLI behavior, JSON/errors, stdin, sender
  inference, no implicit broadcast, receiver write-before-ack, native launcher
  lifetime, exact harness ownership, and real CLI SIGTERM drain.
- `internal/codex/adapter_test.go`, opt-in live tests and private local evidence:
  actual Codex idle/busy, exact target, tool-output provenance, TUI display,
  compiled launcher/resume and restart. See rollout for evidence directory.
- `integration/brain/test_comms_v1.py`: ten tests, including actual Unix node,
  bounded journal, lost responses, duplicate suppression, NO_REPLY, provenance,
  persistence, uncertain recovery and reversible installation.
- Agent Monitor client contract/process isolation and bundle integrity tests run
  in its public CI. Final uploaded release pins/full bundle remain a final gate.

Additional final gates:

- [x] Side-by-side skill resolver regression fixed in both installers; commands
  execute the installed binary with COMMS_BIN unset and preserve explicit overrides.
- [x] Actual installed v0.1.1 exchanges and retained legacy access rechecked.
- [x] Owned isolated Codex test processes stopped; evidence and production preserved.

## Final release evidence

- Released and installed **v0.1.1**, commit `e25e8ea1a7292ed4848a22b12745d6594c3a84ad`.
  Release run `34972108320` and CI `34972108279` passed. Uploaded archives match
  SHA256SUMS; final installs retained machine IDs and legacy CLI/skills.
- Final real request/reply pairs: `msg_final_brain_v011` →
  `brain_a2c07f83dc546a3bf4c54c5d35f17a50d59801ed` (**BRAIN_FINAL_V011**), and
  `msg_final_claude_v011` → `msg_1c6865f3fb03606fa7b037e589dbf85d`
  (**CLAUDE_FINAL_V011**). All four durable states are `handed_off`.
- Brain remains persistent `a_f45edf2c4849fa3c9d0aff6446c3ca99`.
  Node, brain and legacy services are active; legacy public `/who` returned200.
- Final Pi quiet30-second sample: **19.09 MiB RSS, 0.500% of one core**.
- Agent Monitor PR16 is ready, unmerged, head `33b4c15`; CI run `34972617606`
  passed. Exact uploaded v0.1.1 pins, full bundled app build, signature verification,
  actual final-binary viewer queries and installer/integrity regressions passed.
- The final notification is the only pending gate; readiness was confirmed by
  Pi Claude without sending a push early.
