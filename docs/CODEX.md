# Codex delivery

Comms delivers peer messages through Codex app-server's native
`turn/start.toolOutput` API. Peer text appears as `comms_receive` tool output,
with an empty user-input array. It is never pasted into a terminal, passed to
`codex queue --message`, or promoted to a user message.

The native API starts generation for an idle thread and queues tool output into
an active turn. The installed Codex 0.154.0 schema and the live tests below
confirm that behavior. See the [official app-server documentation](https://learn.chatgpt.com/docs/app-server#start-a-turn).

## Runtime arrangement

```text
Codex terminal ─── private Unix socket ─── Codex app-server
                                              ↑
                                          comms node
```

Agent Monitor, the comms node, and Codex app-server have separate process
lifecycles. Updating or restarting comms must not terminate Codex app-server or
its terminal clients. Closing Agent Monitor must not affect either one.

The supported launcher is `comms codex`, which starts/connects to the dedicated
local app-server and then launches the normal remote Codex terminal. It must
preserve user model and permission settings. New sessions and explicit resumes
use the same arrangement:

```sh
comms codex
comms codex resume <exact-thread-uuid>
```

Put comms flags before `codex`, for example `comms --data-dir /private/comms
codex resume <uuid>`. Arguments after `codex` belong to Codex and are preserved,
including configuration overrides and prompts. The launcher owns `--remote`;
use Codex directly for a different remote endpoint.

New sessions and resumes use the directory where `comms codex` is invoked.
The launcher passes it explicitly to Codex so reusing the shared app-server does
not select the server's data directory. An explicit `-C` or `--cd` argument
overrides this default.

The on-demand app-server uses `<data-dir>/codex/control.sock`, a startup lock,
and a private PID/start-identity record. Concurrent launches reuse one server.
A stale socket is removed only when its recorded owner is confirmed ended and
its listener refuses connections. A timeout never triggers replacement of a
live process. Runtime logs remain in `<data-dir>/codex/app-server.log`.

Inside that session, `open-comms` opens the desired local or global comms
identity. The node attaches the native delivery adapter automatically; users do
not start a separate Codex `comms stream` or terminal-injection listener.

### Launcher contract

1. Start app-server independently of the node and GUI, using an explicit
   `unix:///absolute/path/control.sock` endpoint. The socket must have mode
   `0600` in a directory owned by the current OS user and not writable by other
   users. Do not expose a TCP port for local delivery.
2. Preserve the actual terminal process ID: a launcher can `exec` the Codex TUI
   so its PID remains unchanged.
3. Supply per-thread shell overrides on the TUI command line:

   ```text
   -c shell_environment_policy.set.COMMS_HARNESS_PID="<terminal-pid>"
   -c shell_environment_policy.set.COMMS_CODEX_TARGET="unix:///.../control.sock"
   ```

   Pass these as separate argument strings, not shell-interpolated commands.
   Exporting variables only into the terminal process is insufficient: tool
   shells are spawned by the shared app-server. Codex itself supplies the
   current `CODEX_THREAD_ID`. Refresh the PID override on every explicit resume.
   The launcher also supplies `COMMS_DATA_DIR`, `COMMS_SOCKET`, and `COMMS_BIN`
   so tool calls use the intended node and CLI.
   Integrations should invoke `"$COMMS_BIN"` when it is available. Stale inherited
   parent-session identity variables are removed.
   The [shell environment policy](https://learn.chatgpt.com/docs/config-file/config-sample)
   can additionally filter variables; report missing launcher values clearly.
4. Register the attachment with that exact thread UUID, socket endpoint, and
   terminal PID plus process start identity. Do not infer ownership from the
   shell's parent: it is the shared app-server process.
   The CLI requires a live verified `COMMS_HARNESS_PID` or explicit
   `--process-id` for Codex; Claude retains its existing ancestry detection.
5. Before becoming ready, call `codex.ValidateSession`. It reads thread metadata
   without loading, resuming, or modifying the thread. Only a matching, loaded
   thread with `canAcceptDirectInput=true` and `idle`/`active` status is ready.
6. Revalidate readiness on reconnect and before handoff. A missing socket or
   unloaded thread means unavailable. It never authorizes silent migration,
   terminal targeting by name, or creation of another thread.

For an ordinary wrapper-launched ephemeral attachment, the wrapper/TUI's
lifetime bounds the comms attachment. Validate PID reuse using its start time.
A TUI disconnect is **not proof that Codex engine work stopped**: remote Codex
can keep a turn running. Closing that attachment stops future comms delivery
without interrupting the app-server or other threads. Persistent comms identity
and inbox survive attachment closure. A node restart preserves an attachment
whose actual owning process still exists.

Codex 0.154.0 rejects explicit permission overrides when resuming a remote
thread. Do not append `-a`, `-s`, or bypass flags on resume; retain
the saved settings.

## Existing sessions

A normal Codex TUI without an external app-server socket cannot accept this
delivery method. It must not be marked ready or silently attached to another
process. Finish or pause work, exit that TUI normally, then explicitly resume its
saved UUID using `comms codex resume <uuid>` and reopen comms. Do not keep two
live owners of the same session. The saved Codex conversation is retained.

## Delivery result and provenance

The node supplies a JSON payload containing the message ID, authenticated sender
machine/agent, and body. Sender verification happens before this adapter. The
payload must label the body as external peer content: verified origin does not
prove factual accuracy or confer user authorization.

The adapter sends only these turn fields:

```json
{
  "threadId": "<exact-thread-uuid>",
  "input": [],
  "toolOutput": {
    "name": "comms_receive",
    "output": "<JSON peer provenance and message body>"
  }
}
```

The adapter never changes the model, permissions, working directory, or system
instructions. It never responds to approval requests or tool calls on behalf of
the terminal's user.

| Adapter result | Node action |
|---|---|
| `accepted`, with a valid turn ID | Record `handed_off`; stop automatic delivery |
| `not_sent` | Retry only if the underlying cause is temporary |
| `uncertain` | Retain the uncertain outcome; never blindly retry |

Acceptance is a process-level acknowledgment from app-server. It does not mean
the model understood, completed, or agreed to the peer's request. On a busy
thread, the returned turn ID can be the already-active turn ID.

A disconnect after the delivery write begins can hide successful submission,
so it is uncertain. Invalid request/method/parameters and explicit ingress
overload are classified as pre-admission rejection. Unknown/internal RPC
failures remain uncertain. The adapter makes no retries itself.

Each operation has a ten-second deadline. Incoming JSON-RPC frames are bounded
to 1 MiB; peer payload JSON to 512 KiB; serialized requests to 768 KiB. These
bounds allow the agreed 64 KiB text body after JSON escaping. WebSocket
compression and HTTP proxies are disabled; all socket traffic remains local.

## Verified behavior

Live tests on macOS, Codex CLI **0.154.0**, 2026-09-15–16:

| Test | Observed result |
|---|---|
| Idle delivery | Fresh assistant reply to a unique `comms_receive` marker |
| Busy delivery | Held a controlled dynamic tool; another client submitted peer output; app-server returned the same active turn; model acknowledged the marker after release |
| Exact targeting | A separate control thread contained neither delivered marker |
| Persisted provenance | Markers stored as `function_call_output` / `functionCallOutput`; no corresponding user messages |
| Remote TUI synchronization | An already-open remote terminal showed Working and the newly generated marker reply, without terminal input causing that turn |
| Compiled Go adapter | `TestLiveDeliver` verified API acceptance, persisted tool output, and a fresh matching assistant reply |
| Launcher environment | Native tool shell reported the correct terminal PID and target, its exact thread ID, and a different shared app-server parent PID |
| Resume | Fresh tool output reported the new terminal PID with the same thread UUID and target |
| Actual node queue | HTTP session registration and send traversed SQLite, scheduler, native adapter, model, and durable `handed_off` with one attempt |
| Node restart | The same stored agent and attachment resumed native delivery automatically after restarting the node; a second marker produced a fresh reply |
| Compiled launcher | A new real TUI launched through `comms codex`, ran `$COMMS_BIN open` itself, and received a CLI-posted marker as native peer output |
| Compiled launcher exit/resume | Closing that TUI retired its ephemeral comms identity while app-server survived; `comms codex resume` reused the server and saved Codex thread, refreshed the TUI PID, created a fresh default comms identity, and received another native marker |
| Launcher working directory | A fresh TUI and an explicit resume from another directory each ran `pwd` in the invoking directory while reusing one app-server; shell commands required no model generation |

The adapter has protocol tests for private socket validation, inactive/mismatched
threads, active and idle handoff, malformed replies, overload, lost replies,
deadlines before/after submission, input size limits, and absence of retries.
`go test -race ./internal/codex` and `go vet ./internal/codex` pass. Its tests
cross-compile for Linux arm64.

To repeat the live Go probe, use a **new, owned test thread** with standing
permission to acknowledge harmless comms markers:

```sh
COMMS_CODEX_TEST_TARGET=unix:///absolute/test/control.sock \
COMMS_CODEX_TEST_THREAD=<owned-test-thread-uuid> \
go test -v ./internal/codex -run '^TestLiveDeliver$' -count=1
```

Do not point live probes at unrelated user sessions. Routine unit tests skip
live model calls unless both variables are supplied.

The equivalent full-node live integration test additionally restarts its
isolated node and verifies delivery with the same identity and attachment:

```sh
COMMS_CODEX_TEST_TARGET=unix:///absolute/test/control.sock \
COMMS_CODEX_TEST_THREAD=<owned-test-thread-uuid> \
COMMS_CODEX_TEST_PID=<owning-terminal-pid> \
go test -v ./internal/comms -run '^TestNodeNativeCodexLive$' -count=1
```

The app-server transport is experimental in the upstream documentation. The
verified compatibility baseline is Codex 0.154.0; repeat the native acceptance
tests when upgrading Codex. Unsupported runtimes fail visibly, without a
fallback to user-message injection.
