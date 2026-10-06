# Installation

The release contains one executable for the local node, optional broker, and CLI.
It also contains `scripts/install.sh` and the Claude/Codex `open-comms` integration.
The service runs independently of Agent Monitor. Closing or updating the viewer
does not stop delivery. Go and a compiler are not required on the target machine.
The installer uses the platform's Python 3 for safe service-file generation.

## Verify and install a pinned release

Download the platform archive and `SHA256SUMS` from the same GitHub release:

```sh
version=v0.1.7
archive=comms_Linux_arm64.tar.gz   # or comms_Linux_amd64 / comms_Darwin_arm64 / comms_Darwin_amd64
base=https://github.com/joaohts/comms/releases/download/$version
curl -fLO "$base/$archive" -fLO "$base/SHA256SUMS"
grep " $archive\$" SHA256SUMS | shasum -a 256 -c -
tar -xzf "$archive"
bash scripts/install.sh
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

The installer also adds managed `claude` and `codex` aliases to the installing
user's zsh (`$ZDOTDIR/.zshrc`, default `~/.zshrc`) or bash (`~/.bashrc`)
configuration. Open a new shell or source that file, then use `claude`, `codex`,
or `codex resume THREAD` to launch with comms receiving support. Bash login shells
must source `.bashrc`. The aliases use the installed comms executable's absolute
path. Existing custom `claude`/`codex` aliases or functions are preserved; changed
config files are backed up, and rerunning the installer updates the managed blocks
without duplicating them. Other shells receive manual launch guidance.

Use `command claude ...` or `command codex ...` (for example `command codex exec`
or `command codex login`) to bypass the comms launcher. Pass `--skip-claude-alias`
or `--skip-codex-alias` to leave shell configuration untouched; `--skip-skills`
also skips alias setup. To remove a shortcut, delete its `COMMS CLAUDE ALIAS` or
`COMMS CODEX ALIAS` block from the shell config and run `unalias claude` or
`unalias codex` in existing shells. The shortcuts affect terminal launches only;
they do not attach sessions launched by the Claude or Codex apps.

## Run your own broker

Same-machine messaging needs no broker. Cross-machine traffic goes through one
broker, which stores only ciphertext queues. Any comms node can run the broker
role alongside its local node; enable it with the installer on the chosen host:

```sh
bash scripts/install.sh --broker-listen 127.0.0.1:3302
```

The broker listens on plain HTTP, and nodes accept an HTTP broker URL only on
loopback (`--allow-insecure` exists for trusted local testing only). For other
machines, put a TLS reverse proxy in front of the listener, for example Caddy
(`reverse_proxy 127.0.0.1:3302`) or `tailscale serve`, then point each node at
the HTTPS origin:

```sh
comms broker https://broker.example.com
```

Then pair machines and grant access as described in the main README's
cross-machine section.

### Service API key

Configure a service key for any internet-reachable broker. Use a separate, randomly generated shared API key to restrict access to an
instance, including new-machine registration. Save it in a private file on the
broker and each authorized consumer node; distribute it through a private channel.
Do not put it in a URL, command-line value, repository, vault, or public export.

```sh
chmod 600 "$HOME/.local/share/comms/broker-service-key"
./scripts/install.sh --broker-service-key-file "$HOME/.local/share/comms/broker-service-key"
```

The installer records the file path in the supervisor and preserves it on later
updates. The process reads the file at startup. Missing or invalid explicitly
configured files fail closed. Without configuration, this extra gate is optional.
`comms status --compact` reports only whether a service key is configured.
Machine key verification, authentication and directional grants remain required.

## Persistent service identity

```sh
comms open brain --persistent --global --harness service --session-id brain-service
comms stream brain --json
```

A long-running service reads events, preserves peer provenance, and maps replies
through `comms post --from ALIAS --to PEER:AGENT`. The brain ships its own
integration; see [brain integration](../docs/BRAIN.md). An ordinary Claude session must start its stream inside
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
