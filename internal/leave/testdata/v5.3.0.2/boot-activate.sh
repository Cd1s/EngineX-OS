#!/bin/sh
# One-shot boot activator for SC LIVE 4 (inmusic,jp21) and read-only root decks (#61).
set -eu

dest="${SC5000_CUSTOM:-/data/sc5000-custom}"
bundle_dir="$dest/bundle"
bin_dir="$bundle_dir/bin"
lib_hook="$bundle_dir/lib/libengine-ui-hook.so"

mkdir -p /run/systemd/system/engine.service.d

cat <<EOF > /run/systemd/system/engine.service.d/99-sc5000.conf
[Service]
Environment="LD_PRELOAD=$lib_hook"
EOF

if [ -f "$bundle_dir/version.txt" ]; then
    mkdir -p /run/sc5000
    if [ ! -f /etc/sc5000/version.txt ] || ! cmp -s "$bundle_dir/version.txt" /etc/sc5000/version.txt; then
        cp "$bundle_dir/version.txt" /run/sc5000/version.txt
        if [ -f /etc/sc5000/version.txt ]; then
            mount --bind /run/sc5000/version.txt /etc/sc5000/version.txt 2>/dev/null || :
        fi
    fi
fi

for applet in stream-sync rb-bridge sc5000-deemixd; do
    if [ -x "$bin_dir/$applet" ] && ! pgrep -f "$bin_dir/$applet" >/dev/null 2>&1; then
        "$bin_dir/$applet" >/dev/null 2>&1 &
    fi
done

if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || :
    systemctl restart engine.service || :
fi
