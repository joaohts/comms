import ast
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import types
import unittest
from unittest.mock import Mock, patch

import patch_brain
import retire_legacy


SERVER = '''import threading
def comms_ingress(): pass
def main():
    threading.Thread(target=comms_ingress, daemon=True).start()
    print("HTTP brain remains")
'''
TOOLS = '''import os, subprocess, time
def deliver(channel, message, origin, cfg, db):
    if channel.startswith("voice@"):
        import urllib.request
        try:
            urllib.request.urlopen("http://127.0.0.1:3403/trigger", timeout=10)
            status = "voice delivered"
''' + retire_legacy.OLD_VOICE_FALLBACK + '''    elif channel.startswith("comms:"):
        subprocess.run(["old-board"])
        status = "legacy"
    elif channel == "cli":
        status = "cli"
    else:
        return "unknown"
    db.add_message(channel, "assistant", origin, message)
    return status

def t_claude_spawn(env, args, cfg, db):
    import json as _json
    sid = "test-123"
    cwd = "/tmp/test"
    target = f"pi:session-{sid}"
    task = args["task"]
    # deliver the task over comms (doorbell-reliable — old code)
    for _ in range(20):
        pass
    db.step(env["turn_id"], "session_spawned", channel=env["channel"])
    return (f"session {sid} spawned: "
            f"comms:pi:session-{sid}; message it with send_to on that "
            f"channel.")

def t_comms_who(env, args, cfg, db):
    r = subprocess.run(["comms", "who"], capture_output=True, text=True,
                       timeout=15)
    return r.stdout
'''


class RetirementTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / 'brain').mkdir()
        (self.root / 'brain/server.py').write_text(SERVER)
        (self.root / 'brain/tools.py').write_text(TOOLS)

    def functions(self):
        result = retire_legacy.prepare(self.root)
        namespace = {'__package__': 'retirement_fixture'}
        exec(compile(result['brain/tools.py'], 'tools.py', 'exec'), namespace)
        return namespace

    def test_retirement_stays_retired_after_ordinary_upgrade_and_rolls_back(self):
        result = retire_legacy.install(self.root)
        server = (self.root / 'brain/server.py').read_text()
        tree = ast.parse(server)
        main = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == 'main')
        self.assertNotIn('comms_ingress', ast.unparse(main))
        self.assertIn('start_if_enabled', ast.unparse(main))
        self.assertEqual(retire_legacy.install(self.root)['changed'], [])
        self.assertEqual(patch_brain.install(self.root)['changed'], [])
        self.assertIn(patch_brain.RETIRED_TOOLS_HOOK, (self.root / 'brain/tools.py').read_text())
        patch_brain.rollback(result['backup'])
        self.assertEqual((self.root / 'brain/server.py').read_text(), SERVER)
        self.assertEqual((self.root / 'brain/tools.py').read_text(), TOOLS)

    def test_failed_preflight_does_not_partially_retire_server(self):
        (self.root / 'brain/tools.py').write_text(TOOLS.replace(retire_legacy.OLD_VOICE_FALLBACK, '        except Exception: pass\n'))
        with self.assertRaises(ValueError):
            retire_legacy.install(self.root)
        self.assertEqual((self.root / 'brain/server.py').read_text(), SERVER)

    def test_old_channel_names_use_new_adapter_and_keep_relay_provenance(self):
        functions = self.functions()
        module = types.ModuleType('retirement_fixture.comms_v1')
        module.deliver = Mock(return_value={'ok': True, 'id': 'msg_1'})
        db = Mock()
        with patch.dict(sys.modules, {'retirement_fixture.comms_v1': module}), patch('subprocess.run') as legacy:
            for prefix in ('comms:', 'comms-v1:'):
                result = functions['deliver'](prefix+'m_pi:a_worker', 'hello', 'relayed from cli', {}, db)
                self.assertIn('queued', result)
                module.deliver.assert_called_with('m_pi:a_worker', 'hello\n[relayed from cli; replies return through brain to that channel]')
            legacy.assert_not_called()
            module.deliver.return_value = {'ok': False, 'error': 'offline'}
            db.reset_mock()
            self.assertEqual(functions['deliver']('comms:pi:brain', 'hello', 'cli', {}, db), 'offline')
            db.add_message.assert_not_called()

    def test_unavailable_voice_does_not_fallback_or_claim_delivery(self):
        functions = self.functions()
        db = Mock()
        with patch('urllib.request.urlopen', side_effect=OSError('offline')), patch('subprocess.run') as legacy:
            self.assertEqual(functions['deliver']('voice@test', 'hello', 'cli', {}, db), 'voice unavailable: OSError')
            legacy.assert_not_called()
            db.add_message.assert_not_called()

    def test_spawn_selects_local_live_identity_and_uses_stable_submission_id(self):
        functions = self.functions()
        peers = [
            {'address': 'mac:session-test-123', 'recipient': 'm_wrong:a_wrong', 'online': True},
            {'address': 'session-test-123', 'recipient': 'm_pi:a_real', 'online': True},
        ]
        calls = []
        def run(argv, **kwargs):
            calls.append((argv, kwargs))
            return subprocess.CompletedProcess(argv, 0, json.dumps(peers) if argv[1] == 'who' else '{"id":"task_1","state":"queued"}', '')
        with patch('subprocess.run', side_effect=run), patch('time.sleep'):
            result = functions['t_claude_spawn']({'turn_id':'t1', 'channel':'cli'}, {'task':'task body'}, {}, Mock())
        argv, kwargs = calls[-1]
        self.assertEqual(argv[argv.index('--to')+1], 'm_pi:a_real')
        self.assertEqual(argv[argv.index('--id')+1], 'brain_spawn_test-123')
        self.assertEqual(kwargs['input'], 'task body')
        self.assertIn('comms-v1:m_pi:a_real', result)

    def test_spawn_reports_uncertain_submission_without_retrying_or_claiming_started(self):
        functions = self.functions()
        responses = [subprocess.CompletedProcess([], 0, '[{"address":"session-test-123","recipient":"m_pi:a_real","online":true}]', ''),
                     subprocess.CompletedProcess([], 1, '', 'timeout')]
        db = Mock()
        with patch('subprocess.run', side_effect=responses) as run, patch('time.sleep'):
            result = functions['t_claude_spawn']({'turn_id':'t1', 'channel':'cli'}, {'task':'task body'}, {}, db)
        self.assertIn('uncertain', result)
        self.assertIn('brain_spawn_test-123', result)
        self.assertEqual(run.call_count, 2)
        db.step.assert_not_called()


if __name__ == '__main__':
    unittest.main()
