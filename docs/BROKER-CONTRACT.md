# Broker implementation contract

The optional broker role opens **`broker.db` independently** of the local node.
`NewBroker(Config)`, `Handler()`, and `Close()` are its embedding interface. The
deployment supplies HTTPS termination; the HTTP handler works behind a loopback
reverse proxy. No broker endpoint receives a client's private key or plaintext
message/history body.

## Authentication

1. `POST /v1/auth/challenges` with `{machine_id, public_key}` returns `{id, sealed}`.
   Byte slices use JSON base64 encoding.
2. Decrypt `sealed` with NaCl `box.OpenAnonymous`. Its JSON plaintext is
   `{purpose:"comms-auth-v1", id, machine_id, public_key, secret, expires_at}`.
   The client verifies the complete purpose/identity binding before proceeding.
3. `POST /v1/auth/complete` with `{id, proof:secret}` returns `{token, expires_at}`.
4. Subsequent requests authenticate with `Authorization: Bearer <token>`.

Challenges last 60 seconds, are consumed by the first completion attempt, and are
held only in bounded memory. The broker keeps a hash of the proof secret, never
the secret itself. Low-order X25519 public keys are rejected. A machine is not
registered before proof succeeds. A registered ID cannot change its public key.

Credentials last 24 hours and are stored as SHA-256 hashes in `machines`. A new
authentication replaces that machine's prior credential and closes its prior
receive stream. This rotates a bearer credential, **not the machine key**.
Registration itself grants no access to any peer.

## Presence and grants

- `PUT /v1/presence` accepts `{revision, agents:[]Presence}`. It replaces only the
  authenticated machine's global-agent snapshot. Machine IDs are stamped by the
  broker; another machine's ID is rejected. Broker-local petnames are not accepted.
- Revision numbers are positive, increasing integers. Repeating an identical
  revision/snapshot renews its lease. A lower revision or changed snapshot at the
  same revision returns `409 stale_revision`. Snapshot hashing ignores submitted
  lease timestamps; the broker sets its own lease deadlines.
- `PUT /v1/grants/{peer}` accepts `Grant`. It writes the authenticated machine's
  outbound grant to that peer. Disabled permissions remain as versioned tombstones.
- `GET /v1/who` exposes only machines granting the caller messaging/discovery
  permission. Online requires a current receive stream, a current machine lease,
  and an advertised online agent. Persistent advertisements remain discoverable
  while offline; ephemeral offline entries are omitted.
- Broker restart clears machine leases; nodes reconnect and republish snapshots.
- Local nodes must independently enforce grants. A compromised broker cannot be
  trusted to enforce access control or provide authoritative peer public keys.

## Ciphertext queue and stream

`POST /v1/messages` and `POST /v1/receipts` each accept an `Envelope` directly. Its
sender must match authentication and its kind must match the endpoint. Routing,
version, nonce length, ciphertext size, recipient registration, and expiry are
validated before admission. Only ordinary messages need the recipient's messaging
grant; receipts are separately bounded and their correlation is checked by the
receiving node after decryption.

New work returns `202 {id,state:"queued"}`. An identical retry returns
`200 {id,state:"queued",duplicate:true}`. Reusing a pending `(sender_machine_id,id)`
for a different envelope returns `409 id_conflict`. No plaintext or separate
broker traffic history is stored.

`GET /v1/stream` is NDJSON. There is one current stream per machine; a new one
closes the prior connection. Events are:

```json
{"type":"envelope","envelope":{}}
{"type":"history_query","query":{}}
{"type":"heartbeat"}
```

The broker checks grants again before emitting messages. Revoked work stays
blocked until permission returns or expiry; the broker does not fabricate a
recipient-authenticated delivery receipt. Work already in flight cannot be recalled.
Grant changes prompt a queue rescan. Each stream replays remaining unacknowledged
items on reconnect and every 30 seconds, allowing temporary recipient backpressure
to recover without requiring connection replacement.

`POST /v1/acks` takes `{sender_machine_id,id}` and idempotently removes only a queue
item belonging to the authenticated recipient. An acknowledgment from another
machine has no effect. The node sends this only after durable acceptance and
durable receipt obligation. Broker acknowledgment is not end-to-end delivery proof.

The pending message quota and receipt reserve use separate counts and stored-byte
totals per recipient. Full queues return `429`; accepted work is never evicted for
newer work. Expired queue entries are deleted. Completed queue headers are not
retained after acknowledgment, so final replay deduplication belongs to the node.

## Encrypted online history

- `POST /v1/history/{machine}` accepts `RelayQuery` with the authenticated sender,
  target machine, request ID, sealed query, and deadline. A history grant and an
  online target stream are required.
- The stream receives `type:"history_query"`, with the original `RelayQuery`.
- The target replies using `POST /v1/history-results/{id}` and `{sealed}`. Only the
  expected authenticated machine can answer. The waiting query returns `{sealed}`.
- Nodes authenticate/decrypt both directions and enforce history scope/permission;
  the broker merely correlates an online request and its encrypted response.
- Queries last at most 10 seconds. Offline/disconnected targets return `503`, a
  timeout returns `504`, and missing/revoked history grants return `403`.
- Grant revocation cancels waiting queries and permission is rechecked before
  returning a result. Queries, results, and their correlation are never persisted.

## Resource boundaries

Defaults come from `Config`; message quotas are 10,000 envelopes / 256 MiB per
recipient, with a separate 2,048-envelope / 8 MiB receipt reserve. Ciphertext for
receipts is capped at 4,096 plaintext bytes plus encryption overhead. Nodes enforce
the 64 KiB message body limit after decryption; the broker bounds encrypted wire
size instead.

There are 32 concurrent short handlers, 256 live machine streams, 16 concurrent
history queries (at most 4 per requester / 8 per target), 1,024 outstanding auth
challenges (at most 8 per machine ID), and bounded presence snapshots. History
payloads are capped at 2 MiB of encrypted data in each direction. These are
implementation resource bounds, not broker-side analytics or per-account billing.

SQLite transactions and result cursors are released before network writes. Stream
output has a five-second write deadline, cleared between emissions so healthy idle
streams remain open. Shutdown cancels streams/queries before closing the database.

## Tests

`go test -race ./internal/comms -run TestBroker` covers proof/credential lifecycle,
key mismatch, low-order keys, directional presence, grant revisions and revocation,
durable queue/reconnect/ack/deduplication, ciphertext-only persistence, routing
abuse, expiry, independent receipt quotas, encrypted history, timeout/offline
behavior, wrong-peer results, request bounds, and database restart.
