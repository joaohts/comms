# Retire the voice daemon's legacy broker connection

The voice daemon already delegates to the brain through `spike.ask_brain` HTTP,
and the brain already sends local wake/announcement triggers to
`127.0.0.1:3403/trigger`. Retirement removes only the redundant legacy comms
registration, stale-inbox drain, idle inbox/NACK poller, and close operation.

The patcher preserves microphone/wake detection, speaker identification, chimes,
model/audio sessions, the HTTP handler body and its loopback binding. It does not
import or run `daemon.py`, read configuration/secrets/model databases, install
anything, send triggers, or restart a service.

## Prepare and apply

From a comms checkout on the Pi:

```sh
python3 integration/voice/retire_legacy_voice.py \
  --daemon /home/joaohts/voice-assistant/daemon.py --dry-run
```

The JSON report includes original/patched SHA256 and preservation checks. For a
private staged source copy without changing the daemon:

```sh
python3 integration/voice/retire_legacy_voice.py \
  --daemon /home/joaohts/voice-assistant/daemon.py \
  --stage /home/joaohts/.local/state/comms/voice-stage/daemon.py
```

After the coordinated cutover is ready, apply with the original SHA from the
preflight report:

```sh
python3 integration/voice/retire_legacy_voice.py \
  --daemon /home/joaohts/voice-assistant/daemon.py \
  --expected-sha256 ORIGINAL_SHA256 \
  --backup-dir /home/joaohts/.local/state/comms/voice-backups
```

Only `daemon.py` and a checksum manifest are backed up. Backup directories are
mode `0700`, backup files mode `0600`; the daemon's original mode is preserved.
The patcher verifies the AST, compiles the resulting source without executing it,
and rejects unexpected/partial legacy shapes or changed source. Reapplication
is idempotent. Source changes take effect at the operator-coordinated voice
service restart; this tool never restarts it.

## Rollback

```sh
python3 integration/voice/retire_legacy_voice.py --rollback PRINTED_BACKUP_DIRECTORY
```

Rollback validates both checksums and refuses to overwrite edits made since
migration. No other voice/brain files, environment files or databases are read,
copied or restored.

## Safe tests

```sh
python3 -m unittest discover -s integration/voice -p 'test_*.py' -v
```

Tests check the exact removal boundary, source-drift rejection, private backups,
stage/rollback behavior and idempotence. Isolated function tests use fake chimes,
fake model sessions and in-memory HTTP streams; they never import hardware/model
modules, open microphones/speakers, bind ports or send network notifications.
