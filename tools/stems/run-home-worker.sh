#!/bin/sh
# Turn this computer (macOS or Linux; Windows: WSL with mirrored networking)
# into the Stems renderer (and optional domain-free Cloudflare Quick Tunnel relay)
# for EngineX OS decks. Decks on the same LAN find it automatically (UDP 17891),
# and --with-tunnel starts a zero-config https://*.trycloudflare.com public relay.
#
#   tools/stems/run-home-worker.sh                        # default: install demucs (htdemucs) and use it
#   tools/stems/run-home-worker.sh hpss                   # fast spectral split, numpy + scipy only
#   tools/stems/run-home-worker.sh --with-tunnel [hpss]   # also launch domain-free Cloudflare Quick Tunnel
#
# Needs python3 and the enginex-stemsd binary (in PATH, next to this script, or
# ENGINEX_STEMSD=/path; SC5000_STEMSD is also accepted for compatibility).
# Python packages go into ~/.enginex-stems/venv.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
home_dir=${ENGINEX_STEMS_HOME:-$HOME/.enginex-stems}
venv=$home_dir/venv
with_tunnel=${ENGINEX_WITH_TUNNEL:-0}
backend=""

for arg in "$@"; do
  case "$arg" in
    --with-tunnel|--tunnel)
      with_tunnel=1
      ;;
    *)
      backend="$arg"
      ;;
  esac
done
backend=${backend:-${ENGINEX_STEMS_BACKEND:-demucs}}

bin=${ENGINEX_STEMSD:-${SC5000_STEMSD:-$(command -v enginex-stemsd || command -v sc5000-stemsd || echo "$here/enginex-stemsd")}}
if [ ! -x "$bin" ] && [ -x "$here/sc5000-stemsd" ]; then
  bin="$here/sc5000-stemsd"
fi
if [ ! -x "$bin" ]; then
  echo "EngineX OS: enginex-stemsd not found: put the release binary next to this script or build it with 'go build ./cmd/enginex-stemsd'" >&2
  exit 1
fi
[ -x "$venv/bin/python3" ] || python3 -m venv "$venv"
py=$venv/bin/python3
"$py" -c 'import numpy, scipy' 2>/dev/null || "$py" -m pip install -q numpy scipy
case $backend in
  demucs|htdemucs) "$py" -c 'import demucs' 2>/dev/null || "$py" -m pip install -q demucs ;;
esac
if [ "$("$py" "$here/worker.py" --backend "$backend" --print-backend)" = htdemucs ]; then
  # Fetch the model weights now, not during a deck's first session.
  "$py" -c 'from demucs.pretrained import get_model; get_model("htdemucs")'
fi

relay_bin=${ENGINEX_RELAY:-${SC5000_RELAY:-$(command -v enginex-relay || command -v sc5000-relay || true)}}
if [ -z "$relay_bin" ]; then
  if [ -x "$here/enginex-relay" ]; then
    relay_bin="$here/enginex-relay"
  elif [ -x "$here/sc5000-relay" ]; then
    relay_bin="$here/sc5000-relay"
  fi
fi

token_file=""
if [ "$with_tunnel" = "1" ]; then
  mkdir -p "$home_dir"
  token_file="$home_dir/stems-token"
  if [ ! -s "$token_file" ]; then
    (umask 077 && "$py" -c 'import secrets; print(secrets.token_hex(16))' > "$token_file")
  fi
  chmod 600 "$token_file"
  if [ -n "$relay_bin" ] && [ -x "$relay_bin" ]; then
    echo "EngineX OS: starting enginex-relay with domain-free Cloudflare Quick Tunnel (-public auto)..." >&2
    "$relay_bin" -public auto -stems-target http://127.0.0.1:17890 -stems-token-file "$token_file" -state "$home_dir/relay-state" &
    tunnel_pid=$!
    trap 'kill "$tunnel_pid" 2>/dev/null || true' EXIT INT TERM
  else
    cf_bin=$(command -v cloudflared || echo "$home_dir/bin/cloudflared")
    if [ ! -x "$cf_bin" ]; then
      mkdir -p "$home_dir/bin"
      arch=$(uname -m)
      case "$(uname -s):$arch" in
        Linux:aarch64|Linux:arm64) cf_asset="cloudflared-linux-arm64" ;;
        Linux:armv*)               cf_asset="cloudflared-linux-arm" ;;
        Linux:x86_64|Linux:amd64)  cf_asset="cloudflared-linux-amd64" ;;
        *)                         cf_asset="" ;;
      esac
      if [ -n "$cf_asset" ]; then
        echo "EngineX OS: downloading $cf_asset for domain-free Quick Tunnel..." >&2
        curl -fsSL "https://github.com/cloudflare/cloudflared/releases/latest/download/$cf_asset" -o "$home_dir/bin/cloudflared" && chmod 755 "$home_dir/bin/cloudflared"
        cf_bin="$home_dir/bin/cloudflared"
      fi
    fi
    if [ -x "$cf_bin" ]; then
      echo "EngineX OS: starting Cloudflare Quick Tunnel for enginex-stemsd (http://127.0.0.1:17890)..." >&2
      echo "EngineX OS: tunnel bearer token stored in $token_file (set this as your deck Relay Password; LAN decks stay zero-config)" >&2
      "$cf_bin" tunnel --no-autoupdate --url http://127.0.0.1:17890 &
      tunnel_pid=$!
      trap 'kill "$tunnel_pid" 2>/dev/null || true' EXIT INT TERM
    else
      echo "EngineX OS: install cloudflared or enginex-relay to use --with-tunnel on this platform" >&2
      exit 1
    fi
  fi
else
  echo "EngineX OS: LAN Stems worker starting (tip: pass --with-tunnel to also launch a domain-free Cloudflare Quick Tunnel)" >&2
fi

exec "$bin" -listen 0.0.0.0:17890 -token-file "$token_file" -python "$py" -worker "$here/worker.py" -backend "$backend" -sessions 2
