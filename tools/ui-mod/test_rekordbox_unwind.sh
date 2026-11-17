#!/bin/sh
# Run on the native armhf-capable build host, in the private /data namespace.
set -eu
task_src=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
task_tmp=$(mktemp -d)
task_cross=${CROSS-arm-linux-gnueabihf-}
export SC_RBX_REAL_STRIP="$(command -v "${task_cross}strip")"
mkdir "$task_tmp/bin"
trap 'rm -f "$task_tmp/bin/arm-linux-gnueabihf-strip" "$task_tmp/hook.so" "$task_tmp/hook.so.unstripped" "$task_tmp/driver"; rmdir "$task_tmp/bin" "$task_tmp"' EXIT
cat > "$task_tmp/bin/arm-linux-gnueabihf-strip" <<'EOF'
#!/bin/sh
set -eu
cp "$1" "$1.unstripped"
exec "$SC_RBX_REAL_STRIP" "$@"
EOF
chmod 700 "$task_tmp/bin/arm-linux-gnueabihf-strip"
PATH="$task_tmp/bin:$PATH" OUT="$task_tmp/hook.so" sh "$task_src/build.sh"
sha256sum "$task_tmp/hook.so"
task_slot=$("${task_cross}readelf" -Ws "$task_tmp/hook.so.unstripped" | awk '$8 == "tidal_track_decode" { print "0x" $2 }')
task_wrapper=$("${task_cross}readelf" -Ws "$task_tmp/hook.so.unstripped" | awk '$8 == "rekordbox_track_decode" { print "0x" $2 }')
test -n "$task_slot" && test -n "$task_wrapper"
"${task_cross}gcc" -O2 -funwind-tables "$task_src/test_rekordbox_unwind.c" -o "$task_tmp/driver" -ldl
ulimit -c 0
"$task_tmp/driver" "$task_tmp/hook.so" "$task_slot" "$task_wrapper"
