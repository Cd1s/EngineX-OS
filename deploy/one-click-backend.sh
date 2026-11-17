#!/usr/bin/env bash
# One-click EngineX OS backend setup:
#   - enginex-relay (credential console + OAuth + optional trycloudflare quick tunnel)
#   - enginex-stemsd (if tools/stems is present)
#   - enginex-amd + enginex-am-login (if /opt/enginex-amd & /opt/wrapper-lite are present)
#
# Usage:
#   sudo bash deploy/one-click-backend.sh [--public https://relay.example.com] [--relay-bin /path/to/enginex-relay]
set -euo pipefail

PUBLIC_URL=""
RELAY_BIN=""
while [ $# -gt 0 ]; do
  case "$1" in
    --public) PUBLIC_URL="${2:?missing --public value}"; shift 2 ;;
    --relay-bin) RELAY_BIN="${2:?missing --relay-bin value}"; shift 2 ;;
    -h|--help)
      echo "Usage: sudo bash $0 [--public https://relay.example.com] [--relay-bin ./enginex-relay]"
      exit 0
      ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = 0 ] || { echo "Please run as root (sudo bash $0)" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq curl ffmpeg python3 openssl >/dev/null
fi

# 1. Build or install enginex-relay
if [ -z "$RELAY_BIN" ]; then
  if [ -f "$REPO_ROOT/cmd/enginex-relay/main.go" ] && command -v go >/dev/null 2>&1; then
    echo "==> Building enginex-relay from source..."
    RELAY_BIN="/tmp/enginex-relay-$GOARCH"
    (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags="-s -w" -o "$RELAY_BIN" ./cmd/enginex-relay)
  elif [ -x /usr/local/bin/enginex-relay ]; then
    RELAY_BIN="/usr/local/bin/enginex-relay"
  else
    echo "error: enginex-relay binary not found. Pass --relay-bin /path/to/enginex-relay or install Go." >&2
    exit 1
  fi
fi

install -m 0755 "$RELAY_BIN" /usr/local/bin/enginex-relay
mkdir -p /var/lib/enginex-relay /etc/systemd/system/enginex-deemix-relay.service.d

EXEC_ARGS="-listen 0.0.0.0:6595 -state /var/lib/enginex-relay"
if [ -n "$PUBLIC_URL" ]; then
  EXEC_ARGS="$EXEC_ARGS -public $PUBLIC_URL"
else
  EXEC_ARGS="$EXEC_ARGS -quick-tunnel"
fi

cat > /etc/systemd/system/enginex-deemix-relay.service <<EOF
[Unit]
Description=EngineX OS Relay Console
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/enginex-relay $EXEC_ARGS
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF

# 2. Optional remote Stems backend (if tools/stems exists)
STEMS_PORT=""
if [ -f "$SCRIPT_DIR/stems/install-backend.sh" ] && [ -d "$REPO_ROOT/tools/stems" ]; then
  echo "==> Installing enginex-stemsd..."
  bash "$SCRIPT_DIR/stems/install-backend.sh" || true
  if [ -f "$SCRIPT_DIR/relay-host/50-stems.conf" ]; then
    cp "$SCRIPT_DIR/relay-host/50-stems.conf" /etc/systemd/system/enginex-deemix-relay.service.d/50-stems.conf
    STEMS_PORT="8787"
  fi
fi

# 3. Optional Apple Music download helper (if /opt/enginex-amd & /opt/wrapper-lite exist)
AM_TOKEN=""
if [ -f /opt/enginex-amd/enginex-amd.py ] && [ -x /opt/wrapper-lite/amdl ]; then
  echo "==> Configuring enginex-amd Apple Music helper..."
  mkdir -p /var/lib/enginex-amd/data/com.apple.android.music/files /var/lib/enginex-amd/enginex-am
  if [ ! -f /etc/enginex-amd.env ]; then
    AM_TOKEN="$(openssl rand -hex 16)"
    printf "ENGINEX_AMD_TOKEN=%s\n" "$AM_TOKEN" > /etc/enginex-amd.env
    chmod 600 /etc/enginex-amd.env
  else
    AM_TOKEN="$(sed -n 's/^ENGINEX_AMD_TOKEN=//p' /etc/enginex-amd.env | head -n1)"
  fi
  cat > /etc/systemd/system/enginex-am-login.service <<'EOF'
[Unit]
Description=EngineX Apple Music Login & Wrapper-Lite
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/enginex-amd.env
ExecStart=/usr/bin/python3 /opt/enginex-amd/enginex-am-login.py --addr 0.0.0.0 --port 12342 --wrapper-lite /opt/wrapper-lite/wrapper-lite-rootless --base-dir /var/lib/enginex-amd/data/com.apple.android.music/files --lite-port 12340
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
  cat > /etc/systemd/system/enginex-amd.service <<'EOF'
[Unit]
Description=EngineX Apple Music Download Helper
After=network-online.target enginex-am-login.service
Wants=enginex-am-login.service

[Service]
Type=simple
EnvironmentFile=/etc/enginex-amd.env
ExecStart=/usr/bin/python3 /opt/enginex-amd/enginex-amd.py --addr 0.0.0.0 --port 12341 --lite http://127.0.0.1:12340 --amdl /opt/wrapper-lite/amdl --out /var/lib/enginex-amd/enginex-am
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now enginex-am-login.service enginex-amd.service
fi

systemctl daemon-reload
systemctl enable --now enginex-deemix-relay.service
sleep 2

LAN_IP="$(hostname -I | awk '{print $1}')"
echo ""
echo "========================================"
echo " EngineX OS 后端部署完成"
echo "========================================"
echo "局域网控制台: http://${LAN_IP}:6595"
if [ -n "$PUBLIC_URL" ]; then
  echo "公网地址:     $PUBLIC_URL"
else
  echo "公网隧道:     查看日志获取 trycloudflare.com 地址 -> journalctl -u enginex-deemix-relay -n 20 --no-pager"
fi
if [ -n "$STEMS_PORT" ]; then
  echo "Stems 服务:   active (127.0.0.1:${STEMS_PORT})"
fi
if [ -n "$AM_TOKEN" ]; then
  echo "Apple Music:  http://${LAN_IP}:12341  (Token: ${AM_TOKEN})"
fi
echo "初始密码查看: journalctl -u enginex-deemix-relay -n 30 --no-pager | grep -i password"
