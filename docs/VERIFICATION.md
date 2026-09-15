# Feature and production verification

Evidence is added as tests run. Unchecked items are not complete.

- [ ] Local-only startup, no broker required; same-machine global traffic stays local.
- [ ] Ephemeral IDs, persistent create/resume, alias uniqueness, identity listing,
      explicit takeover, retirement and no accidental alias-based mail inheritance.
- [ ] Receiver reconnect, lease expiry versus true harness death, Ctrl+C interruption
      versus exit, node restart preserving surviving sessions.
- [ ] Durable send/receive/receipt; duplicate delivery, expiry, pruned replay records,
      undeliverable ephemeral mail and offline persistent mail.
- [ ] Nonblocking retry/fair scheduling and per-agent handoff serialization;
      uncertain handoff recovery and controlled shutdown/crash recovery.
- [ ] Encrypted registration proof, credential expiration/revocation, mismatched IDs,
      peer pinning, wrong keys, tampered metadata/body, replay and quota abuse.
- [ ] Directional discovery/messaging grants, local enforcement, versioned revocation,
      pending-message failure and bounded receipts without reverse grant.
- [ ] Local and remote history permissions, local-traffic exclusion, encrypted query
      relay, timeout/offline behavior, pagination, no consumption by inspection.
- [ ] Pending queue quotas and backpressure, independent bounded receipt capacity.
- [ ] CLI JSON/errors/stdin/files, sender inference, explicit recipient, prune/stat rules.
- [ ] Claude Monitor and native Codex tool-output delivery with peer provenance;
      exact targeting, idle and busy behavior, no user-message injection.
- [ ] Agent Monitor integration works with GUI absent/frozen and existing dirty changes
      preserved; private-release bundling, migration and rollback documented.
- [ ] Linux ARM64 and macOS release artifacts/checksums, tagged GitHub release.
- [ ] New production Pi node/broker active; legacy service/history preserved.
- [ ] Real root-session round trip with newly spawned Pi Claude agent.
- [ ] Real root-session round trip with existing brain under persistent agent ID;
      brain restart resumes same ID/inbox and functionality.
- [ ] Pi memory/idle CPU measured against initial <128MiB/<1%-one-core targets.
- [ ] Agent Monitor PR exists and references released version and verification evidence.
- [ ] After every completion gate passes, ask the Pi Claude agent to send João a
      completion push through jsplayground; verify the actual push result before
      reporting the goal complete. Do not send this notification early.
