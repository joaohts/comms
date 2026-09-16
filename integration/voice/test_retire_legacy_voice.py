"""Safe source/behavior tests: no daemon imports, microphone, speaker, model or real network."""

import ast
import asyncio
import contextlib
import io
import json
from pathlib import Path
import re
import signal
import sys
import tempfile
from types import SimpleNamespace
import unittest

import retire_legacy_voice as migration


SOURCE = '''"""Voice daemon.
  LIVE:    full-duplex conversation (24 kHz), delegations go to Claude
           over the comms board (tagged replies), fallback claude -p.
"""
import asyncio
from spike import (
    Speaker, ask_brain, load_key, sh,
)
COMMS_ALIAS = "voice"
WAKE_KEY = "ei_joana"
SILENCE_TIMEOUT = 10.0
# Remote trigger state: a comms message can fire a session without the wake
# word ("WAKE") and optionally hand her an opening line ("ANNOUNCE: <text>").
remote = {"fire": False, "announce": None}

def wait_for_wake():
    return -1.0, None

def chime():
    raise AssertionError("real chime must never execute in tests")

async def live_session(key, stop_daemon, announce=None, caller=None):
    answer = await ask_brain([], identity=caller)
    return answer

async def idle_inbox_poller():
    """Temporary comms inbox poller."""
    from spike import parse_inbox_messages
    while True:
        out = await sh("comms", "inbox", COMMS_ALIAS)
        for mid, sender, body in parse_inbox_messages(out):
            await sh("comms", "post", "--from", COMMS_ALIAS, "--to", sender, "NACK")

TRIGGER_PORT = 3403

async def trigger_server():
    """Localhost HTTP trigger, replacing the comms WAKE/ANNOUNCE path:
    POST /trigger {"announce": "..."}; no announce field = plain wake.
    Same-box callers only (the brain's deliver()); the comms poller stays
    during the transition and goes away once the brain switches over."""
    async def handle(reader, writer):
        data = await asyncio.wait_for(reader.read(65536), timeout=5)
        head, _, body = data.partition(b"\\r\\n\\r\\n")
        request_line = head.split(b"\\r\\n", 1)[0].decode(errors="replace")
        if request_line.startswith("POST /trigger"):
            payload = json.loads(body.decode() or "{}")
            remote["announce"] = (payload.get("announce") or "").strip() or None
            remote["fire"] = True
            writer.write(b"HTTP/1.1 200 OK\\r\\n\\r\\n")
        else:
            writer.write(b"HTTP/1.1 404 Not Found\\r\\n\\r\\n")
        await writer.drain()
        writer.close()
    server = await asyncio.start_server(handle, "127.0.0.1", TRIGGER_PORT)
    async with server:
        await server.serve_forever()

async def main():
    key = load_key()
    stop_daemon = asyncio.Event()
    loop = asyncio.get_running_loop()
    loop.add_signal_handler(signal.SIGINT, stop_daemon.set)

    await sh("comms", "open", COMMS_ALIAS, "--note",
             "legacy voice daemon")
    await sh("comms", "inbox", COMMS_ALIAS)
    trigger = asyncio.create_task(trigger_server())
    while not stop_daemon.is_set():
        poller = asyncio.create_task(idle_inbox_poller())
        try:
            score, utterance = await asyncio.to_thread(wait_for_wake)
        finally:
            poller.cancel()
        if stop_daemon.is_set():
            break
        announce = remote["announce"]
        speaker = None
        if score >= 0:
            speaker = await asyncio.to_thread(identify_speaker, utterance)
        remote["fire"] = False
        remote["announce"] = None
        chime()
        await live_session(key, stop_daemon, announce=announce, caller=speaker)
    await sh("comms", "close", COMMS_ALIAS)
    print("stopped")
'''


def extract_function(source, name, namespace):
    node = migration.function(ast.parse(source), name)
    isolated = ast.Module(body=[node], type_ignores=[])
    exec(compile(isolated, "isolated_function", "exec"), namespace)
    return namespace[name]


class SourceTests(unittest.TestCase):
    def test_only_known_transition_edges_removed(self):
        updated, report = migration.prepare(SOURCE)
        tree = ast.parse(updated)
        self.assertFalse(any(migration.is_legacy_call(n) for n in ast.walk(tree)))
        self.assertNotIn("COMMS_ALIAS", updated)
        self.assertNotIn("idle_inbox_poller", updated)
        self.assertNotIn("comms board", updated)
        self.assertNotIn("poller stays", updated)
        self.assertEqual(report["removed_main_commands"], ["open", "inbox", "close"])
        self.assertTrue(report["http_handler_unchanged"])
        self.assertIn("live_session", report["unchanged_functions"])
        self.assertIn("ask_brain", updated)
        self.assertIn("SILENCE_TIMEOUT = 10.0", updated)
        self.assertIn('"127.0.0.1", TRIGGER_PORT', updated)
        self.assertEqual(migration.prepare(updated)[0], updated)

    def test_source_drift_is_rejected(self):
        changes = (
            SOURCE.replace('COMMS_ALIAS = "voice"', 'COMMS_ALIAS = "another-agent"'),
            SOURCE.replace('            poller.cancel()', '            cleanup_audio()'),
            SOURCE.replace('    await sh("comms", "close", COMMS_ALIAS)',
                           '    await sh("comms", "who", COMMS_ALIAS)'),
            SOURCE + '\nasync def extra():\n    await sh("comms", "post", "other")\n',
        )
        for changed in changes:
            with self.subTest(changed=changed[-90:]):
                with self.assertRaises(ValueError):
                    migration.prepare(changed)

    def test_other_shell_helper_use_is_preserved(self):
        source = SOURCE + '\nasync def unrelated():\n    return await sh("true")\n'
        updated, report = migration.prepare(source)
        self.assertIn("load_key, sh,", updated)
        self.assertIn("unrelated", report["unchanged_functions"])

    def test_private_backup_stage_and_guarded_rollback(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            daemon = root / "daemon.py"
            daemon.write_text(SOURCE)
            daemon.chmod(0o755)
            staged = root / "staged/daemon.py"
            preview = migration.migrate(daemon, stage=staged)
            self.assertFalse(preview["source_changed"])
            self.assertEqual(daemon.read_text(), SOURCE)
            self.assertEqual(staged.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(ValueError):
                migration.migrate(daemon, expected="incorrect")
            result = migration.migrate(daemon, backup_root=root / "backups",
                                       expected=migration.sha256(SOURCE.encode()))
            backup = Path(result["backup"])
            self.assertEqual({p.name for p in backup.iterdir()}, {"daemon.py", "manifest.json"})
            self.assertEqual(backup.stat().st_mode & 0o777, 0o700)
            self.assertEqual((backup / "daemon.py").stat().st_mode & 0o777, 0o600)
            self.assertEqual((backup / "manifest.json").stat().st_mode & 0o777, 0o600)
            self.assertEqual(daemon.stat().st_mode & 0o777, 0o755)
            self.assertFalse(result["service_restarted"])
            self.assertFalse(migration.migrate(daemon)["changed"])
            installed = daemon.read_text()
            daemon.write_text(installed + "\n# later edit\n")
            with self.assertRaises(ValueError):
                migration.rollback(backup)
            daemon.write_text(installed)
            migration.rollback(backup)
            self.assertEqual(daemon.read_text(), SOURCE)
            self.assertEqual(daemon.stat().st_mode & 0o777, 0o755)


class SafeBehaviorTests(unittest.IsolatedAsyncioTestCase):
    async def test_main_keeps_wake_session_and_trigger_without_legacy_binary(self):
        updated, _ = migration.prepare(SOURCE)
        called = []
        async def trigger():
            called.append("http-task")
        async def to_thread(fn, *args):
            called.append("wake")
            return fn(*args)
        async def live(key, stop, announce=None, caller=None):
            called.append(("session", key, announce, caller))
            stop.set()
        fake_loop = SimpleNamespace(add_signal_handler=lambda *args: called.append("signal"))
        namespace = {"asyncio": SimpleNamespace(Event=asyncio.Event,
                     get_running_loop=lambda: fake_loop, create_task=asyncio.create_task,
                     to_thread=to_thread), "signal": signal,
                     "load_key": lambda: "test-placeholder", "trigger_server": trigger,
                     "wait_for_wake": lambda: (-1, None),
                     "remote": {"fire": True, "announce": "in-memory-only"},
                     "chime": lambda: called.append("fake-chime"), "live_session": live}
        # No sh function is provided: a remaining comms edge would fail here.
        main = extract_function(updated, "main", namespace)
        with contextlib.redirect_stdout(io.StringIO()):
            await main()
            await asyncio.sleep(0)
        self.assertIn("http-task", called)
        self.assertIn(("session", "test-placeholder", "in-memory-only", None), called)
        self.assertEqual(called.count("fake-chime"), 1)

    async def test_http_trigger_uses_memory_streams_only(self):
        updated, _ = migration.prepare(SOURCE)
        registered = {}
        class Server:
            async def __aenter__(self): return self
            async def __aexit__(self, *args): pass
            async def serve_forever(self): pass
        async def fake_start(handler, host, port):
            registered.update(handler=handler, host=host, port=port)
            return Server()
        remote = {"fire": False, "announce": None}
        namespace = {"asyncio": SimpleNamespace(start_server=fake_start, wait_for=asyncio.wait_for),
                     "json": json, "remote": remote, "TRIGGER_PORT": 3403, "re": re, "sys": sys}
        await extract_function(updated, "trigger_server", namespace)()
        self.assertEqual((registered["host"], registered["port"]), ("127.0.0.1", 3403))
        class Reader:
            def __init__(self, data): self.data = data
            async def read(self, size): data, self.data = self.data, b""; return data
        class Writer:
            def __init__(self): self.data = b""; self.closed = False
            def write(self, data): self.data += data
            async def drain(self): pass
            def close(self): self.closed = True
        for payload, expected in (({}, None), ({"announce": " memory only "}, "memory only")):
            remote.update(fire=False, announce=None)
            body = json.dumps(payload).encode()
            reader = Reader(b"POST /trigger HTTP/1.1\r\nContent-Length: " + str(len(body)).encode()
                            + b"\r\n\r\n" + body)
            writer = Writer()
            await registered["handler"](reader, writer)
            self.assertEqual(remote, {"fire": True, "announce": expected})
            self.assertIn(b"200 OK", writer.data)
            self.assertTrue(writer.closed)


if __name__ == "__main__":
    unittest.main()
