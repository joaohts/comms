#!/usr/bin/env python3
"""Install an idempotent Claude shortcut, preserving custom shell definitions."""
import os
from pathlib import Path
import re
import shlex
import shutil
import sys
import tempfile

command = str(Path(sys.argv[1]).absolute())
shell = Path(os.environ.get("SHELL", "")).name
user_home = Path.home()
if shell == "zsh":
    rc = Path(os.environ.get("ZDOTDIR") or user_home) / ".zshrc"
elif shell == "bash":
    rc = user_home / ".bashrc"
else:
    print("Claude shortcut skipped: unsupported shell. Launch with comms claude.")
    sys.exit(0)
rc = rc.resolve()
start, end = "# BEGIN COMMS CLAUDE ALIAS", "# END COMMS CLAUDE ALIAS"
old = rc.read_text() if rc.exists() else ""
pattern = re.compile(r"^" + re.escape(start) + r"\n.*?^" + re.escape(end) + r"(?:\n|$)", re.M | re.S)
matches = list(pattern.finditer(old))
if old.count(start) != len(matches) or old.count(end) != len(matches) or len(matches) > 1:
    raise SystemExit("Malformed comms Claude alias block in " + str(rc) + "; repair it or use --skip-claude-alias.")
unmanaged = pattern.sub("", old)
if re.search(r"^\s*(?:alias\s+claude=|(?:function\s+)?claude\s*\(\s*\)|function\s+claude\b)", unmanaged, re.M):
    print("Preserved existing Claude alias/function in " + str(rc) + ". Use comms claude for the node's receiver mode.")
    sys.exit(0)
block = start + "\n# Use the node's Claude receiver setting; bypass with: command claude <args>\n"
block += "alias claude=" + shlex.quote(shlex.quote(command) + " claude") + "\n" + end + "\n"
new = pattern.sub(lambda _: block, old) if matches else old + ("" if not old or old.endswith("\n") else "\n") + block
if new != old:
    rc.parent.mkdir(parents=True, exist_ok=True)
    if rc.exists():
        fd, backup = tempfile.mkstemp(prefix=rc.name + ".comms-backup-", dir=rc.parent)
        os.close(fd)
        shutil.copy2(rc, backup)
        print("Shell config backup: " + backup)
    rc.write_text(new)
print("Claude shortcut installed in " + str(rc) + ". Open a new shell or source this file, then run claude.")
if shell == "bash":
    print("Bash login shells must source ~/.bashrc to load the shortcut.")
