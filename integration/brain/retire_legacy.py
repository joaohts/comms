#!/usr/bin/env python3
"""Retire brain's legacy board paths after enabling its comms-v1 adapter.

No service restarts, credential edits, or database changes. Source backups and
rollback use patch_brain.py; subsequent ordinary adapter updates preserve this
explicit retirement choice.
"""

import argparse
import json
from pathlib import Path

import patch_brain


OLD_VOICE_FALLBACK = '''        except Exception:
            # daemon HTTP down — legacy comms path as fallback only
            subprocess.run(["comms", "post", "--from", "brain",
                            "--to", "pi:voice", f"ANNOUNCE: {message}"],
                           capture_output=True, timeout=15)
            status = "delivered to voice (announce, comms fallback)"
'''
NEW_VOICE_FAILURE = '''        except Exception as exc:
            return f"voice unavailable: {type(exc).__name__}"
'''
SPAWN_BEGIN = "    # BEGIN managed comms-v1 spawn submission\n"
SPAWN_END = "    # END managed comms-v1 spawn submission\n"
SPAWN_SUBMISSION = SPAWN_BEGIN + '''    # Wait for the exact new local agent's harness-owned receiver. Resolve its
    # alias once, then submit to immutable IDs with an idempotent task ID.
    for _ in range(20):
        time.sleep(3)
        who = subprocess.run(["comms", "who", "--compact"],
                             capture_output=True, text=True, timeout=15)
        if who.returncode:
            continue
        try:
            peers = _json.loads(who.stdout)
        except ValueError:
            continue
        peer = next((p for p in peers if p.get("address") == f"session-{sid}"
                     and p.get("online")), None)
        if peer is None:
            continue
        target = peer["recipient"]
        posted = subprocess.run(["comms", "post", "--from", "brain",
                                 "--to", target, "--id", f"brain_spawn_{sid}",
                                 "--stdin", "--compact"], input=task,
                                capture_output=True, text=True, timeout=15)
        if posted.returncode:
            return (f"session {sid} joined, but task submission failed or is "
                    f"uncertain: {posted.stderr.strip()}. Check message "
                    f"brain_spawn_{sid} before retrying.")
        break
    else:
        return (f"session {sid} created but its comms receiver is not online; "
                f"inspect it with: tmux attach -t {sid}")
''' + SPAWN_END


def once(source, old, new):
    if source.count(old) != 1:
        raise ValueError("legacy source changed; inspect the retirement diff before applying")
    return source.replace(old, new, 1)


def retire_tools(source):
    if patch_brain.RETIRED_MARKER in source:
        return source
    source = once(source, OLD_VOICE_FALLBACK, NEW_VOICE_FAILURE)
    source = once(source, patch_brain.TOOLS_HOOK, patch_brain.RETIRED_TOOLS_HOOK)
    # The old channel name remains accepted by the managed adapter hook.
    start = source.index(patch_brain.TOOLS_ANCHOR, source.index(patch_brain.TOOLS_END))
    end = source.index('    elif channel == "cli":\n', start)
    source = source[:start] + source[end:]
    start = source.index('    # deliver the task over comms (doorbell-reliable')
    end = source.index('    db.step(env["turn_id"], "session_spawned"', start)
    source = source[:start] + SPAWN_SUBMISSION + source[end:]
    source = once(source, 'f"comms:pi:session-{sid}; message it with send_to on that "',
                  'f"comms-v1:{target}; message it with send_to on that "')
    source = once(source, 'r = subprocess.run(["comms", "who"], capture_output=True, text=True,',
                  'r = subprocess.run(["comms", "who", "--compact"], capture_output=True, text=True,')
    source = source.replace('reports arrive as normal turns on channel comms:pi:session-<id>.',
                            'reports arrive on the immutable comms-v1:<machine>:<agent> channel.')
    source = source.replace('"note, armed state). Message any of them directly with "',
                            '"online state, exact recipient). Message any of them with "')
    return source + "\n" + patch_brain.RETIRED_MARKER + "\n"


def prepare(root):
    root = Path(root).resolve()
    updates = patch_brain.prepare(root)
    server = updates["brain/server.py"].decode()
    if patch_brain.RETIRED_MARKER not in server:
        server = once(server, patch_brain.SERVER_ANCHOR,
                      "    " + patch_brain.RETIRED_MARKER + "\n")
    updates["brain/server.py"] = server.encode()
    updates["brain/tools.py"] = retire_tools(updates["brain/tools.py"].decode()).encode()
    for relative, content in updates.items():
        compile(content, relative, "exec")
    return updates


def install(root, backup_root=None, dry_run=False):
    return patch_brain.apply_updates(root, prepare(root), backup_root, dry_run)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(Path.home() / "voice-assistant"))
    parser.add_argument("--backup-dir")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--rollback", metavar="BACKUP_DIRECTORY")
    args = parser.parse_args()
    result = (patch_brain.rollback(args.rollback) if args.rollback else
              install(args.root, args.backup_dir, args.dry_run))
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
