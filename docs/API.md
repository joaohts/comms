# Local and broker API v1

Working implementation contract. The server returns JSON errors with `code` and
`error`. Mutations validate/commit before returning success. Unix socket is
`<data-dir>/node.sock`; owner-only directory/socket permissions are mandatory.
Default data dir is `$COMMS_DATA_DIR` or `~/.local/share/comms`.

## CLI output views

The HTTP API and existing `--json` records retain their full schemas. `--compact`
is an opt-in JSON view for routine agent calls; it does not change IDs, database
records, encrypted envelopes, receiver fencing, or acknowledgment semantics.

| Command | Compact fields |
|---|---|
| open | id, alias, persistent, scope |
| agents / identities | id, alias, persistent, scope, online; retired when applicable |
| who | address (readable alias), recipient (exact machine:agent IDs), online, persistent |
| post / status MESSAGE | id, state, failure_code when present |
| status | version, api_version, name, machine_id, broker_enabled, broker_connected |
| log / inbox | messages with id, from, to, body, state, created_at, optional failure_code; next_cursor |
| stream | message_id, sender_machine_id, sender_agent_id, body, and peer provenance |

Native Codex deliveries use the same compact peer-content view as `stream
--compact`: kind, authority, origin_authenticated, claims_verified, message_id,
sender_machine_id, sender_agent_id and body. The recipient is already fixed by
the receiver/thread binding. Attempt/attachment IDs, retry counters and lease
timestamps remain private to the receiver. Plain stream output is already short.
`stream --json` still returns the original complete event for existing clients.

Compact `who.recipient` and history `from`/`to` are full immutable addresses,
accepted by `post --to`; aliases are readable labels. Administrative commands
such as `sessions`, `peers`, `grants` and `export` keep full output, and reject
`--compact` rather than silently omitting data required for administration.

Plain `open`, `who`, and `agents` output uses aliases without internal IDs. Use
`--compact` or `--json` when you need exact identifiers programmatically.

`comms serve [--data-dir PATH] [--broker-listen 127.0.0.1:PORT]` runs the service.
Broker HTTPS termination is configured in front of its loopback listener.

## Local routes

| Route | Purpose |
|---|---|
| GET /v1/status | Version, machine ID, public key, role and connection status |
| GET /v1/agents | Local identities; `persistent=true` filters saved identities |
| DELETE /v1/agents/{id} | Retire identity and fail its pending mail |
| POST /v1/sessions | Open/resume and attach atomically |
| PUT /v1/sessions/{id}/lease | Renew attachment |
| DELETE /v1/sessions/{id} | Close attachment |
| GET /v1/sessions/{id}/stream | Receiver stream; never used by GUI observers |
| POST /v1/sessions/{id}/handoffs | Correlated delivery-attempt result |
| POST /v1/messages | Queue a send, resolving alias once |
| GET /v1/messages | Read-only history/inbox filters and pagination |
| GET /v1/messages/{id} | Message status |
| POST /v1/messages/{id}/resolve | Explicit operator resolution of uncertain handoff |
| POST /v1/prune | Prune completed content, retain replay headers until expiry |
| GET /v1/who | Local plus granted remote discovery |
| GET/PUT /v1/peers[/{id}] | List/import out-of-band verified public identities |
| GET/PUT /v1/grants[/{id}] | Versioned messaging/history grants |
| GET /v1/stats | Derived local statistics, excluding pruned records |
| GET /v1/events | Bounded observational change notifications |
| PUT /v1/broker | Configure broker URL and authenticate |
| POST /v1/history | Local or end-to-end encrypted remote history query |

Session request fields: `alias`, `persistent`, `scope` (`local`/`global`), `harness`
(`claude`/`codex`/`service`), `harness_session_id`, optional `delivery_target`,
optional verified `process_id`, `process_started`, and explicit `takeover`.
Response includes `agent` and `session`, containing immutable IDs.
Receiver streams renew their own lease; keepalives never become model notifications.

Codex attachments require an already loaded app-server thread and private Unix
target. The node attaches its native tool-output receiver automatically; a plain
stdout stream cannot consume a Codex attachment. See [Codex integration](CODEX.md).

Uncertain handoffs can be explicitly resolved with `comms resolve ID --status
handed_off|retry|undeliverable [--sender MACHINE_ID]`. Retry is an operator choice
that may duplicate a previously accepted handoff. Positive late confirmation from
the original receiver is accepted only for the unchanged attachment/attempt.

Send request: `session_id`, `to`, `body`, optional `id`. Reply includes the durable
message ID and state. Aliases in `to` are `agent` locally and `peer:agent` remotely.
Sender identity/scope is taken from the attached session, not arbitrary `from` data.

## Broker routes

An operator may require a shared service API key on **every** broker request,
including registration and streams. It is sent in `X-Comms-Service-Key` alongside
the existing machine `Authorization: Bearer ...` credential. A missing/wrong key
returns 401 `service_key_required` before request parsing or registration. This
extra gate does not replace proof of machine-key possession, grants, or encryption.

Configure both server and consumer nodes with `serve --broker-service-key-file
/private/path` (or `COMMS_BROKER_SERVICE_KEY_FILE`). The installer accepts and
preserves the same option. The file must be private (0600), regular, and contain
32–4096 printable non-space ASCII characters; a trailing newline is accepted.
An explicit unreadable/missing/empty file fails startup rather than opening the
broker. With no option or environment setting, the service-key feature is off.
Restart the node to apply or rotate this service key; this is independent of
machine identity keys. Broker clients refuse HTTP redirects to avoid forwarding
credentials elsewhere. Legacy proxy forwarding is also gated and strips this
header before contacting the old server.

Local status exposes only `broker_service_key_configured`, never the key value.
Exchange the service key privately, **not** in public identity bundles or the vault.

| Route | Purpose |
|---|---|
| POST /v1/auth/challenges | Start self-service identity registration/auth |
| POST /v1/auth/complete | Consume purpose-bound possession proof; issue credential |
| PUT /v1/presence | Versioned global-agent snapshot and node heartbeat |
| PUT /v1/grants/{peer} | Authenticated caller's grant/revocation |
| GET /v1/who | Authorized presence only |
| POST /v1/messages | Queue ciphertext subject to receiver's grant and quota |
| POST /v1/receipts | Queue bounded receipts; receiving endpoint validates correlation |
| GET /v1/stream | Ciphertext queue plus separate transient query notifications |
| POST /v1/acks | Delete recipient-owned queue item after durable receipt |
| POST /v1/history/{machine} | Bounded online encrypted query relay |
| POST /v1/history-results/{request} | Correlated encrypted result, not agent mail |

Only `message` and `receipt` are durable envelope kinds. History is a bounded
in-memory HTTP query/response relay, never broker history or an agent inbox entry.
The broker authenticates its caller and cannot claim another sender in request JSON.

## Claude receiver preference

`GET /v1/claude` returns `{"receiver":"monitor"}` or `{"receiver":"channel"}`.
`PUT /v1/claude` stores that object in the existing settings table. Unset means
`monitor`; invalid values return 400 and do not change the preference. No schema
migration is needed. `GET /v1/status` also includes `claude_receiver`.
The preference applies to future `comms claude` launches, without mutating
existing attachments.

A Claude Monitor attachment has an empty `delivery_target`. An MCP channel
attachment uses `claude-channel` and connects to its ordinary stream endpoint
with `?receiver=channel`. The node refuses a mismatched receiver with 409.
Handoff receipts retain the existing semantics for both adapters.

## Agent Monitor integration

CLI `status`, `who`, `identities`, `events` support `--json`; events are NDJSON.
Agent Monitor can use the bundled CLI as its Unix-socket client. Bundle a pinned
release and checksums, install/start the node outside the GUI process lifecycle,
and preserve existing identities/databases. No implicit plaintext-history import.
