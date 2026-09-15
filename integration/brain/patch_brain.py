#!/usr/bin/env python3
"""Install/revert the two optional brain hooks. Never changes secrets or restarts services."""

import argparse
import datetime
import difflib
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import tempfile
import uuid


SERVER_BEGIN = "    # BEGIN managed comms-v1 ingress\n"
SERVER_END = "    # END managed comms-v1 ingress\n"
TOOLS_BEGIN = "    # BEGIN managed comms-v1 delivery\n"
TOOLS_END = "    # END managed comms-v1 delivery\n"
SERVER_ANCHOR = "    threading.Thread(target=comms_ingress, daemon=True).start()\n"
TOOLS_ANCHOR = '    elif channel.startswith("comms:"):\n'
SERVER_HOOK = SERVER_BEGIN + '''    try:
        from .comms_v1 import start_if_enabled
        start_if_enabled(run_turn, cfg, db)
    except Exception as exc:
        print(f"[comms-v1] optional ingress unavailable: {type(exc).__name__}", flush=True)
''' + SERVER_END
TOOLS_HOOK = TOOLS_BEGIN + '''    elif channel.startswith("comms-v1:"):
        from .comms_v1 import deliver as deliver_v1
        target = channel.split(":", 1)[1]
        stamped = (f"{message}\\n[{origin}; replies return through brain to that channel]"
                   if origin.startswith("relayed from ") else message)
        result = deliver_v1(target, stamped)
        if not result["ok"]:
            return result["error"]
        status = f"queued on comms-v1 to {target} ({result['id']})"
''' + TOOLS_END
TARGETS = ("brain/server.py", "brain/tools.py", "brain/comms_v1.py")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def replace_hook(source, begin, end, hook, anchor, after):
    if begin in source or end in source:
        if source.count(begin) != 1 or source.count(end) != 1:
            raise ValueError("ambiguous existing managed hook")
        start = source.index(begin)
        finish = source.index(end, start) + len(end)
        return source[:start] + hook + source[finish:]
    if source.count(anchor) != 1:
        raise ValueError("expected integration anchor is missing or ambiguous; no files changed")
    return source.replace(anchor, anchor + hook if after else hook + anchor, 1)


def prepare(root, module_path=None):
    root = Path(root).resolve()
    module_path = Path(module_path or Path(__file__).with_name("comms_v1.py"))
    server = (root / "brain/server.py").read_text()
    tools = (root / "brain/tools.py").read_text()
    updates = {
        "brain/server.py": replace_hook(server, SERVER_BEGIN, SERVER_END, SERVER_HOOK,
                                        SERVER_ANCHOR, True).encode(),
        "brain/tools.py": replace_hook(tools, TOOLS_BEGIN, TOOLS_END, TOOLS_HOOK,
                                       TOOLS_ANCHOR, False).encode(),
        "brain/comms_v1.py": module_path.read_bytes(),
    }
    # Validate every resulting file before the first mutation.
    for relative, content in updates.items():
        compile(content, relative, "exec")
    return updates


def atomic_write(path, data, mode=0o644):
    fd, temporary = tempfile.mkstemp(prefix=".comms-v1-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as file:
            file.write(data)
            file.flush()
            os.fsync(file.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def install(root, backup_root=None, dry_run=False):
    root = Path(root).resolve()
    updates = prepare(root)
    changed = {relative: data for relative, data in updates.items()
               if not (root / relative).exists() or (root / relative).read_bytes() != data}
    if dry_run:
        for relative, data in changed.items():
            old = (root / relative).read_text() if (root / relative).exists() else ""
            print("".join(difflib.unified_diff(old.splitlines(True), data.decode().splitlines(True),
                                            fromfile=relative, tofile=relative)))
        return {"changed": sorted(changed), "dry_run": True}
    if not changed:
        return {"changed": [], "already_installed": True}
    backup_root = Path(backup_root or root / ".comms-v1-backups").resolve()
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    backup = backup_root / (stamp + "-" + uuid.uuid4().hex[:8])
    backup.mkdir(parents=True, mode=0o700)
    manifest = {"root": str(root), "files": {}}
    for relative, data in changed.items():
        path = root / relative
        saved = backup / relative
        original = path.read_bytes() if path.exists() else None
        mode = stat.S_IMODE(path.stat().st_mode) if path.exists() else 0o644
        if original is not None:
            saved.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, saved)
        manifest["files"][relative] = {"existed": original is not None,
                                       "before": digest(original) if original is not None else None,
                                       "after": digest(data), "mode": mode}
    atomic_write(backup / "manifest.json", json.dumps(manifest, indent=2).encode(), 0o600)
    for relative, data in changed.items():
        atomic_write(root / relative, data, manifest["files"][relative]["mode"])
    return {"changed": sorted(changed), "backup": str(backup),
            "service_restarted": False, "enabled": False}


def rollback(backup):
    backup = Path(backup).resolve()
    manifest = json.loads((backup / "manifest.json").read_text())
    root = Path(manifest["root"])
    for relative, record in manifest["files"].items():
        if relative not in TARGETS:
            raise ValueError("unexpected backup target")
        path = root / relative
        if not path.exists() or digest(path.read_bytes()) != record["after"]:
            raise ValueError(f"{relative} changed after installation; refusing to overwrite it")
        if record["existed"] and digest((backup / relative).read_bytes()) != record["before"]:
            raise ValueError("backup checksum mismatch")
    for relative, record in manifest["files"].items():
        path = root / relative
        if record["existed"]:
            atomic_write(path, (backup / relative).read_bytes(), record["mode"])
        else:
            path.unlink()
    return {"restored": sorted(manifest["files"]), "service_restarted": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(Path.home() / "voice-assistant"))
    parser.add_argument("--backup-dir")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--rollback", metavar="BACKUP_DIRECTORY")
    args = parser.parse_args()
    result = rollback(args.rollback) if args.rollback else install(args.root, args.backup_dir, args.dry_run)
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
