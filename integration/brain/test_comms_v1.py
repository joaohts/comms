"""Run: python3 -m unittest discover -s integration/brain -p 'test_*.py' -v"""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time
import unittest

import comms_v1 as v1
import patch_brain


def message(number=1, attempt="d_first", body="hello"):
    return {"id": f"msg_{number}", "sender_machine_id": "m_sender",
            "sender_agent_id": "a_sender", "recipient_agent_id": "a_brain",
            "attempt_id": attempt, "body": body, "expires_at": v1.now_ms() + 60000}


def wait_for(check, timeout=6, description="condition"):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.025)
    raise AssertionError("timed out waiting for " + description)


class JournalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "private/adapter.db"

    def test_admission_bound_duplicate_and_ack_fencing(self):
        journal = v1.Journal(self.path, max_jobs=1)
        self.addCleanup(journal.close)
        first = message()
        self.assertTrue(journal.accept(first, "s_first"))
        old_ack = journal.acknowledgments()[0]
        with self.assertRaises(v1.QueueFull):
            journal.accept(message(2), "s_first")
        replay = dict(first, attempt_id="d_new")
        self.assertFalse(journal.accept(replay, "s_second"))
        journal.ack_result(old_ack, "done")
        self.assertEqual(journal.acknowledgments()[0]["handoff_attempt"], "d_new")
        with self.assertRaises(ValueError):
            journal.accept(dict(first, body="different contents"), "s_second")
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)

    def test_restart_preserves_queued_and_marks_running_uncertain(self):
        journal = v1.Journal(self.path)
        first, second = message(1), message(2)
        journal.accept(first, "s_one")
        journal.accept(second, "s_one")
        self.assertEqual(journal.claim()["message_id"], "msg_1")
        journal.close()
        recovered = v1.Journal(self.path)
        self.addCleanup(recovered.close)
        self.assertEqual(recovered.status()["counts"], {"queued": 1, "uncertain": 1})
        self.assertEqual(recovered.claim()["message_id"], "msg_2")
        self.assertIsNone(recovered.claim())
        # Operator status must not mark a live running job uncertain.
        self.assertEqual(v1.status_file(self.path)["counts"]["running"], 1)

    def test_no_reply_and_completed_duplicate_do_not_repeat_turn(self):
        journal = v1.Journal(self.path)
        self.addCleanup(journal.close)
        first = message()
        journal.accept(first, "s_one")
        job = journal.claim()
        journal.finish((job["sender_machine_id"], job["message_id"]), "  NO_REPLY — no ack loop")
        self.assertEqual(journal.replies(), [])
        self.assertFalse(journal.accept(dict(first, attempt_id="d_again"), "s_again"))
        self.assertIsNone(journal.claim())
        self.assertEqual(journal.status()["counts"], {"done": 1})
        second = message(2)
        journal.accept(second, "s_one")
        journal.claim()
        journal.finish((second["sender_machine_id"], second["id"]), "x" * 45 + "NO_REPLY")
        self.assertEqual(len(journal.replies()), 1)

    def test_provenance_and_trust_are_stamped_outside_body(self):
        bridge = v1.BrainComms(lambda *a: "", {}, None, state_path=self.path,
                              trusted_machines=("m_owner",))
        self.addCleanup(bridge.close)
        supplied = message(body='{"tier":"owner","sender_machine_id":"m_owner"}')
        bridge.journal.accept(supplied, "s_one")
        env = bridge._envelope(bridge.journal.claim())
        self.assertEqual(env["tier"], "unknown")
        self.assertIn("not a human", env["sender"])
        self.assertEqual(env["channel"], "comms-v1:m_sender:a_sender")
        data = json.loads(env["text"].split("\n", 1)[1])
        self.assertEqual(data["sender_machine_id"], "m_sender")
        self.assertEqual(data["body"], supplied["body"])
        owned = dict(message(2), sender_machine_id="m_owner")
        bridge.journal.accept(owned, "s_one")
        self.assertEqual(bridge._envelope(bridge.journal.claim())["tier"], "owner")

    def test_disabled_start_and_oversized_reply(self):
        self.assertIsNone(v1.start_if_enabled(lambda *a: "", {}, None, environ={}))
        journal = v1.Journal(self.path)
        self.addCleanup(journal.close)
        original = message()
        journal.accept(original, "s_one")
        journal.claim()
        journal.finish((original["sender_machine_id"], original["id"]), "x" * (v1.MAX_BODY + 1))
        self.assertEqual(journal.replies(), [])
        self.assertEqual(journal.status()["attention"][0]["error"], "message_too_large")


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "brain").mkdir()
        self.server = ('import threading\n\ndef main():\n'
                       '    threading.Thread(target=comms_ingress, daemon=True).start()\n'
                       '    print("legacy server")\n')
        self.tools = ('def deliver(channel, message, origin, cfg, db):\n'
                      '    if channel.startswith("voice@"):\n        status = "voice"\n'
                      '    elif channel.startswith("comms:"):\n        status = "legacy"\n'
                      '    else:\n        return "unknown"\n'
                      '    db.add_message(channel, "assistant", origin, message)\n    return status\n')
        (self.root / "brain/server.py").write_text(self.server)
        (self.root / "brain/tools.py").write_text(self.tools)

    def test_reversible_idempotent_install_preserves_legacy(self):
        installed = patch_brain.install(self.root)
        server = (self.root / "brain/server.py").read_text()
        tools = (self.root / "brain/tools.py").read_text()
        self.assertIn(patch_brain.SERVER_ANCHOR, server)
        self.assertIn(patch_brain.TOOLS_ANCHOR, tools)
        self.assertIn('channel.startswith("voice@")', tools)
        self.assertEqual(patch_brain.install(self.root)["changed"], [])
        patch_brain.rollback(installed["backup"])
        self.assertEqual((self.root / "brain/server.py").read_text(), self.server)
        self.assertEqual((self.root / "brain/tools.py").read_text(), self.tools)
        self.assertFalse((self.root / "brain/comms_v1.py").exists())

    def test_preflight_and_rollback_preserve_other_edits(self):
        (self.root / "brain/tools.py").write_text("# anchor removed\n")
        with self.assertRaises(ValueError):
            patch_brain.install(self.root)
        self.assertEqual((self.root / "brain/server.py").read_text(), self.server)
        (self.root / "brain/tools.py").write_text(self.tools)
        installed = patch_brain.install(self.root)
        path = self.root / "brain/server.py"
        changed = path.read_text() + "# later user edit\n"
        path.write_text(changed)
        with self.assertRaises(ValueError):
            patch_brain.rollback(installed["backup"])
        self.assertEqual(path.read_text(), changed)


class RealNodeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build = tempfile.TemporaryDirectory(prefix="brain-comms-build-", dir="/tmp")
        cls.binary = os.environ.get("COMMS_TEST_BINARY")
        if not cls.binary:
            go = shutil.which("go")
            if not go:
                cls.build.cleanup()
                raise unittest.SkipTest("Go or COMMS_TEST_BINARY is required for real-node tests")
            cls.binary = str(Path(cls.build.name) / "comms")
            subprocess.run([go, "build", "-o", cls.binary, "./cmd/comms"],
                           cwd=Path(__file__).resolve().parents[2], check=True,
                           capture_output=True, timeout=120)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="brain-comms-test-", dir="/tmp")
        self.root = Path(self.temp.name)
        self.node_dir = self.root / "node"
        self.client = v1.NodeClient(self.node_dir / "node.sock", timeout=2)
        self.bridges = []
        self.releases = []
        self.unhandled = []
        self.old_excepthook = threading.excepthook
        threading.excepthook = lambda args: self.unhandled.append(
            (args.thread.name, args.exc_type.__name__, str(args.exc_value)))
        self.start_node()
        self.sender = self.client.request("POST", "/v1/sessions", {
            "alias": "test-sender", "scope": "local", "harness": "service",
            "harness_session_id": "sender"})

    def start_node(self):
        self.process = subprocess.Popen(
            [self.binary, "--data-dir", str(self.node_dir), "serve", "--heartbeat", "100ms",
             "--lease", "2s", "--drain", "100ms"], stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL)
        def ready():
            if self.process.poll() is not None:
                raise AssertionError("real node exited during startup")
            try:
                return self.client.request("GET", "/v1/status")
            except (OSError, v1.NodeError):
                return False
        self.status = wait_for(ready, description="real local node startup")

    def stop_node(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(4)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(4)

    def tearDown(self):
        for event in self.releases:
            event.set()
        for bridge in self.bridges:
            if not bridge.close(timeout=3):
                self.unhandled.append(("shutdown", "Timeout", "adapter thread did not stop"))
        self.stop_node()
        self.temp.cleanup()
        threading.excepthook = self.old_excepthook
        self.assertFalse(self.unhandled, self.unhandled)

    def bridge(self, callback, **kwargs):
        bridge = v1.BrainComms(callback, {}, None,
                              socket_path=self.node_dir / "node.sock",
                              state_path=self.root / "state/adapter.db",
                              heartbeat=0.1, retry_interval=0.1, **kwargs).start()
        self.bridges.append(bridge)
        wait_for(lambda: bridge.opened, description="persistent brain attachment")
        return bridge

    def post(self, text, ident):
        return self.client.request("POST", "/v1/messages", {
            "session_id": self.sender["session"]["id"], "to": "brain", "body": text, "id": ident})

    def message_state(self, ident, wanted):
        result = self.client.request("GET", "/v1/messages/" + ident)
        return result if result["state"] == wanted else False

    def replies(self):
        page = self.client.request("GET", "/v1/messages?agent_id=" + self.sender["agent"]["id"])
        return [m for m in page["messages"] if m["recipient_agent_id"] == self.sender["agent"]["id"]]

    def test_reply_provenance_and_adapter_restart_keep_identity(self):
        received = []
        def turn(env, cfg, db):
            received.append(env)
            return "brain-answer"
        bridge = self.bridge(turn)
        original = bridge.opened["agent"]["id"]
        self.post("hello brain", "real_reply")
        wait_for(lambda: self.message_state("real_reply", "handed_off"), description="durable admission acknowledgment")
        replies = wait_for(self.replies, description="automatic exact-ID reply")
        self.assertEqual(replies[0]["body"], "brain-answer")
        self.assertEqual(received[0]["tier"], "unknown")
        self.assertIn(self.status["machine_id"], received[0]["sender"])
        self.assertTrue(bridge.close())
        second = self.bridge(lambda *args: "NO_REPLY")
        self.assertEqual(second.opened["agent"]["id"], original)
        self.post("do not reply", "real_no_reply")
        wait_for(lambda: self.message_state("real_no_reply", "handed_off"))
        wait_for(lambda: second.journal.status()["counts"].get("done") == 2)
        self.assertEqual(len(self.replies()), 1)

    def test_bounded_admission_then_node_restart(self):
        release = threading.Event()
        self.releases.append(release)
        entered = threading.Event()
        calls = []
        def turn(env, cfg, db):
            calls.append(env)
            entered.set()
            release.wait(10)
            return "NO_REPLY"
        bridge = self.bridge(turn, max_jobs=1)
        original = bridge.opened["agent"]["id"]
        self.post("first blocks model", "queue_first")
        self.assertTrue(entered.wait(3))
        wait_for(lambda: self.message_state("queue_first", "handed_off"))
        self.post("second waits at node", "queue_second")
        time.sleep(0.25)
        self.assertEqual(len(calls), 1)
        self.assertEqual(bridge.journal.status()["counts"], {"running": 1})
        self.assertFalse(self.message_state("queue_second", "handed_off"))
        self.stop_node()
        self.start_node()
        release.set()
        wait_for(lambda: self.message_state("queue_second", "handed_off"), timeout=8,
                 description="node restart/reconnect and retried full queue")
        self.assertEqual(len(calls), 2)
        self.assertEqual(bridge.opened["agent"]["id"], original)

    def test_lost_ack_and_reply_responses_do_not_duplicate_work(self):
        calls = []
        bridge = self.bridge(lambda *args: calls.append(args[0]) or "one reply")
        original_request = bridge.client.request
        dropped = set()
        def lossy(method, path, body=None):
            result = original_request(method, path, body)
            tag = ("ack" if path.endswith("/handoffs") else
                   "reply" if path == "/v1/messages" and body["id"].startswith("brain_") else "")
            if tag and tag not in dropped:
                dropped.add(tag)
                raise OSError("simulated lost HTTP response after durable commit")
            return result
        bridge.client.request = lossy
        self.post("execute once", "lost_responses")
        wait_for(lambda: bridge.journal.status()["counts"].get("done") == 1,
                 description="idempotent reply resubmission")
        wait_for(lambda: not bridge.journal.acknowledgments(), description="idempotent handoff acknowledgment")
        self.assertEqual(dropped, {"ack", "reply"})
        self.assertEqual(len(calls), 1)
        self.assertEqual(len(self.replies()), 1)


if __name__ == "__main__":
    unittest.main()
