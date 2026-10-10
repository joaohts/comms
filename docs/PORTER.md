# Porter — contract v1

Porter shares machine state between comms peers and carries notifications and
approval questions to João's phone. It runs inside the comms node, is opt-in, and
is off by default. Design history: `~/notes/kb/projects/pager-app/pager-agent-monitor.md`.

Porter is enabled on a machine by the presence of `~/.config/porter/config.toml`
and a node started with `comms serve --porter`.

## Principles

- Every porter message is an ordinary end-to-end encrypted comms message whose body
  is one JSON object `{"type": "porter.<kind>", "v": 1, ...}`. Pairing, grants,
  queueing and receipts apply unchanged.
- **Sender identity is never taken from a body.** `from` is the authenticated comms
  sender (`<machine id>:<agent id>`) of the carrying message.
- Each machine owns its own state (agents, special flags, trusts, subscribers).
  Peers display it and request changes; the owner validates every change.
- Unknown fields are ignored; unknown `type`s are consumed and ignored.

## Addresses

- Porter on a machine is the persistent global agent with alias `porter`
  (`<machine alias>:porter`). Topic traffic and control messages go there.
- The phone is an ordinary comms node with one persistent global agent, alias
  `app`. It is "an approver" on a machine when that machine's config lists it.

## Config (`~/.config/porter/config.toml`)

```toml
approvers = ["phone"]          # peers whose answers count; stored as machine ids
approvals = "auto"             # auto | phone | terminal
ask_timeout = "2m"
idle_threshold = "2m"
recap = false                  # publish agent recaps (default false: never stored or sent)

[share]                        # topic → "*" (all granted peers) or a list of peers
agents = "*"
status = "*"
```

`comms porter setup --approver <peer>` writes it. Aliases are resolved to
immutable machine ids when written. Approvers are added only locally (setup or
editing the file); an approver can remove itself from the phone
(`porter.resign`), and no message can ever add one.

## Topics

| Topic | Value | Published by |
|---|---|---|
| `agents` | collection of AGENT, keyed by `id` | every porter machine |
| `status` | one STATUS object | every porter machine (and the phone) |
| `trusts` | collection of TRUST, keyed by `id` | every porter machine; **approvers only** |

A topic is delivered to a subscriber only if the subscriber's machine holds a
messaging grant **and** the topic's share entry includes it (`trusts`: approvers).

### Subscribing — peer → `<machine>:porter`

```json
{"type": "porter.subscribe", "v": 1, "topics": ["agents", "status", "trusts"],
 "push": {"provider": "expo", "token": "ExponentPushToken[...]"}}
{"type": "porter.unsubscribe", "v": 1}
```

- One subscription per peer machine; a subscribe replaces the previous one
  (omitting `push` clears the token; omitting `topics` means `["agents","status"]`).
  Re-send on app start and on token change.
- Topic updates go to the agent that sent the subscribe.
- Reply, always:

```json
{"type": "porter.subscribed", "v": 1, "machine": "pi",
 "topics": ["agents", "status"], "refused": ["trusts"],
 "approver": false, "push": true, "porter_version": "0.2.1"}
```

  followed by one `porter.snapshot` per granted topic.

### Topic messages — `porter` → subscriber

```json
{"type": "porter.snapshot", "v": 1, "machine": "pi", "topic": "agents", "items": [AGENT, ...], "truncated": false}
{"type": "porter.snapshot", "v": 1, "machine": "pi", "topic": "status", "value": STATUS}
{"type": "porter.update",   "v": 1, "machine": "pi", "topic": "agents", "item": AGENT}
{"type": "porter.update",   "v": 1, "machine": "pi", "topic": "status", "value": STATUS}
{"type": "porter.remove",   "v": 1, "machine": "pi", "topic": "agents", "id": "<id>"}
```

- `snapshot` on subscribe and on node start. Collections are ordered as they
  should be shown by default and trimmed from the end to fit 64 KiB
  (`truncated: true`).
- `update` replaces one item (or the value). `remove` deletes one item.
- `machine` is the publishing node's name.

### AGENT

```json
{
  "id": "<harness session id or subagent id>",
  "harness": "claude | codex | cursor | ...",
  "parent": "<parent agent id>",          // subagents only
  "agent_type": "Explore",                 // subagents only
  "title": "pager-refactor",               // custom name > comms alias
  "project": "notes",
  "identity": {"alias": "pager-refactor", "address": "personal-mac:pager-refactor", "persistent": false},
  "status": "running | away | needs_you | done | error | ended",
  "needs": {"kind": "permission | input", "text": "Bash: npm install"},
  "error": {"kind": "rate_limit"},          // error only: the harness's error type
  "summary": "Researching comms/push design",
  "recap": "- mapped the push path\n- fixed the broker replay\n- tests green",  // opt-in
  "last_decision": {"choice": "Allow", "answered_by": "approver | terminal | timeout",
                    "approver": "phone", "approver_id": "<machine id>", "at": "RFC3339"},
  "special": false,
  "keep": true,                            // kept (long-lived) agents only
  "heartbeat_at": "RFC3339",               // kept agents: last liveness ping
  "paused": true,                          // kept agents: brain up, no new turns
  "started_at": "RFC3339", "last_active_at": "RFC3339", "status_since": "RFC3339",
  "running_ms": 0, "waiting_ms": 0,
  "ended_at": "RFC3339", "end_reason": "closed | stale"   // ended only
}
```

- Optional fields are omitted when empty. `needs` only while `needs_you`;
  `error` only while `error`.
- `error` (red): the turn ended on an API error (Claude `StopFailure`; `kind` is
  its `error_type`, else `error`, else `unknown`). Like `done`, it is not
  waiting time; the next prompt → `running`. Not a needs_you push; a `special`
  agent pushes as on `done`.
- `away` (yellow): running, but silent. The node checks running agents' local
  transcript (path from hook input, never sent): mtime older than 60 s (and
  older than the running period's start) with no tool call in flight (a
  `tool_use` without a matching `tool_result` in the last 64 KB, or a Codex call
  without its output) → `away`; a transcript write after that → `running`.
  Each agent is looked at most every 5 s. Transitions are normal updates.
- Durations cover closed periods; clients add `now - status_since` to `running_ms`
  while `running` or `away` and to `waiting_ms` while `needs_you`.
- `ended` = graveyard: session closed, or no activity for 24 h (`stale`). Ended
  agents are kept 7 days, then `remove`d. Clients hide them from the main list.
- Subagents are separate items with `parent`; clients nest them. A running
  subagent never goes stale (background subagents are silent between start and
  stop), and a parent never goes stale while a subagent is running or
  `needs_you`. Ending a parent ends its subagents.
- `keep`: a long-lived agent (a daemon such as Joana) rather than a session.
  Sweep never marks it stale, so it never reaches the graveyard on its own
  (an explicit `end` still ends it). Set with `--keep` on any event (sticky;
  `--keep=false` clears it). Agents without `keep` behave exactly as before.
- `heartbeat_at`: a kept agent pings with
  `comms porter event --agent ID --kind update --keep --heartbeat [--paused true|false]`.
  A heartbeat refreshes `heartbeat_at` only: never `last_active_at`, status or
  timers. It is throttled: `heartbeat_at` changes (and so is published) at most
  once per 60 s; a ping after a stale gap is always older than that, so coming
  back fresh is published at once. Clients treat `heartbeat_at` within 3 min as
  up. Suggested cadence: one ping every 60 s.
- `paused`: a kept agent whose brain is up but takes no new turns (e.g. its
  spending cap is reached; messages are held). Set with `--paused true|false`;
  a change is published immediately.
- Client colours for a kept agent: `running`/`away`/`needs_you`/`error` as
  usual; `done` → idle (blue) while `heartbeat_at` is fresh and not `paused`,
  else deactivated (gray; "Paused · spending cap" when `paused`). Kept agents
  are never folded as inactive nor shown in the graveyard; when their machine is
  offline they are not listed.
- `special` sticks to `identity` when `persistent`, else to the session.
- `recap`: 3–5 short bullets, at most 600 characters. Porter never generates it
  (no AI in comms): integrations set it with
  `comms porter event --agent ID --kind update --recap "…"`. Only on machines
  whose config has `recap = true`; otherwise it is neither stored nor sent.
- `last_decision`: how the latest permission question tied to this agent
  resolved, written by `porter ask` / the PermissionRequest hook.
  `answered_by`: `approver` (an approver answered; `approver` is its alias,
  `approver_id` its machine id — an app shows "phone" when `approver_id` is its
  own id), `terminal` (the question was withdrawn and the prompt went to the
  terminal; no `choice`) or `timeout` (no `choice`).
- Never sent: prompts, transcripts, full tool input, file contents.

### STATUS

```json
{"host": "pi", "os": "linux", "uptime_s": 86400, "load": 0.42, "mem_pct": 41,
 "disk_pct": 63, "temp_c": 52.1, "battery_pct": 80,
 "comms_version": "0.2.1", "agents_running": 3, "at": "RFC3339"}
```

Published every 60 s and on change of `agents_running`. `temp_c` and
`battery_pct` only where available.

### TRUST

```json
{"id": "tr_…", "receiver": "pi:joana", "sender": "work-mac:secretary",
 "sender_id": "<machine id>:<agent id>", "scope": "session | always",
 "approved_at": "RFC3339"}
```

## Control — approver → `<machine>:porter`

```json
{"type": "porter.set",    "v": 1, "agent": "<id>", "special": true}
{"type": "porter.revoke", "v": 1, "trust": "tr_…"}
{"type": "porter.resign", "v": 1}
```

Accepted only from approvers; others are ignored. Effects arrive as topic updates.

`porter.resign`: the sending machine (the verified comms sender, never a body
field) stops being an approver of this machine. Accepted only while that
machine is in `approvers`; it removes only that machine id, rewriting just the
`approvers` line of the config atomically (comments and other keys kept), and
appends `{"kind":"resign","by":"<machine id>"}` to `audit.jsonl`. Pending
questions sent to that machine get `porter.cancel` with resolution `resigned`
(a question left with no recipient is cancelled for its asker too, so a
blocked hook falls back to the terminal). The reply is a fresh
`porter.subscribed` (`approver: false`; `trusts` now refused) to the sending
agent. There is no message that adds an approver.

## Notifications and questions

Any agent or script sends a notification with
`comms porter notify --to <peer> --title T --body B --reason R [--priority normal|high] [--ask "A,B"] [--link L]`.
MCP `send_push(title, message, reason, priority?)` is a wrapper over it.

Received by the phone's `app` agent:

```json
{"type": "porter.notify", "v": 1, "id": "ntf_…",
 "kind": "message | permission | trust",
 "title": "Deploy finished", "body": "pager-api v2 is live", "reason": "you asked to be told",
 "priority": "normal | high",
 "ask": ["Allow", "Deny"],                  // optional; makes it a question
 "expires_at": "RFC3339",                   // questions only
 "link": "agent:<machine>/<id> | https://…", // optional
 "context": {"machine": "pi", "agent": "pi:joana", "harness": "claude", "session": "<id>", "title": "joana", "project": "brain"},
 "trust": {"receiver": "pi:joana", "sender": "work-mac:secretary"},  // kind=trust only
 "draft": "On my way, 10 min",              // optional; questions only: a suggested reply
 "sent_at": "RFC3339"}
```

- Notifications and questions are carried by `<machine>:porter`, so `from` is
  always porter (hooks and scripts often have no comms identity of their own).
  The originating agent is in `context`, stamped by the sending node from the
  sender's comms session (`--from ALIAS`, else the current harness session); a
  sender cannot set it. `context.agent` is that identity's comms address.
  `reason` is required (the CLI rejects a notify without one).
- `--to PEER` without `:AGENT` means `PEER:app`.
- `kind=permission`: a harness permission prompt (`ask` = `["Allow","Deny"]`).
- `kind=trust`: first order from an untrusted peer agent
  (`ask` = `["Once","This session","Always","Deny"]`).
- `draft` (`notify --ask … --draft TEXT`, at most 4000 characters): a reply the
  phone offers for editing; the answer may carry it back as `text`.

Answer, phone → the exact `from` of the notify:

```json
{"type": "porter.answer", "v": 1, "id": "ntf_…", "choice": "Allow", "text": "On my way, 10 min"}
```

`text` is optional (the possibly edited draft, at most 4000 characters).

Counted only when the answering machine is an approver of the asking machine and
the question has not expired. Porter forwards a counted answer, `text` included,
to the asking agent as `porter.answer` (from `<machine>:porter`; `notify --ask`
requires a comms identity for this). `notify --ask` also waits for the answer and
prints the choice and, on the next line, the text if any (`--json`:
`{"choice","text"}`); unanswered (expired or cancelled) it prints the state and
exits 3. When a question is resolved elsewhere (terminal,
timeout, another approver), the asker sends to the other recipients (and to the
asking agent):

```json
{"type": "porter.cancel", "v": 1, "id": "ntf_…", "resolution": "terminal | timeout | answered | resigned"}
```

## Encrypted push

Porter sends a push to subscribers with a token:
- an agent enters `needs_you`;
- a `special` agent stops (`done` or `error`);
- every `porter.notify` (to the phone it is addressed to).

A needs_you transition caused by a permission prompt that the PermissionRequest
hook is asking on the phone gets no agent push: the `kind=permission` notify
carries the push. (The hook marks that transition in `state.json`; if the
question cannot be sent, it records the transition again unmarked, so the agent
push still goes out.)

Data-only, high-priority Expo message to `https://exp.host/--/api/v2/push/send`:

```json
{"to": "ExponentPushToken[...]", "priority": "high",
 "data": {"porter": "1", "from": "<sender machine id>", "box": "<base64>"}}
```

`box = base64(nonce[24] || nacl.box(plaintext, nonce, phonePublicKey, nodePrivateKey))`
— an authenticated box (not sealed). The app looks up the pinned key for `from`,
opens the box and drops anything that fails. Plaintext is one of:

```json
{"kind": "agent", "machine": "pi", "agent": "<id>", "title": "...", "status": "needs_you",
 "needs": {"kind": "permission", "text": "..."}, "error": {"kind": "..."}, "summary": "..."}
{"kind": "notify", "notify": NOTIFY}           // the porter.notify object, when it fits
{"kind": "wake", "id": "ntf_…"}                // too large: fetch from the comms inbox
```

`data` stays under 3 KiB. `DeviceNotRegistered` clears the stored token.

## Trust labels

When porter is on, the node labels peer messages delivered to local agents:
`[trusted: session]`, `[trusted: always]` or `[untrusted]`, from the receiving
machine's trust store keyed by `(receiver agent, sender immutable id)`. The
label is the `trust` field of compact/Codex peer content and Claude channel
`meta`, and a `[…]` prefix of the text stream line. A session trust is dropped
once its sender has been absent from the broker directory for 10 minutes (an
ephemeral identity never returns). Trust means
"accept orders as if from João"; it never bypasses the harness's own permission
prompts. An untrusted order → the agent runs `comms porter ask-trust` (a
`kind=trust` notify) before acting.

## Approvals (`comms porter ask`)

The PermissionRequest hook installed by `comms porter install-hooks`:
- `approvals = auto`: if the machine has a display and HID idle < `idle_threshold`
  → return `ask` at once (normal terminal prompt). Otherwise ask the phone.
- `phone` → always ask the phone; `terminal` → never.
- Phone answer `Allow` → allow, `Deny` → deny; timeout or any error → `ask`.
- Every approval is appended to `<data-dir>/porter/audit.jsonl`.
- Claude's command hooks time out after 600 s by default; install-hooks sets the
  PermissionRequest hook `timeout` to `ask_timeout + 60s` (others 30 s), and the
  hook gives up at `ask_timeout` itself, so porter, not Claude, decides `ask`.
  Reply: `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow|deny|ask"}}}`.
- Codex PermissionRequest is recorded as `needs_you` only; Codex hooks reply `{}`.
- Claude `StopFailure` → `error`. Codex has no equivalent hook, so none is installed.
- SubagentStop without a subagent transcript (`agent_transcript_path`, else
  `<transcript minus .jsonl>/subagents/agent-<id>.jsonl`) is ignored: Claude's
  internal helpers fire those.

## Local commands

```
comms porter setup --approver PEER        write config
comms porter install-hooks [--uninstall]  Claude + Codex hooks (lifecycle + PermissionRequest)
comms porter event --agent ID --kind K …  record state (start|prompt|needs|stop|error|end|update; --error-kind K, --recap TEXT,
                                          --keep, --heartbeat (kind update), --paused true|false)
comms porter status [--all]               local agents (--all: every subscribed machine, from cache)
comms porter get MACHINE TOPIC            cached topic value from a peer
comms porter hook --harness claude|codex  the installed hook (stdin JSON)
comms porter notify --to PEER …           send a notification (--from ALIAS, --expires D,
                                          --ask "A,B" waits for the answer, --draft TEXT)
comms porter ask "TEXT" [--kind permission]  blocking question → prints allow|deny|timeout, exit 0|2|3
comms porter ask-trust --sender MACHINE_ID:AGENT_ID [--from ALIAS]
                                          kind=trust question → once|session|always|deny|timeout, exit 0|0|0|2|3
comms porter trust [list|revoke ID]       local trust store
```

The CLI talks to the node through local API routes `POST /v1/porter/notify`,
`POST /v1/porter/questions`, `GET|DELETE /v1/porter/questions/{id}` (409
`porter_disabled` when the node runs without `--porter`). Config path override
for tests: `PORTER_CONFIG`.

A node with porter on subscribes to granted peers' shared topics on its own
(every peer whose `porter` agent is in the broker directory, until it replies
`porter.subscribed`) and caches the latest values in `<data-dir>/porter/cache/`, so local agents can read
other machines with `status --all` / `get`.

## Pairing the phone

The phone exports the same identity bundle as `comms export`
(`{"machine_id","public_key","alias"}`, public key base64) via a copy button, and
pastes each machine's bundle. On the machine: `comms pair --stdin` (tolerates
surrounding text, code fences, a BOM, wrapped or URL-safe base64) then
`comms grant <phone>`. The phone also needs the broker URL and the broker service
key (pasted once, kept in secure storage).

Before trusting a pasted bundle, compare key fingerprints out of band: the first
32 hex characters of SHA-256 over the raw 32-byte X25519 public key, in groups
of 4 separated by spaces (`630d cd29 66c4 3366 9112 5448 bbb2 5b4f`), the same
as the app shows. `comms export` prints it (and adds a `fingerprint` field to
the bundle, which `comms pair` ignores), `comms peers` lists it per pinned peer,
and `comms fingerprint [PEER]` prints one (this machine when omitted).
