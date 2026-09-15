# Persistent Joana brain integration

`integration/brain/comms_v1.py` adds a separate comms-v1 ingress to the existing
`joana-brain.service`. The legacy ingress, HTTP `/turn` server, model, memory,
global `run_turn` lock, voice and WhatsApp paths remain in place.

The adapter uses Python's standard library and HTTP over the node's Unix socket.
It does not scrape CLI output or read the node's keys. The Agent Monitor GUI is
not needed on the Pi.

## Behavior

- Opens `brain` with `persistent=true`, `scope=global`, and `harness=service`.
- Uses the brain process PID plus its boot-ID/start-tick identity on Linux. The
  harness session name is stable within that process. A node restart reconnects
  the same attachment; a brain process restart resumes the persistent agent ID
  after the node confirms the old process exited.
- Renews the lease every 15 seconds on a dedicated thread, independently of model
  turns, delivery acknowledgments, and replies.
- Accepts incoming messages into a private, bounded SQLite journal **before**
  reporting `handed_off`. This confirms process-level admission; it does not claim
  the model understood or completed the task.
- Runs one journal job at a time through the existing `run_turn`. Other brain
  channels still serialize through the same existing global lock.
- Automatically replies to the exact original `machine_id:agent_id`. Reply IDs
  remain stable across HTTP retries and adapter restarts. Replies use ordinary
  message grants; they do not use the protocol receipt exception.
- Preserves the legacy `NO_REPLY` breaker: blank replies and replies containing
  `NO_REPLY` within their first 40 characters produce no automatic reply.

## Trust and provenance

Every turn includes stamped machine ID, agent ID, message ID, and an explicit
external-agent-content label. Message bodies cannot set the envelope's sender or
tier. Authentication identifies the peer machine; it does not make peer text a
human instruction or establish its factual truth.

**The default tier is `unknown`, including for paired machines.** Only machine IDs
explicitly listed in `COMMS_V1_TRUSTED_MACHINES` receive the existing brain `owner`
tier. For João's fleet, configure the verified Mac and Pi IDs deliberately. This
adapter does not change the existing brain tool-policy definitions.

## Installation

Run from the checked-out comms release source, using the brain's existing Python
environment or a compatible Python 3.10+ installation:

```sh
python3 integration/brain/patch_brain.py \
  --root /home/joaohts/voice-assistant \
  --backup-dir /home/joaohts/.local/state/comms/brain-backups
```

The installer validates all modified Python files before writing, backs up the
three affected paths with checksums, then installs the module and small hooks in
`brain/server.py` and `brain/tools.py`. Reinstalling identical code is a no-op.
`--dry-run` prints the proposed diff. It never reads or changes `.env`,
`.comms-env`, `brain/config.toml`, or service configuration, and never restarts a
service.

Keep the brain service's bare `comms` command resolving to the **legacy CLI**
during parallel migration: the unchanged legacy ingress/tools use legacy flags.
The new adapter accesses the node socket directly and does not need the new Go
CLI on this service's PATH.

## Enable the service integration

Create a user-service drop-in named
`~/.config/systemd/user/joana-brain.service.d/comms-v1.conf`:

```ini
[Service]
Environment=COMMS_V1_ENABLED=1
Environment=COMMS_V1_SOCKET=/home/joaohts/.local/share/comms/node.sock
Environment=COMMS_V1_STATE=/home/joaohts/voice-assistant/brain/data/comms-v1/adapter.db
# Replace these public identifiers with the verified fleet machine IDs.
Environment=COMMS_V1_TRUSTED_MACHINES=MAC_MACHINE_ID,PI_MACHINE_ID
```

Use the actual installed node socket path. Pair machines and establish the needed
directional grants separately. The adapter does not grant access, replace keys,
or trust identities obtained from the broker.

```sh
systemctl --user daemon-reload
systemctl --user restart joana-brain.service
```

Only restart the brain unit during this integration. No restart of
`openclaw-gateway`, voice, WhatsApp, or the legacy broker is needed. Node outages
do not stop the brain's other channels; the adapter reconnects automatically.

Optional settings:

| Variable | Default |
|---|---|
| `COMMS_V1_ENABLED` | Disabled |
| `COMMS_V1_SOCKET` | `~/.local/share/comms/node.sock` |
| `COMMS_V1_STATE` | `comms-v1/adapter.db` beside the configured brain DB |
| `COMMS_V1_ALIAS` | `brain` |
| `COMMS_V1_TRUSTED_MACHINES` | Empty; all senders get `unknown` tier |
| `COMMS_V1_QUEUE_COUNT` | 32 active jobs |
| `COMMS_V1_QUEUE_BYTES` | 4 MiB of pending input/reply data |

## Journal, backpressure and recovery

The journal is application state owned by the brain adapter; it is separate from
both comms role databases and from the existing brain model/memory database. Its
file is mode `0600`, with SQLite WAL and FULL synchronization. Completed bodies
are cleared after processing/submission; duplicate-detection headers remain until
the original message expires. Pending/uncertain work is not silently evicted.

When admission is full, the adapter reports a retry outcome and leaves the work
at the node. It never acknowledges admission that did not happen. Lost handoff
responses are retried idempotently. Duplicate notifications update the current
delivery-attempt reference without running the model again. Lost reply-submission
responses reuse the same outgoing message ID.

A brain process crash during `run_turn` makes that job **uncertain** on restart;
tools might already have run. It is held for operator review and is not replayed
automatically. Queued jobs survive. The adapter cannot cancel a currently running
model call; normal close stops new admissions/work and waits for a bounded drain.

Inspect identity, queue counts and attention records without printing message
bodies or changing live journal state:

```sh
python3 -m brain.comms_v1 status \
  --state /home/joaohts/voice-assistant/brain/data/comms-v1/adapter.db
```

An `ack_state=stale` entry means the node's handoff window ended before an
acknowledgment could be recorded. The journal proves process admission; inspect
the corresponding record and reconcile the node's uncertain handoff with its
explicit `comms resolve MESSAGE_ID --status handed_off --sender MACHINE_ID`
operation. Resolving transport admission does not claim model/task completion.

## Explicit forwarding

The small tools hook supports:

```text
send_to(channel="comms-v1:MACHINE_ID:AGENT_ID", message="...")
```

It uses the current persistent brain attachment and returns the node's queued
message ID. Failed submissions return an error and are not recorded as delivered
messages in the destination thread. Existing `comms:`, voice, WhatsApp and CLI
delivery remain unchanged. Legacy discovery/spawn tools continue using the old
board during this parallel phase.

## Disable and rollback

Set `COMMS_V1_ENABLED=0` (or remove the drop-in), reload user units, then restart
only the brain. Preserve the journal for outstanding-work inspection.

To restore source files, use the backup directory printed by installation:

```sh
python3 integration/brain/patch_brain.py --rollback /path/to/printed/backup
```

Rollback first verifies all installed checksums and refuses to overwrite later
edits. It restores only the three adapter/hook files; it leaves secrets, model
data, legacy history, node state and the journal untouched.

## Tests

```sh
python3 -m unittest discover -s integration/brain -p 'test_*.py' -v
```

The tests use fake `run_turn` callbacks. With Go installed they build and run a
real local comms node; alternatively set `COMMS_TEST_BINARY` to a built binary.
They cover provenance/trust, bounded admission, duplicate attempts, lost handoff
and reply responses, `NO_REPLY`, persistent adapter resume, node restart,
uncertain model-turn recovery, private storage, and reversible installation.
No real model/API calls or remote service changes are made by these tests.
