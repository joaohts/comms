# Broker service-key protection — v0.1.5, 2026-09-16 UTC

At João's urgent request, optional shared service-key enforcement was added and
**enabled on the Pi broker**. Mac and Pi run verified release v0.1.5 with the
private file explicitly configured in their supervisors. The key value is not
recorded here, in the vault, or in public identity exports.

- Release commit `a21dd0c`; release workflow `35050635527` and CI `35050635480` passed.
- `X-Comms-Service-Key` gates every broker route, including auth/registration,
  streams and optional legacy forwarding. Machine proof/bearer auth, grants and
  message encryption remain separate requirements.
- Both managed machines use `~/.local/share/comms/broker-service-key` (0600),
  passed by `--broker-service-key-file`. Missing/invalid configured files fail
  startup; normal upgrades preserve the option. HTTP redirects are not followed.
- Public missing/wrong-key requests returned 401 `service_key_required`. With the
  correct service key but no machine credential, discovery returned 401
  `unauthorized`. With the key, retired legacy/local-only routes remain 404.
- Mac/Pi status reports configured=true and connected=true. A real brain reply
  **SERVICE_KEY_ROUNDTRIP_OK** (`brain_befda3aab34b16bd757b30415ac938dd24911796`)
  followed request `msg_service_api_key_probe`, durably handed off.
- Final published artifacts were installed after the urgent deployment; this also
  exercised preservation of the key setting without re-supplying the flag.
- At João's explicit request, a private JSON containing the broker URL, public
  identities and service API key was copied to his clipboard without printing
  the secret. The guide explains extracting only the secret to a private file.
- Agent Monitor PR16 adds file selection (no key-content display/read in GUI),
  safe configured status, and installer forwarding; bundled v0.1.5 pins are exact.
  Final head `e495c841bf2c804ed88f8ef544f2d0a44bd822ca`, CI `35052072094` passed.
  Headless bundle/signature/client tests passed; PR remains unmerged.

Earlier deployment records follow as history.

# Current deployment — v0.1.3, 2026-09-16 UTC

**The new `comms` CLI is the default on Mac and Pi.** `comms-v1` is a symlink to
that executable. Both supervised nodes run v0.1.3 with unchanged machine keys;
brain retains persistent ID `a_f45edf2c4849fa3c9d0aff6446c3ca99`.

- Release: https://github.com/joaohts/comms/releases/tag/v0.1.3
- Release commit `4c275bbc58f9d0609eadda7bcaf8236f78fea81a`; release workflow
  `35046287824` passed, including Go race/vet, 16 brain and 6 voice tests.
- Public Cloudflare broker: `https://comms.jsplayground.cc/v1/`. Unauthenticated
  `/v1/who` returns 401; local-only `/v1/sessions` returns 404 publicly.
- Legacy `comms-api.service` is **inactive and disabled**. The new node's proxy
  flag is removed. Old public `/who`, `/cli`, and `/skill` return 404.
- Final WAL-safe legacy backup (integrity_check=ok):
  `/home/joaohts/.local/state/comms/legacy-retirement/20260916T021005Z/legacy-history-final.db`.
  The old source/database remains separate; no history was imported or deleted.
- Brain no longer starts its legacy polling thread. Both comms channel spellings
  use the new adapter; spawned-task discovery uses compact JSON and immutable
  recipients with stable submission IDs. Future adapter updates retain retirement.
- Voice's transitional board poller/registration/fallback is retired. Its existing
  localhost HTTP trigger, audio/model code and direct brain HTTP delegation remain.
- Brain and voice were restarted while idle. openclaw-gateway was not restarted.
  Source backups: brain `20260916T020619Z-bc2c285b`, voice
  `20260916T020619Z-09220230` under each private backup directory.
- Pi launcher commit `9510103` emits `/open-comms session-$id --global` inside the
  actual Claude session after REPL readiness; changelog commit `ba2c708` records it.
- After disabling the old broker, root received **BRAIN_AFTER_LEGACY_OFF**
  (`brain_7526af84b3f43cbd1c220fca907ca11a08680fc7`) and
  **CLAUDE_AFTER_LEGACY_OFF** (`msg_287705299fcd75bc77de4ddf3441481a`).
  Requests `msg_brain_after_legacy_off` and `msg_claude_after_legacy_off` are
  durably handed off. These exchanges used the new public broker exclusively.
- A freshly spawned default-model Claude displayed the correct new join command,
  but Fable quota prevented execution. This new-session model check remains
  quota-blocked; its temporary session was cleaned up. The emitted command and
  existing-agent/brain delivery were verified independently.
- Agent Monitor PR16 remains ready and **unmerged**, pinned to v0.1.3 at
  `1c231d12d7c9e33dc7b3bc430d5e13b9cb3f1a29`. Full bundle/signature/client checks
  and CI `35046754240` passed. The original dirty GUI checkout was preserved.
- Work-Mac install/update/pairing guide and public-only Pi/personal exports are
  synced in the vault: `projects/comms-operations.md` and `projects/comms-pairing/`.
  No work machine identity or grants were fabricated; onboarding runs on that Mac.

The root Codex conversation still needs its documented one-time launcher resume
for continuous native receiving. Manual tool reads used for these tests are not
represented as automatic idle delivery.

The remainder preserves the initial rollout and earlier checkpoints.

# Initial production rollout — 2026-09-15 (historical)

**Software delivery verified: v0.1.1 runs on both Mac and Pi.** Final real brain
and Pi Claude round trips pass, and Agent Monitor PR16 is ready for review.
The Pi Claude agent sent the completion push at **13:08:33 UTC**. Its response
and an independent database check confirm one push,
`7b9463c9-9563-4cf6-918e-677f8561781b`, delivered=1 and provider_status=ok.
All required delivery and notification gates are complete.

## Final release

- Release: https://github.com/joaohts/comms/releases/tag/v0.1.1
- Commit: `e25e8ea1a7292ed4848a22b12745d6594c3a84ad`.
- Release 34972108320 and CI 34972108279 passed; actual uploaded hashes:

| Archive | SHA256 |
|---|---|
| Darwin arm64 | `88f92510db1f251346986d5eaa839d26a107a18ca35a683108892aa77116aeb5` |
| Darwin amd64 | `d632d9b77b13f9fd2a2985852248557e378e65cc524670d575997474e60b6c34` |
| Linux arm64 | `1629e026277b65e8662a510925b02056bc7542e968bff2f63bec4a5a37f12231` |

Both machines installed these verified archives successfully, preserving keys,
identities, grants, legacy CLI/skills and queues. Brain's release adapter was
already identical; no extra brain restart was needed for the packaging patch.

Final request/reply IDs and CI evidence are in [VERIFICATION.md](VERIFICATION.md).
Brain returned **BRAIN_FINAL_V011** and the fresh Pi Claude returned
**CLAUDE_FINAL_V011**, with both directions durably handed off. Brain is still
persistent under its original new-node ID. Legacy public `/who` returned 200.
The final Pi sample measured **19.09 MiB RSS / 0.500% of one core over 30 seconds**.

[Agent Monitor PR16](https://github.com/joaohts/agent-monitor/pull/16) is ready,
unmerged, and pins this release. Full bundled build, codesign verification,
actual viewer-client queries and final macOS CI 34972617606 passed. The user's
original dirty AgentMonitor.swift remains untouched.

## Earlier rollout checkpoints — historical

The following records retain the candidate rollout and the upgrade issue that
led to v0.1.1. Their outstanding-work lists have been superseded by the final
release evidence above.

## Repositories and release

- New private repository: https://github.com/joaohts/comms
- Main commits: `164fa31` initial implementation, `b08dc45` completed integrations.
- Published candidate: **v0.1.0-rc.1**, tag target
  `b08dc4599aaf509a5969d9deac2c59f2dc1d7432`.
- GitHub CI and Release workflows for this candidate passed.
- Authenticated release downloads are cached at
  `~/.local/state/comms/releases/v0.1.0-rc.1` on the Mac and Pi.
- All downloaded platform archives passed SHA256SUMS verification.
- Agent Monitor draft PR: https://github.com/joaohts/agent-monitor/pull/16
  in `/Users/joaohts/fun/agent-monitor-comms-v1`, branch `feat/comms-node-v1`.
  Existing dirty `~/fun/agent-monitor/AgentMonitor.swift` was preserved.
- PR still needs exact **final v0.1.1 uploaded asset hashes**, final bundle build,
  latest CI review and ready-for-review status. Do not merge without a request.

## Live candidate services

- Mac: `~/.local/bin/comms-v1`, launchd `com.joaohts.comms`.
- Pi: `~/.local/bin/comms-v1`, systemd **user** `comms-node.service`.
- Both use `~/.local/share/comms/node.sock` and `node.db`.
- Pi runs the broker in the same process, with separate `broker.db`.
- New public broker API: `https://comms.jsplayground.cc/v1/...`.
- Pi listener: `127.0.0.1:3300`; its own client uses `http://127.0.0.1:3300`.
- Legacy `comms-api.service` is still active as a **system** service, now on
  port3301. New broker forwards non-/v1 paths to `http://127.0.0.1:3301`.
- Legacy proxy `/who` with legacy credentials returned HTTP200 after migration.
- Bare `comms` remains legacy. Do not replace the brain's PATH with the new CLI.

## Verified public identities and grants

Public identity bundles were exchanged through existing authenticated SSH,
never obtained from the broker. Both nodes have messaging + history grants.

| Node | Machine ID | Local label |
|---|---|---|
| Mac | `m_a6e65fba19860403bd50d43e174adce7` | personal |
| Pi | `m_4b94209a48e6ba8725cbe892afe63e2e` | pi |

No private keys or bearer credentials belong in this document.

## Brain integration: deployed and directly working

- Brain user service: `joana-brain.service`; do not restart openclaw-gateway.
- Installed release module and tiny hooks with `integration/brain/patch_brain.py`.
- Source backup:
  `/home/joaohts/.local/state/comms/brain-backups/20260915T121116Z-9f8563ef`.
- Drop-in: `~/.config/systemd/user/joana-brain.service.d/comms-v1.conf`.
- Enabled v1 Unix-socket ingress and explicitly trusted the verified Mac/Pi IDs.
- Original legacy ingress, model, memory, voice and WhatsApp paths remain.
- Brain is **persistent** ID `a_f45edf2c4849fa3c9d0aff6446c3ca99`.
- Its first v1 attachment: `s_86ec76dabf4a1a5f2bbec0010cc6e9e0`.
- Direct preflight to actual brain returned its exact marker in2.5s before changes.
- Root sent new encrypted message `msg_production_brain_probe_1` through public
  broker; brain generated **BRAIN_NEW_COMMS_1211** and replied through new node.
- Brain reply ID: `brain_1e97f5652dbad53f4ddb438e56f0bc709d991360`.
- Root consumed that reply through `comms-v1 stream ... --json --once` as actual
  tool output. Both directions reached durable `handed_off`.
- Real encrypted remote history query `comms-v1 log pi:brain --operator --json`
  returned the two matching messages. No synthetic echo service was substituted.
- Restarted the actual brain service: the same persistent ID resumed with a new
  attachment. `msg_brain_after_restart` produced `BRAIN_PERSISTENT_RESTART_OK`,
  reply `brain_2485b5bcd633cad8fbaa2a6a94768dc6de7b008c`; both handed off.

## Root's new-board communication identity

- Alias `notes-codex`, agent `a_dee6a1972277e8ebf7eca456ea2ef5ae`.
- Attachment `s_4b1e4d95e54da4237da2c57ed02c08e3`.
- Explicit service/manual-tool receiver used by this active root session:
  `comms-v1 stream notes-codex --json --once`.
- All one-shot root receivers have completed; start another for the next reply.
- Use `comms-v1 post --from notes-codex --to pi:AGENT ...` for actual new transport.

## Newly spawned Pi Claude validation agent

- Managed tmux session: **mcp-e48be70f**, created through the existing
  `claude-sessions.sh create` helper used by pi-mcp/jsplayground.
- Log: `/home/joaohts/observability/logs/claude-sessions/mcp-e48be70f.log`.
- Default Fable5.1 hit its model limit. Selected **Sonnet5 for this session only**
  using `/model` menu's session-only action. Global default was not changed.
- Opened **claude-live-test**, ephemeral/global, via the dedicated new skill and
  this session's actual Claude Monitor stream. Agent ID:
  `a_0ed6e965e20f772e582136b08e924b79`.
- Root sent `msg_pi_claude_roundtrip_1`; actual reply was
  **CLAUDE_V1_ROUNDTRIP_OK**, ID `msg_8c81b54a9629aab90b200d50a5d787ef`.
  Both directions reached durable `handed_off` and root consumed the reply.
- Dedicated skill: `~/.claude/skills/open-comms-v1/SKILL.md`, explicit new binary.
- No completion push has been requested or sent. The agent accepted the benign
  transport check; it did not accept the initial long briefing as authorization
  for a later push. Actual submission must be verified at completion.

## Additional direct production checks

- Restarted Pi `comms-node.service`: brain and Claude receivers reconnected with
  unchanged attachment IDs. The separate GUI was not involved.
- Remote history included `msg_remote_history_isolation` and excluded same-machine
  `msg_local_history_isolation`; local history included both.
- Inspection left both probes `received`, proving reads did not consume them.
- Revoking only history returned `history_denied`; the original messaging/history
  grant was restored after the test. Probe ephemeral agents were closed.
- Compiled core Go tests ran on the Pi's actual ARM64 CPU and passed. Log:
  `~/.local/state/comms/pi-core-tests.log`.
- A 30-second quiet sample measured **19.79 MiB RSS** and **0.467% of one core**
  for the deployed candidate, below the initial 128 MiB / 1% targets.

## Backups

- WAL-safe legacy DB backup (Python sqlite3 backup API), integrity_check=ok:
  `/home/joaohts/.local/state/comms/legacy-backups/comms-20260915T114647Z.db`.
- Legacy unit backup:
  `~/.local/state/comms/legacy-backups/comms-api-unit-before-v1.txt`.
- Legacy port override:
  `/etc/systemd/system/comms-api.service.d/comms-v1-migration.conf`.

## Testing already performed

- Full Go race suite and vet pass; broker, node integration, crypto/store, CLI,
  native adapter, launcher and reader-isolation tests are present in source.
- Ten Python brain tests pass, including actual Unix-node integration.
- Real native Codex idle/busy/provenance/exact-target/TUI-display tests passed.
- Compiled Node->native adapter live test passed before and after node restart.
- Compiled `comms codex` launcher and explicit resume passed, with exact TUI PID,
  environment propagation, ephemeral retirement and independent app-server life.
- Native evidence is under
  `~/.config/comms/codex-adapter-tests/20260915/` on the Mac.
- Real-process SIGTERM regression passed, including a negative control with the
  old context propagation. Shutdown now drains active handoffs before canceling.
- Isolated Codex test TUIs/node/app-servers were stopped after verifying their
  command lines and test paths. Evidence remains; production services untouched.

## Remaining work

1. Completed: both skill installers resolve the installed new CLI when `COMMS_BIN`
   is unset. The focused command-execution regression passes in both repositories.
2. Create final **v0.1.1** through release CI, verify uploaded checksums and upgrade
   both candidates while preserving identities and legacy routing.
3. Repeat actual brain and Pi Claude exchanges on installed v0.1.1.
4. Pin uploaded hashes in Agent Monitor PR 16, verify full bundle/CI, mark ready.
5. Audit VERIFICATION.md, then request the Pi Claude's final jsplayground push,
   verify actual submission, and only then complete the goal.

User delegated remaining engineering details. No new broad approval round is
needed for authorized implementation, deployment, releases, configuration or PR.

## Initial release and upgrade correction

- Published **v0.1.0** at `a22f50041b270e603b5d259a8d8a245d92c1ea89`.
  Release run `34970443555` and main CI `34970443248` passed.
- Downloaded and verified every uploaded platform archive. Mac and Pi now run
  v0.1.0 with unchanged machine IDs, peers and grants. The Pi upgrade also
  preserved legacy CLI/skill content; the brain patch installer was a no-op.
- The actual Mac upgrade exposed an asynchronous launchd removal race: immediate
  bootstrap failed with EIO while the old job was exiting. A later bootstrap
  succeeded and restored the node. v0.1.1 will include a bounded upgrade wait and
  regression test. Published v0.1.0 assets will not be replaced.
- Consistent pre-upgrade backups passed integrity checks in each machine's
  `~/.local/state/comms/node-backups/20260915T124010Z/`.

- The bounded launchd restart function passed against the actual Mac service:
  it returned to a connected state in 16.15 seconds with the same machine ID.
  Four deterministic restart/error cases also pass.
- Both installers now keep skill backups outside active discovery roots under
  `~/.local/state/comms/skill-backups/`; repeat-backup and real command-resolution
  tests pass. The previously created Pi backup was preserved there.
