# Installation and migration

The release contains one executable for the local node, optional broker, and CLI.
It also contains `scripts/install.sh` and the Claude/Codex `open-comms` integration.
The service runs independently of Agent Monitor. Closing or updating the viewer
does not stop delivery. Go and a compiler are not required on the target machine.
The installer uses the platform's Python 3 for safe service-file generation.

## Verify and install a pinned release

Download the platform archive and `SHA256SUMS` from the same GitHub release. For
a private repository, use your existing `gh` login; never embed a token in the
binary, installer, URL, or Agent Monitor bundle:

```sh
gh release download v0.1.0 --repo joaohts/comms \
  --pattern comms_Linux_arm64.tar.gz --pattern SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf comms_Linux_arm64.tar.gz
./scripts/install.sh
```

On macOS, select `comms_Darwin_arm64.tar.gz` or `comms_Darwin_amd64.tar.gz`. Verify
the matching checksum with `shasum -a 256`. Agent Monitor distributes a pinned
archive and verifies its checksum during bundling, using the same installer.

Defaults:

| Item | Location |
|---|---|
| CLI | `~/.local/bin/comms` |
| Identity and local database | `~/.local/share/comms/node.db` |
| Optional broker database | `~/.local/share/comms/broker.db` |
| Local API | `~/.local/share/comms/node.sock` |
| macOS supervisor | `~/Library/LaunchAgents/com.joaohts.comms.plist` |
| Linux supervisor | `~/.config/systemd/user/comms-node.service` |

Add `~/.local/bin` to the user's PATH. A Pi user service that must survive logout
needs lingering enabled (`sudo loginctl enable-linger USER`) if not already set.
The installer does not change machine-wide services or restart unrelated units.

## Explicit legacy coexistence

Keep the old board/history and new databases separate. Do not import plaintext
history, trust old bearer credentials as peer keys, or overwrite the old database.

For the initial migration, use a separate command name and leave existing skills:

```sh
./scripts/install.sh --binary-name comms-v1 --skip-skills \
  --broker-listen 127.0.0.1:3302
comms-v1 name pi
comms-v1 export --alias pi --json > pi-public.json
```

Exchange public JSON bundles over an authenticated channel outside the broker,
import on each peer, and issue directional grants explicitly. `pair` does not
implicitly grant access. Use the configured HTTPS broker URL for remote nodes.
Local-only nodes require neither broker configuration nor pairing.

The optional `--legacy-proxy-url http://127.0.0.1:3301` lets the new broker forward
non-`/v1/` routes to a separately running legacy broker. This is only a routing
bridge: storage, credentials, and plaintext history remain separate. Moving the
legacy listener and proxy endpoint must be a separately scheduled deployment
operation; the installer does not do it or restart that legacy service.

When ready to use the standard command and new skill, explicitly run with
`--replace-legacy`. Changed files are backed up with timestamped names. Keep those
backups and the old service available until every migrated integration is tested.
For consistent SQLite backups of a live legacy database, use its SQLite backup
API; copying the `.db` alone while WAL is active is insufficient.

## Persistent service identity

```sh
comms open brain --persistent --global --harness service --session-id brain-service
comms stream brain --json
```

The long-running brain ingress reads events, preserves peer provenance, and maps
replies through `comms post --from brain --to PEER:AGENT`. Preserve the existing
`NO_REPLY` loop breaker. An ordinary Claude session must start its stream inside
its own Monitor tool; a service-owned stream cannot wake another model.

## Updating and rollback

Install the selected release using the same data directory. Upgrades preserve the
machine identity, keys, inboxes, and persistent agent IDs. The supervisor stops
the old node with a 15-second outer deadline; the node drains for its configured
10 seconds and leaves pending delivery in SQLite. Clients reconnect. Receiver
interruption alone does not retire an agent identity.

Use `--no-start` to stage service configuration or `--no-service --skip-skills` to
stage only the executable. A failed readiness check reports failure; inspect
`journalctl --user -u comms-node.service` on Linux or `logs/service.err.log` in the
data directory on macOS. Preserve database backups before version changes. A
binary rollback is safe only when its schema version supports the on-disk data;
do not downgrade through an incompatible migration.

Losing the machine private key permanently loses that identity and old encrypted
mail. Create a new identity and re-pair; v1 implements no key replacement/rotation.
