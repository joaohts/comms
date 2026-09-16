#!/usr/bin/env python3
"""Retire only the voice daemon's legacy comms edges. Never imports the daemon or restarts it."""

import argparse
import ast
import copy
import datetime
import difflib
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
import uuid


LIVE_OLD = ("  LIVE:    full-duplex conversation (24 kHz), delegations go to Claude\n"
            "           over the comms board (tagged replies), fallback claude -p.")
LIVE_NEW = ("  LIVE:    full-duplex conversation (24 kHz); delegations go to the local\n"
            "           brain over localhost HTTP.")
REMOTE_OLD = ("# Remote trigger state: a comms message can fire a session without the wake\n"
              '# word ("WAKE") and optionally hand her an opening line ("ANNOUNCE: <text>").')
REMOTE_NEW = ("# Local HTTP trigger state: the brain can open a session without the wake\n"
              "# word and optionally provide an opening announcement.")
TRIGGER_DOC = ('    """Localhost HTTP trigger for same-machine callers (the brain\'s deliver()).\n'
               '    POST /trigger {"announce": "..."}; no announce field = plain wake.\n'
               '    """\n')


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def function(tree, name):
    found = [n for n in tree.body if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
             and n.name == name]
    if len(found) != 1:
        raise ValueError(f"expected one {name} definition; source needs manual review")
    return found[0]


def is_legacy_call(node):
    return (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
            and node.func.id == "sh" and len(node.args) >= 2
            and isinstance(node.args[0], ast.Constant) and node.args[0].value == "comms")


def without_doc(node):
    node = copy.deepcopy(node)
    if node.body and isinstance(node.body[0], ast.Expr) and isinstance(node.body[0].value, ast.Constant):
        if isinstance(node.body[0].value.value, str):
            node.body.pop(0)
    return ast.dump(node, include_attributes=False)


def apply_edits(source, edits):
    lines = source.splitlines(keepends=True)
    previous = len(lines) + 1
    for start, finish, replacement in sorted(edits, reverse=True):
        if finish >= previous:
            raise ValueError("overlapping source edits")
        lines[start - 1:finish] = replacement.splitlines(keepends=True)
        previous = start
    return "".join(lines)


def prepare(source):
    tree = ast.parse(source)
    original_main = function(tree, "main")
    original_trigger = function(tree, "trigger_server")
    for required in ("wait_for_wake", "live_session"):
        function(tree, required)
    pollers = [n for n in tree.body if isinstance(n, ast.AsyncFunctionDef)
               and n.name == "idle_inbox_poller"]
    aliases = [n for n in tree.body if isinstance(n, ast.Assign)
               and any(isinstance(t, ast.Name) and t.id == "COMMS_ALIAS" for t in n.targets)]
    calls = [n for n in ast.walk(tree) if is_legacy_call(n)]
    edits = []
    expected_main = copy.deepcopy(original_main)

    if pollers or aliases or calls:
        if len(pollers) != 1 or len(aliases) != 1:
            raise ValueError("partial or unknown legacy migration; no source changed")
        if not isinstance(aliases[0].value, ast.Constant) or aliases[0].value.value != "voice":
            raise ValueError("unexpected legacy alias; no source changed")
        commands = []
        removed_main = []
        for node in original_main.body:
            if isinstance(node, ast.Expr) and isinstance(node.value, ast.Await) and is_legacy_call(node.value.value):
                call = node.value.value
                if (not isinstance(call.args[1], ast.Constant) or len(call.args) < 3
                        or not isinstance(call.args[2], ast.Name) or call.args[2].id != "COMMS_ALIAS"):
                    raise ValueError("unexpected main comms call")
                commands.append(call.args[1].value)
                removed_main.append(node.lineno)
                edits.append((node.lineno, node.end_lineno, ""))
        if sorted(commands) != ["close", "inbox", "open"]:
            raise ValueError("expected only legacy open/inbox/close in main")
        allowed_calls = [n for n in ast.walk(pollers[0]) if is_legacy_call(n)]
        allowed_calls += [n.value.value for n in original_main.body if n.lineno in removed_main]
        if len(calls) != len(allowed_calls):
            raise ValueError("legacy comms call outside known transition code")
        edits.extend([(pollers[0].lineno, pollers[0].end_lineno, ""),
                      (aliases[0].lineno, aliases[0].end_lineno, "")])

        loops = [n for n in original_main.body if isinstance(n, ast.While)]
        if len(loops) != 1 or len(loops[0].body) < 2:
            raise ValueError("unexpected main wake loop")
        loop = loops[0]
        spawn, waiting = loop.body[:2]
        if (not isinstance(spawn, ast.Assign) or len(spawn.targets) != 1
                or not isinstance(spawn.targets[0], ast.Name) or spawn.targets[0].id != "poller"
                or not isinstance(spawn.value, ast.Call)
                or not isinstance(spawn.value.func, ast.Attribute) or spawn.value.func.attr != "create_task"
                or not isinstance(spawn.value.func.value, ast.Name) or spawn.value.func.value.id != "asyncio"
                or len(spawn.value.args) != 1 or not isinstance(spawn.value.args[0], ast.Call)
                or not isinstance(spawn.value.args[0].func, ast.Name)
                or spawn.value.args[0].func.id != "idle_inbox_poller"):
            raise ValueError("unexpected poller startup")
        if (not isinstance(waiting, ast.Try) or len(waiting.body) != 1 or waiting.handlers
                or waiting.orelse or len(waiting.finalbody) != 1):
            raise ValueError("unexpected wake/poller try-finally")
        final = waiting.finalbody[0]
        if (not isinstance(final, ast.Expr) or not isinstance(final.value, ast.Call)
                or not isinstance(final.value.func, ast.Attribute) or final.value.func.attr != "cancel"
                or not isinstance(final.value.func.value, ast.Name) or final.value.func.value.id != "poller"):
            raise ValueError("unexpected poller cleanup")
        body = waiting.body[0]
        wait_calls = [n for n in ast.walk(body) if isinstance(n, ast.Name) and n.id == "wait_for_wake"]
        if len(wait_calls) != 1:
            raise ValueError("wake operation changed; refusing to infer a replacement")
        lines = source.splitlines(keepends=True)
        difference = body.col_offset - waiting.col_offset
        replacement = "".join(line[difference:] for line in lines[body.lineno - 1:body.end_lineno])
        edits.append((spawn.lineno, spawn.end_lineno, ""))
        edits.append((waiting.lineno, waiting.end_lineno, replacement))

        expected_main.body = [n for n in expected_main.body if n.lineno not in removed_main]
        expected_loop = next(n for n in expected_main.body if isinstance(n, ast.While))
        expected_loop.body = copy.deepcopy(waiting.body) + expected_loop.body[2:]
    else:
        commands = []

    doc = original_trigger.body[0]
    if not (isinstance(doc, ast.Expr) and isinstance(doc.value, ast.Constant)
            and isinstance(doc.value.value, str)):
        raise ValueError("trigger-server documentation missing")
    edits.append((doc.lineno, doc.end_lineno, TRIGGER_DOC))
    updated = apply_edits(source, edits).replace(LIVE_OLD, LIVE_NEW).replace(REMOTE_OLD, REMOTE_NEW)

    interim = ast.parse(updated)
    if not any(isinstance(n, ast.Name) and n.id == "sh" for n in ast.walk(interim)):
        imports = [n for n in interim.body if isinstance(n, ast.ImportFrom) and n.module == "spike"
                   and any(a.name == "sh" for a in n.names)]
        if len(imports) > 1:
            raise ValueError("ambiguous spike helper import")
        if imports:
            node = imports[0]
            names = [a.name + (" as " + a.asname if a.asname else "")
                     for a in node.names if a.name != "sh"]
            updated = apply_edits(updated, [(node.lineno, node.end_lineno,
                                            "from spike import " + ", ".join(names) + "\n")])

    final_tree = ast.parse(updated)
    compile(updated, "daemon.py", "exec")
    if any(is_legacy_call(n) or isinstance(n, ast.Name) and n.id in ("COMMS_ALIAS", "idle_inbox_poller")
           for n in ast.walk(final_tree)):
        raise ValueError("legacy voice transport reference remains")
    protected = []
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name not in (
                "main", "trigger_server", "idle_inbox_poller"):
            if ast.dump(node, include_attributes=False) != ast.dump(function(final_tree, node.name), include_attributes=False):
                raise ValueError(f"unrelated function changed: {node.name}")
            protected.append(node.name)
    if without_doc(original_trigger) != without_doc(function(final_tree, "trigger_server")):
        raise ValueError("HTTP trigger behavior changed")
    if ast.dump(expected_main, include_attributes=False) != ast.dump(function(final_tree, "main"), include_attributes=False):
        raise ValueError("main changed outside the known legacy transport edges")
    return updated, {"removed_main_commands": commands, "removed_poller": bool(pollers),
                     "unchanged_functions": protected, "http_handler_unchanged": True}


def atomic_write(path, data, mode):
    fd, temporary = tempfile.mkstemp(prefix=".voice-migration-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def migrate(daemon, backup_root=None, expected=None, dry_run=False, stage=None):
    daemon = Path(daemon).expanduser().resolve()
    original = daemon.read_bytes()
    before = sha256(original)
    if expected and before != expected:
        raise ValueError("daemon source changed since preflight; no files changed")
    text, checks = prepare(original.decode())
    patched = text.encode()
    report = {"daemon": str(daemon), "before_sha256": before, "after_sha256": sha256(patched),
              "changed": patched != original, "service_restarted": False, **checks}
    if dry_run:
        print("".join(difflib.unified_diff(original.decode().splitlines(True), text.splitlines(True),
                                        fromfile=str(daemon), tofile=str(daemon))))
        return report
    if stage:
        staged = Path(stage).expanduser().resolve()
        if staged == daemon:
            raise ValueError("stage output must not overwrite the running source")
        staged.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        atomic_write(staged, patched, 0o600)
        return {**report, "staged": str(staged), "source_changed": False}
    if patched == original:
        return report
    backup_root = Path(backup_root or Path.home() / ".local/state/comms/voice-backups").expanduser()
    backup = backup_root / (datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
                            + "-" + uuid.uuid4().hex[:8])
    backup.mkdir(parents=True, mode=0o700)
    os.chmod(backup, 0o700)
    mode = stat.S_IMODE(daemon.stat().st_mode)
    atomic_write(backup / "daemon.py", original, 0o600)
    manifest = {"daemon": str(daemon), "before_sha256": before,
                "after_sha256": report["after_sha256"], "source_mode": mode}
    atomic_write(backup / "manifest.json", json.dumps(manifest, indent=2).encode(), 0o600)
    # Recheck after backup preparation instead of overwriting a concurrent edit.
    if sha256(daemon.read_bytes()) != before:
        raise ValueError("daemon changed while preparing backup; source not replaced")
    atomic_write(daemon, patched, mode)
    return {**report, "backup": str(backup), "source_changed": True}


def rollback(backup):
    backup = Path(backup).expanduser().resolve()
    manifest = json.loads((backup / "manifest.json").read_text())
    daemon = Path(manifest["daemon"])
    if sha256(daemon.read_bytes()) != manifest["after_sha256"]:
        raise ValueError("daemon changed after migration; refusing to overwrite later edits")
    original = (backup / "daemon.py").read_bytes()
    if sha256(original) != manifest["before_sha256"]:
        raise ValueError("source backup checksum mismatch")
    atomic_write(daemon, original, manifest["source_mode"])
    return {"restored": str(daemon), "service_restarted": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--daemon", default=str(Path.home() / "voice-assistant/daemon.py"))
    parser.add_argument("--backup-dir")
    parser.add_argument("--expected-sha256")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--stage")
    parser.add_argument("--rollback")
    args = parser.parse_args()
    if args.rollback:
        result = rollback(args.rollback)
    else:
        result = migrate(args.daemon, args.backup_dir, args.expected_sha256, args.dry_run, args.stage)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
