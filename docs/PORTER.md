# Porter

Porter reports the agents running on a machine — status `running` / `needs_you`
/ `done`, title, short summary, durations — to subscribed peers and, optionally,
to their phone as an encrypted push. It is opt-in and off by default.

## Pieces

- `comms porter event ...` (hooks, short-lived) folds lifecycle events into
  `<data-dir>/porter/state.json`. It does not need the node.
- `comms serve --porter` makes the node attach a persistent, global agent with
  alias `porter` (harness `service`). The node-owned receiver takes
  subscriptions, watches `state.json` for replacement and publishes changes.
  There is no separate daemon.
- Subscribers live in `<data-dir>/porter/subscribers.json`, keyed by the peer's
  immutable machine id. No SQLite tables are involved.

## Wire format

All porter traffic is ordinary end-to-end encrypted comms messages whose body
is one JSON object with a `type` field. Pairing, grants, broker queueing and
receipts apply unchanged.

### Peer → `<machine>:porter`

```json
{"type": "porter.subscribe", "push": {"provider": "expo", "token": "ExponentPushToken[...]"}}
{"type": "porter.unsubscribe"}
```

- `push` is optional. A subscribe replaces the previous subscription of that
  peer (one per peer machine), so omitting `push` clears a stored token. The
  app re-sends subscribe on start and whenever its token changes.
- Updates go to the agent that sent the subscribe.
- Accepted only from remote peers that currently hold a messaging grant on
  this node. Local agents read `state.json` (or `comms porter status`) instead.
- Malformed or refused controls are consumed and ignored; there is no reply.
- Revoking the peer's grant drops its subscription at the next publish.

### `porter` → subscriber

```json
{"type": "porter.snapshot", "machine": "personal-mac", "agents": [AGENT, ...], "truncated": false}
{"type": "porter.agent",    "machine": "personal-mac", "agent": AGENT}
{"type": "porter.remove",   "machine": "personal-mac", "id": "<agent id>"}
```

- `snapshot`: on every subscribe and on node start (to existing subscribers).
  Ordered needs_you first, then most recently active; trimmed from the end to
  fit the 64 KiB message limit, with `truncated: true`.
- `agent`: one record per new or changed agent.
- `remove`: the session ended.
- `machine` is the node name (`comms name`).

`AGENT`:

```json
{
  "id": "<harness session id>", "harness": "claude", "title": "pager-refactor",
  "project": "~/notes", "status": "running | needs_you | done",
  "needs": {"kind": "permission", "text": "Bash: npm install"},
  "summary": "...", "special": false,
  "started_at": "RFC3339", "last_active_at": "RFC3339", "status_since": "RFC3339",
  "running_ms": 0, "waiting_ms": 0
}
```

Durations cover closed status periods; clients add `now - status_since` for the
current one. Prompts, transcripts, tool input and file contents are never sent.

## Encrypted push

For subscribers with a push token, porter pushes when an agent enters
`needs_you`, and additionally on every stop of a `special` agent. A freshly
started session is not a stop.

The push is a data-only, high-priority Expo message (no title/body) sent to
`https://exp.host/--/api/v2/push/send`:

```json
{"to": "ExponentPushToken[...]", "priority": "high",
 "data": {"porter": "1", "from": "<sender machine id>", "box": "<base64>"}}
```

`box` is `base64(nonce[24] || nacl.box(plaintext, nonce, subscriberPublicKey,
nodePrivateKey))` — an authenticated box, not a sealed box. The app looks up the
pinned key for `from`, opens the box, and drops anything that fails. Plaintext:

```json
{"machine": "personal-mac", "agent": "<id>", "title": "...", "status": "needs_you",
 "needs": {"kind": "permission", "text": "..."}, "summary": "..."}
```

Title, summary and needs text are clipped so `data` stays under 3 KiB (Expo's
limit is 4 KiB). A `DeviceNotRegistered` answer clears the stored token; the
subscription itself remains. A leaked token only allows droppable spam; Expo
"enhanced push security" stays off.
