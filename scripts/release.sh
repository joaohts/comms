#!/usr/bin/env bash
set -euo pipefail
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
release_version=${1:?usage: scripts/release.sh VERSION [OUTPUT_DIRECTORY]}
release_version=${release_version#v}
output_dir=${2:-$repo_dir/dist}
case "$release_version" in *[!A-Za-z0-9.+-]*|'') printf 'Invalid release version\n' >&2; exit 2 ;; esac
mkdir -p "$output_dir"
output_dir=$(CDPATH= cd -- "$output_dir" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
cd "$repo_dir"

for tuple in Darwin:darwin:arm64 Darwin:darwin:amd64 Linux:linux:arm64 Linux:linux:amd64; do
  IFS=: read -r os_name go_os go_arch <<< "$tuple"
  artifact="comms_${os_name}_${go_arch}.tar.gz"
  root="$stage/${os_name}_${go_arch}"
  mkdir -p "$root/scripts" "$root/integration" "$root/deploy"
  CGO_ENABLED=0 GOOS="$go_os" GOARCH="$go_arch" go build -trimpath -ldflags "-s -w -X main.version=$release_version -X github.com/joaohts/comms/internal/comms.Version=$release_version" -o "$root/comms" ./cmd/comms
  cp scripts/install.sh "$root/scripts/"
  cp -R integration/open-comms "$root/integration/"
  cp integration/claude-alias.py "$root/integration/"
  if [[ -d integration/brain ]]; then
    mkdir -p "$root/integration/brain"
    cp integration/brain/comms_v1.py integration/brain/patch_brain.py "$root/integration/brain/"
  fi
  cp deploy/README.md "$root/deploy/"
  printf '%s\n' "$release_version" > "$root/VERSION"
  python3 - "$root" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path(sys.argv[1])
rows=[hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.relative_to(root).as_posix() for p in sorted(root.rglob('*')) if p.is_file()]
(root/'CHECKSUMS').write_text('\n'.join(rows)+'\n')
PY
  COPYFILE_DISABLE=1 LC_ALL=C tar -C "$root" -czf "$output_dir/$artifact" comms scripts integration deploy VERSION CHECKSUMS
done

(
  cd "$output_dir"
  if command -v sha256sum >/dev/null 2>&1; then sha256sum comms_*.tar.gz > SHA256SUMS
  else shasum -a 256 comms_*.tar.gz > SHA256SUMS
  fi
)
printf 'Release %s artifacts ready in %s\n' "$release_version" "$output_dir"
