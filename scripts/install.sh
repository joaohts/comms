#!/usr/bin/env bash
# Install a released comms binary and per-user supervisor, independent of the GUI.
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
bundle_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
binary="$bundle_dir/comms"
custom_binary=0
bin_dir="$HOME/.local/bin"
data_dir="${COMMS_DATA_DIR:-$HOME/.local/share/comms}"
data_explicit=0
[[ -z "${COMMS_DATA_DIR:-}" ]] || data_explicit=1
binary_name=comms
broker_listen=""
broker_explicit=0
legacy_proxy=""
legacy_explicit=0
service_key_file=""
service_key_explicit=0
allow_insecure=0
start=1
service=1
skills=1
codex_alias=1
claude_alias=1
replace_legacy=0
skill_backup_root="${XDG_STATE_HOME:-$HOME/.local/state}/comms/skill-backups"

# BEGIN COMMS_SKILL_BACKUP
backup_comms_skill() {
  local comms_skill_source="$1" comms_skill_harness="$2" comms_skill_name="$3" comms_backup_root="$4"
  local comms_backup_batch
  comms_backup_batch=$(umask 077; mkdir -p "$comms_backup_root"; mktemp -d "$comms_backup_root/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXX")
  mkdir -m 700 "$comms_backup_batch/$comms_skill_harness"
  cp -Rp "$comms_skill_source" "$comms_backup_batch/$comms_skill_harness/$comms_skill_name"
  printf 'Preserved skill backup: %s\n' "$comms_backup_batch/$comms_skill_harness/$comms_skill_name" >&2
}
# END COMMS_SKILL_BACKUP


# BEGIN COMMS_LAUNCHD_RESTART
restart_comms_launch_agent() {
  local comms_domain="$1" comms_plist="$2" comms_job="$1/com.joaohts.comms"
  local comms_old_job comms_old_pid='' comms_had_job=0 comms_removed=0
  local comms_poll comms_boot_error comms_boot_status=0
  if comms_old_job=$(launchctl print "$comms_job" 2>/dev/null); then
    comms_had_job=1
    comms_old_pid=$(printf '%s\n' "$comms_old_job" | awk '/^[[:space:]]*pid = [0-9]+/ {print $3; exit}')
    launchctl bootout "$comms_job" 2>/dev/null || true
    # bootout can return before launchd removes the job or the old node finishes
    # its drain. Wait for both, with room beyond the 15-second ExitTimeOut.
    for ((comms_poll=0;comms_poll<100;comms_poll++)); do
      if ! launchctl print "$comms_job" >/dev/null 2>&1 &&
         { [[ -z "$comms_old_pid" ]] || ! kill -0 "$comms_old_pid" 2>/dev/null; }; then
        comms_removed=1; break
      fi
      sleep 0.2
    done
    if (( comms_removed == 0 )); then
      printf 'Previous comms launch agent did not finish stopping within 20 seconds.\n' >&2
      return 1
    fi
  fi
  for ((comms_poll=0;comms_poll<10;comms_poll++)); do
    if comms_boot_error=$(launchctl bootstrap "$comms_domain" "$comms_plist" 2>&1); then
      return 0
    else
      comms_boot_status=$?
    fi
    # EIO immediately after an existing job's removal can be transient. Other
    # errors, and fresh-install failures, retain their original status/message.
    if (( comms_had_job == 0 || comms_boot_status != 5 )); then break; fi
    sleep 0.2
  done
  printf '%s\n' "$comms_boot_error" >&2
  return "$comms_boot_status"
}
# END COMMS_LAUNCHD_RESTART

usage() {
  cat <<'EOF'
Usage: scripts/install.sh [options]
  --binary PATH         Released executable (default: bundle/comms)
  --bin-dir PATH        Installation directory (default: ~/.local/bin)
  --binary-name NAME    Command name; comms-v1 permits side-by-side migration
  --data-dir PATH       Persistent identity and databases
  --broker-listen ADDR  Enable broker role on this address
  --legacy-proxy-url URL Forward non-/v1 HTTP paths to legacy broker
  --broker-service-key-file PATH  Private shared broker API key (server and client)
  --allow-insecure      Allow HTTP broker URLs for trusted local testing
  --replace-legacy      Explicitly back up and replace legacy CLI/skill files
  --skip-skills         Do not install open-comms integrations or shell alias
  --skip-codex-alias     Do not configure the Codex shell shortcut
  --skip-claude-alias    Do not configure the Claude shell shortcut
  --no-service          Install files without registering a supervisor
  --no-start            Register service but do not start/restart it
EOF
}

while (($#)); do
  case "$1" in
    --binary|--bin-dir|--binary-name|--data-dir|--broker-listen|--legacy-proxy-url|--broker-service-key-file)
      (($# >= 2)) || { usage >&2; exit 2; }
      case "$1" in
        --binary) binary=$2; custom_binary=1 ;; --bin-dir) bin_dir=$2 ;; --binary-name) binary_name=$2 ;;
        --data-dir) data_dir=$2; data_explicit=1 ;;
        --broker-listen) broker_listen=$2; broker_explicit=1 ;;
        --legacy-proxy-url) legacy_proxy=$2; legacy_explicit=1 ;;
        --broker-service-key-file) service_key_file=$2; service_key_explicit=1 ;;
      esac; shift 2 ;;
    --allow-insecure) allow_insecure=1; shift ;;
    --replace-legacy) replace_legacy=1; shift ;;
    --skip-skills) skills=0; shift ;;
    --skip-codex-alias) codex_alias=0; shift ;;
    --skip-claude-alias) claude_alias=0; shift ;;
    --no-service) service=0; start=0; shift ;;
    --no-start) start=0; shift ;;
    --help|-h) usage; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

case "$binary_name" in *[!A-Za-z0-9._-]*|'') printf 'Invalid binary name\n' >&2; exit 2 ;; esac
case "$data_dir$bin_dir$binary$broker_listen$legacy_proxy$service_key_file" in *$'\n'*) printf 'Paths/settings must not contain newlines\n' >&2; exit 2 ;; esac
test -x "$binary" || { printf 'Executable not found: %s\n' "$binary" >&2; exit 1; }
if (( custom_binary == 0 )); then
  python3 - "$bundle_dir" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path(sys.argv[1]).resolve(); manifest=root/'CHECKSUMS'
if not manifest.is_file(): raise SystemExit('Release CHECKSUMS missing; use a verified release bundle or explicitly provide --binary PATH.')
for line in manifest.read_text().splitlines():
    expected,name=line.split('  ',1); p=(root/name).resolve()
    if not p.is_relative_to(root) or hashlib.sha256(p.read_bytes()).hexdigest()!=expected:
        raise SystemExit('Release checksum mismatch: '+name)
PY
fi
"$binary" version --json | python3 -c 'import json,sys; v=json.load(sys.stdin); assert v.get("protocol_version")==1 and v.get("version"), "unsupported comms version"'
if (( service )); then
  existing_config=$(python3 - "$HOME" "${XDG_CONFIG_HOME:-$HOME/.config}" <<'PY'
import json,pathlib,plistlib,shlex,sys
home,config=map(pathlib.Path,sys.argv[1:]); args=[]
plist=home/'Library/LaunchAgents/com.joaohts.comms.plist'; unit=config/'systemd/user/comms-node.service'
if plist.is_file():
    with plist.open('rb') as f: args=plistlib.load(f).get('ProgramArguments',[])
elif unit.is_file():
    for line in unit.read_text().splitlines():
        if line.startswith('ExecStart='): args=shlex.split(line.partition('=')[2]); break
def value(flag): return args[args.index(flag)+1].replace('%%','%') if flag in args else ''
print(json.dumps({'data_dir':value('--data-dir'),'broker_listen':value('--broker-listen'),'legacy_proxy':value('--legacy-proxy-url'),'service_key_file':value('--broker-service-key-file'),'allow_insecure':'--allow-insecure' in args}))
PY
)
  existing_data=$(python3 -c 'import json,sys;print(json.load(sys.stdin)["data_dir"])' <<< "$existing_config")
  if [[ -n "$existing_data" ]]; then
    if (( data_explicit )) && [[ "$existing_data" != "$data_dir" ]]; then
      printf 'Existing node owns %s; refusing to switch its identity directory to %s. Stage a separate binary with --no-service for an intentional migration.\n' "$existing_data" "$data_dir" >&2
      exit 1
    fi
    data_dir=$existing_data
  fi
  if (( broker_explicit == 0 )); then
    broker_listen=$(python3 -c 'import json,sys;print(json.load(sys.stdin)["broker_listen"])' <<< "$existing_config")
  fi
  if (( legacy_explicit == 0 )); then
    legacy_proxy=$(python3 -c 'import json,sys;print(json.load(sys.stdin)["legacy_proxy"])' <<< "$existing_config")
  fi
  if (( service_key_explicit == 0 )); then
    service_key_file=$(python3 -c 'import json,sys;print(json.load(sys.stdin)["service_key_file"])' <<< "$existing_config")
  fi
  existing_insecure=$(python3 -c 'import json,sys;print(int(json.load(sys.stdin)["allow_insecure"]))' <<< "$existing_config")
  if (( existing_insecure )); then allow_insecure=1; fi
fi
if [[ -n "$service_key_file" ]]; then
  python3 - "$service_key_file" <<'PY'
# BEGIN COMMS_SERVICE_KEY_PREFLIGHT
import pathlib,sys
try:
    p=pathlib.Path(sys.argv[1]); info=p.stat()
    assert p.is_file() and info.st_mode & 0o077 == 0 and info.st_size <= 4096
    key=p.read_text(encoding='ascii').strip(' \t\r\n\v\f')
    assert 32 <= len(key) <= 4096 and all(33 <= ord(c) <= 126 for c in key)
except (OSError,UnicodeError,AssertionError):
    raise SystemExit('Invalid broker service key file: require a private regular file (0600) containing 32-4096 printable non-space ASCII characters.')
# END COMMS_SERVICE_KEY_PREFLIGHT
PY
fi
umask 077
mkdir -p "$bin_dir" "$data_dir"
[[ -O "$data_dir" ]] || { printf 'Data directory must be owned by the installing user: %s\n' "$data_dir" >&2; exit 1; }
chmod 700 "$data_dir"
target="$bin_dir/$binary_name"
stamp=$(date -u +%Y%m%dT%H%M%SZ)

if [[ -e "$target" || -L "$target" ]]; then
  # A modern CLI has a side-effect-free version command. Never run arbitrary
  # legacy commands with a new subcommand to identify them.
  if file -L "$target" | grep -Eq 'Mach-O|ELF'; then modern=1; else modern=0; fi
  if (( modern == 0 && replace_legacy == 0 )); then
    printf 'Existing command is preserved: %s. Choose --binary-name comms-v1 or explicit --replace-legacy.\n' "$target" >&2
    exit 1
  fi
  cp -pL "$target" "$target.backup-$stamp"
fi

install -m 755 "$binary" "$target.new"
mv -f "$target.new" "$target"

if (( skills )); then
  skill_name=open-comms
  [[ "$binary_name" == comms ]] || skill_name="open-$binary_name"
  for skill_harness in claude codex; do
    if [[ "$skill_harness" == claude ]]; then skill_root="$HOME/.claude/skills"; else skill_root="${CODEX_HOME:-$HOME/.codex}/skills"; fi
    skill_dir="$skill_root/$skill_name"
    if [[ -f "$skill_dir/SKILL.md" ]] && ! cmp -s "$bundle_dir/integration/open-comms/SKILL.md" "$skill_dir/SKILL.md"; then
      if (( replace_legacy == 0 )) && ! grep -Fq '<!-- comms installer managed skill -->' "$skill_dir/SKILL.md"; then
        printf 'Preserved existing skill: %s (use --replace-legacy to back it up and replace).\n' "$skill_dir" >&2
        continue
      fi
      backup_comms_skill "$skill_dir" "$skill_harness" "$skill_name" "$skill_backup_root"
    fi
    mkdir -p "$skill_dir"
    install -m 644 "$bundle_dir/integration/open-comms/SKILL.md" "$skill_dir/SKILL.md"
    # Use short command names in the skill, preserving a distinct name when
    # this installation still coexists with another comms executable.
    python3 - "$skill_dir/SKILL.md" "$target" "$skill_name" 'comms installer managed skill' <<'PY'
# BEGIN COMMS_SKILL_RENDER
import json,pathlib,re,sys
p=pathlib.Path(sys.argv[1]); command=pathlib.Path(sys.argv[2]).absolute()
skill,marker=sys.argv[3:5]; label=command.name
short=command.with_name('comms')
if short.is_file() and short.samefile(command):
    label='comms'
text=p.read_text().replace('name: open-comms\n','name: '+skill+'\n',1)
# Older release bundles still contain the former executable resolver.
old_preamble='Resolve the CLI from `COMMS_BIN` when set, otherwise use `comms`. In all\ncommands below, `comms` means that resolved executable. Quote the executable\nas `"${COMMS_BIN:-comms}"` in shell commands; never overwrite PATH or fall back\nto a legacy board when the new node is unavailable.\n\n'
text=text.replace(old_preamble,'',1)
def monitor(match):
    value=json.loads(match.group('command')).replace('"${COMMS_BIN:-comms}"','comms')
    value=re.sub(r'^comms(?=\s|$)',lambda _:label,value)
    return match.group('prefix')+json.dumps(value,ensure_ascii=False)
text=re.sub(r'(?P<prefix>Monitor\(\{\s*command:\s*)(?P<command>"(?:\\.|[^"\\])*")',monitor,text)
text=re.sub(r'(?m)^comms(?=\s)',lambda _:label,text)
text=re.sub(r'`comms(?= |`)',lambda _:'`'+label,text)
text+='\n<!-- '+marker+' -->\n'
p.write_text(text)
# END COMMS_SKILL_RENDER
PY
  done
fi
if (( skills && codex_alias )); then
  python3 - "$target" <<'PYALIAS'
# BEGIN COMMS_CODEX_ALIAS
import os,pathlib,re,shlex,shutil,sys,tempfile
command=str(pathlib.Path(sys.argv[1]).absolute())
shell=pathlib.Path(os.environ.get('SHELL','')).name
home=pathlib.Path.home()
if shell == 'zsh':
    rc=pathlib.Path(os.environ.get('ZDOTDIR') or home)/'.zshrc'
elif shell == 'bash':
    rc=home/'.bashrc'
else:
    print('Codex shortcut skipped: unsupported shell. Launch with '+shlex.quote(command)+' codex.')
    sys.exit(0)
# Resolve symlinks so dotfile-manager links stay intact.
rc=rc.resolve()
start='# BEGIN COMMS CODEX ALIAS'
end='# END COMMS CODEX ALIAS'
old=rc.read_text() if rc.exists() else ''
pattern=re.compile(r'^'+re.escape(start)+r'\n.*?^'+re.escape(end)+r'(?:\n|$)',re.M|re.S)
matches=list(pattern.finditer(old))
if old.count(start)!=len(matches) or old.count(end)!=len(matches) or len(matches)>1:
    raise SystemExit('Malformed comms alias block in '+str(rc)+'; fix it or use --skip-codex-alias.')
unmanaged=pattern.sub('',old)
if re.search(r'^\s*(?:alias\s+codex=|(?:function\s+)?codex\s*\(\s*\)|function\s+codex\b)',unmanaged,re.M):
    print('Preserved existing Codex alias/function in '+str(rc)+'. Use '+shlex.quote(command)+' codex for comms.')
    sys.exit(0)
block=start+'\n# Interactive sessions use comms; bypass with: command codex <args>\n'
block+='alias codex='+shlex.quote(shlex.quote(command)+' codex')+'\n'+end+'\n'
new=pattern.sub(lambda _:block,old) if matches else old+('' if not old or old.endswith('\n') else '\n')+block
if new!=old:
    rc.parent.mkdir(parents=True,exist_ok=True)
    if rc.exists():
        fd,backup=tempfile.mkstemp(prefix=rc.name+'.comms-backup-',dir=rc.parent)
        os.close(fd)
        shutil.copy2(rc,backup)
        print('Shell config backup: '+backup)
    rc.write_text(new)
print('Codex shortcut installed in '+str(rc)+'. Open a new shell or source this file; then run codex.')
if shell == 'bash':
    print('Bash login shells must source ~/.bashrc to load the shortcut.')
# END COMMS_CODEX_ALIAS
PYALIAS
fi

if (( skills && claude_alias )); then
  python3 "$bundle_dir/integration/claude-alias.py" "$target"
fi

args=(serve --data-dir "$data_dir")
[[ -z "$service_key_file" ]] || args+=(--broker-service-key-file "$service_key_file")
[[ -z "$broker_listen" ]] || args+=(--broker-listen "$broker_listen")
[[ -z "$legacy_proxy" ]] || args+=(--legacy-proxy-url "$legacy_proxy")
(( allow_insecure == 0 )) || args+=(--allow-insecure)

if (( service )); then
  case $(uname -s) in
    Darwin)
      plist="$HOME/Library/LaunchAgents/com.joaohts.comms.plist"
      mkdir -p "$(dirname "$plist")" "$data_dir/logs"
      python3 - "$plist" "$target" "$data_dir" "${args[@]}" <<'PY'
import plistlib,sys
path,binary,data=sys.argv[1:4]
job={'Label':'com.joaohts.comms','ProgramArguments':[binary]+sys.argv[4:],
     'RunAtLoad':True,'KeepAlive':True,'ThrottleInterval':5,
     'ProcessType':'Background','Umask':0o077,'ExitTimeOut':15,
     'StandardOutPath':data+'/logs/service.out.log',
     'StandardErrorPath':data+'/logs/service.err.log'}
with open(path,'wb') as f: plistlib.dump(job,f)
PY
      if (( start )); then
        restart_comms_launch_agent "gui/$(id -u)" "$plist"
      fi
      ;;
    Linux)
      unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
      mkdir -p "$unit_dir"
      python3 - "$unit_dir/comms-node.service" "$target" "${args[@]}" <<'PY'
import pathlib,sys
def quote(s): return '"'+s.replace('\\','\\\\').replace('"','\\"').replace('%','%%')+'"'
command=' '.join(quote(v) for v in sys.argv[2:])
pathlib.Path(sys.argv[1]).write_text('[Unit]\nDescription=Comms local node and optional encrypted broker\nAfter=network.target\n\n[Service]\nType=simple\nExecStart='+command+'\nRestart=on-failure\nRestartSec=3\nTimeoutStopSec=15\nKillMode=mixed\nUMask=0077\nNoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n')
PY
      systemctl --user daemon-reload
      systemctl --user enable comms-node.service
      if (( start )); then systemctl --user restart comms-node.service; fi
      ;;
    *) printf 'No supervisor integration for this OS; run %s serve --data-dir %s.\n' "$target" "$data_dir" >&2 ;;
  esac
fi

printf 'Installed %s. Data: %s\n' "$target" "$data_dir"
if (( service && start )); then
  ready=0
  for ((i=0;i<30;i++)); do
    if "$target" status --data-dir "$data_dir" --json >/dev/null 2>&1; then ready=1; break; fi
    sleep 0.2
  done
  (( ready )) || { printf 'Service did not become ready; inspect its supervisor logs.\n' >&2; exit 1; }
  "$target" status --data-dir "$data_dir" --json
fi
