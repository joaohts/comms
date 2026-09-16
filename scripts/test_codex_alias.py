"""Exercise the installer's shell setup without changing the user's dotfiles."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SOURCE = (Path(__file__).parent / 'install.sh').read_text().split(
    '# BEGIN COMMS_CODEX_ALIAS\n', 1)[1].split('# END COMMS_CODEX_ALIAS', 1)[0]


class CodexAliasTests(unittest.TestCase):
    def test_shells_idempotency_upgrade_and_quoting(self):
        for shell in ('bash', 'zsh'):
            if not shutil.which(shell):
                continue
            with self.subTest(shell=shell), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                env = dict(os.environ, HOME=directory, SHELL='/bin/' + shell,
                           ZDOTDIR=directory)
                rc = root / ('.zshrc' if shell == 'zsh' else '.bashrc')
                rc.write_text('# existing config\n')
                binary = root / 'comms $cash `literal` "quote" \'apostrophe'
                binary.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
                binary.chmod(0o755)
                def install(target):
                    return subprocess.run([sys.executable, '-c', SOURCE, str(target)],
                                          env=env, check=True, capture_output=True)
                install(binary)
                first = rc.read_text()
                install(binary)
                self.assertEqual(first, rc.read_text())
                self.assertEqual(len(list(root.glob('.*.comms-backup-*'))), 1)
                script = ('shopt -s expand_aliases\n' if shell == 'bash' else '')
                invocation = root / 'invoke.sh'
                invocation.write_text('codex resume "thread with spaces"\n')
                script += 'source "$1"\nsource "$2"\n'
                result = subprocess.run([shell, '-c', script, 'test', str(rc), str(invocation)],
                                        env=env, check=True, capture_output=True, text=True)
                self.assertEqual(result.stdout, 'codex\nresume\nthread with spaces\n')
                install(root / 'comms-v1')
                self.assertEqual(rc.read_text().count('# BEGIN COMMS CODEX ALIAS'), 1)
                self.assertTrue(rc.read_text().startswith('# existing config\n'))

    def test_preserves_custom_alias_and_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            env = dict(os.environ, HOME=directory, SHELL='/bin/zsh', ZDOTDIR=directory)
            real = root / 'dotfile'
            real.write_text("alias codex='custom-codex'\n")
            rc = root / '.zshrc'
            rc.symlink_to(real)
            subprocess.run([sys.executable, '-c', SOURCE, '/bin/comms'],
                           env=env, check=True, capture_output=True)
            self.assertEqual(real.read_text(), "alias codex='custom-codex'\n")
            real.write_text('# config\n')
            subprocess.run([sys.executable, '-c', SOURCE, '/bin/comms'],
                           env=env, check=True, capture_output=True)
            self.assertTrue(rc.is_symlink())
            self.assertIn('alias codex=', real.read_text())


if __name__ == '__main__':
    unittest.main()
