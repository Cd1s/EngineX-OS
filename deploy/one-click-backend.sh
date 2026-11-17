#!/usr/bin/env bash
# Run from an EngineX checkout on Debian/Ubuntu with systemd (amd64 or arm64).
# sudo bash deploy/one-click-backend.sh [--public https://relay.example.com] [--relay-bin /path/to/enginex-relay]
set -euo pipefail
PUBLIC_URL=""
RELAY_BIN=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --public) PUBLIC_URL="${2:?missing --public value}"; shift 2 ;;
    --relay-bin) RELAY_BIN="${2:?missing --relay-bin value}"; shift 2 ;;
    -h|--help) echo "Usage: sudo bash $0 [--public https://relay.example.com] [--relay-bin /path/to/enginex-relay]"; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "Run as root: sudo bash $0" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "This installer needs systemd" >&2; exit 1; }
command -v apt-get >/dev/null || { echo "This installer supports Debian/Ubuntu" >&2; exit 1; }
case $(uname -m) in
  x86_64|amd64) GOARCH=amd64 ;;
  aarch64|arm64) GOARCH=arm64 ;;
  *) echo "Supported architectures: amd64 and arm64" >&2; exit 1 ;;
esac
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl python3 python3-numpy python3-scipy ffmpeg openssl

# Go's automatic toolchain selection needs Go >=1.21. Bootstrap only when absent
# or older, into this private work directory, verifying the official checksum.
if ! command -v go >/dev/null || [[ $(go version) =~ go1\.(1[0-9]|[0-9])\. ]]; then
  python3 - "$WORK" "$GOARCH" <<'PYGO'
import hashlib, json, pathlib, sys, tarfile, urllib.request
work, arch = pathlib.Path(sys.argv[1]), sys.argv[2]
releases = json.load(urllib.request.urlopen('https://go.dev/dl/?mode=json', timeout=30))
release = next(r for r in releases if r['stable'])
asset = next(f for f in release['files'] if f['os']=='linux' and f['arch']==arch and f['kind']=='archive')
archive = work / 'go.tar.gz'
with urllib.request.urlopen('https://go.dev/dl/'+asset['filename'], timeout=120) as source, archive.open('wb') as out:
    while block := source.read(1024*1024):
        out.write(block)
with archive.open('rb') as source:
    if hashlib.file_digest(source, 'sha256').hexdigest() != asset['sha256']:
        raise SystemExit('Go download checksum mismatch')
with tarfile.open(archive) as source:
    source.extractall(work, filter='data')
PYGO
  export PATH="$WORK/go/bin:$PATH"
fi
export GOTOOLCHAIN=auto
if [ -z "$RELAY_BIN" ]; then
  echo "Building enginex-relay..."
  RELAY_BIN="$WORK/enginex-relay"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags='-s -w' -o "$RELAY_BIN" ./cmd/enginex-relay)
fi

# The renderer installer preserves its token on rerun; failed optional components
# are errors, not a successful deployment summary.
echo "Installing enginex-stemsd (HPSS renderer)..."
(cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags='-s -w' -o "$WORK/enginex-stemsd" ./cmd/enginex-stemsd)
cp "$REPO_ROOT/tools/stems/worker.py" "$SCRIPT_DIR/stems/enginex-stemsd.service" "$WORK/"
sh "$SCRIPT_DIR/stems/install-backend.sh" "$WORK"
mkdir -p /etc/systemd/system/enginex-relay.service.d

AM_STATUS="not installed (optional helper files are absent)"
if [ -f /opt/enginex-amd/enginex-amd.py ] && [ -f /opt/enginex-amd/enginex-am-login.py ] && [ -x /opt/wrapper-lite/amdl ] && [ -x /opt/wrapper-lite/wrapper-lite-rootless ]; then
  mkdir -p /var/lib/enginex-amd/data/com.apple.android.music/files /var/lib/enginex-amd/enginex-am
  if [ ! -f /etc/enginex-amd.env ]; then
    (umask 077; printf 'ENGINEX_AMD_TOKEN=%s\n' "$(openssl rand -hex 16)" > /etc/enginex-amd.env)
  fi
  for service in enginex-am-login enginex-amd; do
    install -m 644 "$SCRIPT_DIR/relay-host/$service.service" "/etc/systemd/system/$service.service"
  done
  systemctl daemon-reload
  systemctl enable --now enginex-am-login.service enginex-amd.service
  systemctl restart enginex-am-login.service enginex-amd.service
  systemctl is-active --quiet enginex-am-login.service enginex-amd.service
  install -m 644 "$SCRIPT_DIR/relay-host/60-applemusic.conf" /etc/systemd/system/enginex-relay.service.d/60-applemusic.conf
  AM_STATUS="running; the relay detects its LAN URL and synchronizes it to decks"
fi

if [ -n "$PUBLIC_URL" ]; then
  # Reject systemd specifiers/argument separators in an operator-provided URL.
  # shellcheck source=deploy/relay-host/migrate.sh
  . "$SCRIPT_DIR/relay-host/migrate.sh"
  TEMPLATE=$(installed_relay_unit)
  python3 - "$PUBLIC_URL" "${TEMPLATE:-$SCRIPT_DIR/relay-host/enginex-relay.service}" "$WORK/relay.service" <<'PYUNIT'
import pathlib, re, sys, urllib.parse
value, template, output = sys.argv[1:]
u = urllib.parse.urlsplit(value)
if u.scheme not in ('https', 'http') or not u.hostname or u.username or u.password or any(c.isspace() or c in '%$\\"' for c in value):
    raise SystemExit('Use an HTTP(S) public URL without credentials, spaces or systemd specifiers')
def update(match):
    line, n = re.subn(r'-public(?:=|\s+)(?:"[^"]*"|\S+)', '-public '+value, match[0], count=1)
    return line if n else line+' -public '+value
pathlib.Path(output).write_text(re.sub(r'^ExecStart=.*$', update, pathlib.Path(template).read_text(), flags=re.M))
PYUNIT
  sh "$SCRIPT_DIR/relay-host/install.sh" "$RELAY_BIN" "$WORK/relay.service"
else
  sh "$SCRIPT_DIR/relay-host/install.sh" "$RELAY_BIN"
fi
cp "$SCRIPT_DIR/relay-host/50-stems.conf" /etc/systemd/system/enginex-relay.service.d/50-stems.conf
systemctl daemon-reload
systemctl restart enginex-relay.service
systemctl is-active --quiet enginex-relay.service enginex-stemsd.service
LAN_IP=$(hostname -I | awk '{print $1}')
cat <<SUMMARY

EngineX backend is running.
Console: http://${LAN_IP}:6595
Console password file (root only): /var/lib/enginex-relay/initial-password
Change the initial password in the relay console.
Stems: enginex-stemsd on loopback port 6596, through the same relay URL/password.
Apple Music: $AM_STATUS
Public/Quick Tunnel URL: journalctl -u enginex-relay -n 30 --no-pager
Deck: run enginex-installer --setup-backend after flashing, or enter this backend URL/password in the deck's :8080 console.
Service: enginex-relay.service; state: /var/lib/enginex-relay
SUMMARY
