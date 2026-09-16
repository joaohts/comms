"""Run actual installer key preflight and upgrade config extraction in fixtures."""
import json
from pathlib import Path
import plistlib
import shlex
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).with_name('install.sh').read_text()


class ServiceKeyInstallerTests(unittest.TestCase):
    def test_preflight_never_prints_key_and_rejects_invalid_files(self):
        code=SOURCE.split('# BEGIN COMMS_SERVICE_KEY_PREFLIGHT\n',1)[1].split('# END COMMS_SERVICE_KEY_PREFLIGHT',1)[0]
        with tempfile.TemporaryDirectory() as directory:
            p=Path(directory)/'key'; secret='test-secret-that-must-never-be-printed-123456'
            for value,mode,success in [(secret+'\n',0o600,True),('',0o600,False),(secret,0o644,False),('short',0o600,False),(secret+'\nheader: injected',0o600,False)]:
                p.write_text(value);p.chmod(mode)
                result=subprocess.run([sys.executable,'-c',code,str(p)],capture_output=True,text=True)
                self.assertEqual(result.returncode==0,success)
                self.assertNotIn(secret,result.stdout+result.stderr)

    def test_upgrade_preserves_explicit_key_path_on_both_platforms(self):
        reader=SOURCE.split('existing_config=$(python3',1)[1].split("<<'PY'\n",1)[1].split('\nPY\n',1)[0]
        inherit=SOURCE.split('  if (( service_key_explicit == 0 )); then\n',1)[1].split('  fi\n',1)[0]
        args=SOURCE.split('args=(serve --data-dir "$data_dir")',1)[1].split('\nif (( service )); then',1)[0]
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);home=root/'fixture';config=root/'config';key=root/'private key'
            argv=['/fixture/comms','serve','--data-dir','/fixture/data','--broker-service-key-file',str(key)]
            for platform in ('mac','linux'):
                plist=home/'Library/LaunchAgents/com.joaohts.comms.plist'
                unit=config/'systemd/user/comms-node.service'
                if platform=='mac':
                    plist.parent.mkdir(parents=True);plist.write_bytes(plistlib.dumps({'ProgramArguments':argv}))
                else:
                    plist.unlink();unit.parent.mkdir(parents=True);unit.write_text('ExecStart='+shlex.join(argv)+'\n')
                output=subprocess.check_output([sys.executable,'-c',reader,str(home),str(config)],text=True)
                self.assertEqual(json.loads(output)['service_key_file'],str(key))
                script='set -euo pipefail\nexisting_config='+shlex.quote(output)+'\nservice_key_file=""\n'+inherit
                script+='\ndata_dir=/fixture/data\nbroker_listen=""\nlegacy_proxy=""\nallow_insecure=0\nargs=(serve --data-dir "$data_dir")'+args+'\nprintf "%s\\n" "${args[@]}"\n'
                result=subprocess.check_output(['bash','-c',script],text=True).splitlines()
                self.assertEqual(result[result.index('--broker-service-key-file')+1],str(key))


if __name__=='__main__':unittest.main()
