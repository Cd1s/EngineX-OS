# Sourced by nspawn-boot inside its private mount namespace.
settings_copy_prepare() {
    mkdir -p "$W/data/t7-drive"
    mkdir -p "$W/data/enginex/settings-copy" "$W/data/enginex/ui"
    printf '# T7 exact receiver bytes\r\nwaveform=denon\r\nfour_deck_role=24\r\nfuture_local_key=keep\r\n' > "$W/data/enginex/ui/settings.conf"
    printf '# T7 exact scale bytes\nENGINEX_UI_SCALE=100\n' > "$W/data/enginex/ui-scale.env"
    chmod 600 "$W/data/enginex/ui/settings.conf" "$W/data/enginex/ui-scale.env"
    if [ -n "${NSPAWN_SETTINGS_COPY_SUITE:-}" ]; then
        mkdir -p "$W/data/t7-bin"
        cp "$NSPAWN_SETTINGS_COPY_SUITE" "$W/data/t7-bin/enginex-suite"
        chmod 755 "$W/data/t7-bin/enginex-suite"
        cat > "$R/usr/local/bin/stream-sync" <<'WRAPPER'
#!/bin/sh
if [ "${1:-}" = --settings-copy-prestart ] && [ "$(cat /data/t7-flow-stage 2>/dev/null)" = undo ]; then
    cp /data/enginex/settings-copy/undo.json /data/t7-undo-snapshot.json
    /data/t7-bin/enginex-suite stream-sync "$@" || exit "$?"
    mkdir -p /data/t7-prestart-files/enginex/settings-copy
    cp -a /data/enginex/settings-copy/EngineX-Settings /data/t7-prestart-files/enginex/settings-copy/
    mkdir -p /data/t7-prestart-files/AppDataUser/'AIR Music Technology'/Engine /data/t7-prestart-files/enginex/ui
    cp -a /data/AppDataUser/'AIR Music Technology'/Engine/State /data/t7-prestart-files/AppDataUser/'AIR Music Technology'/Engine/
    [ ! -d /data/.config ] || cp -a /data/.config /data/t7-prestart-files/
    [ ! -f /data/enginex/ui/settings.conf ] || cp -a /data/enginex/ui/settings.conf /data/t7-prestart-files/enginex/ui/
    [ ! -f /data/enginex/ui-scale.env ] || cp -a /data/enginex/ui-scale.env /data/t7-prestart-files/enginex/
    exit 0
fi
exec /data/t7-bin/enginex-suite stream-sync "$@"
WRAPPER
        cp "$REPO/rootfs-overlay/etc/enginex/engine-ui-guard" "$R/etc/enginex/engine-ui-guard"
    fi
    if [ -n "${NSPAWN_SETTINGS_COPY_HOOK:-}" ]; then
        cp --remove-destination "$NSPAWN_SETTINGS_COPY_HOOK" "$R/etc/enginex/libengine-ui-hook.so"
        cp --remove-destination "$NSPAWN_SETTINGS_COPY_HOOK" "$R/usr/local/lib/enginex/libengine-ui-hook.so"
    fi
    if [ -n "${NSPAWN_SETTINGS_COPY_PACKS:-}" ]; then
        mkdir -p "$W/data/t7-packs"
        cp -a "$NSPAWN_SETTINGS_COPY_PACKS/en" "$W/data/t7-packs/"
    fi
    if [ "${NSPAWN_SETTINGS_COPY_FLOW:-0}" = 1 ]; then
        cp "$REPO/tools/az01-packer/settings-copy-flow-probe.qml" "$R/opt/nspawn/settings-copy-probe.qml"
    else
        cp "$REPO/tools/az01-packer/settings-copy-probe.qml" "$R/opt/nspawn/settings-copy-probe.qml"
    fi
    cat > "$R/opt/nspawn/install-settings-probe" <<'SCRIPT'
#!/bin/sh
set -eu
mkdir -p /data/enginex/ui/packs/en/Skins/Common/Items
[ ! -d /data/t7-packs/en ] || cp -a /data/t7-packs/en/. /data/enginex/ui/packs/en/
cp /opt/nspawn/settings-copy-probe.qml /data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml
SCRIPT
    cat >> "$R/etc/systemd/system/engine.service.d/99-nspawn.conf" <<'UNIT'
ExecStartPre=/bin/sh /opt/nspawn/install-settings-probe
UNIT
    if [ "${NSPAWN_SETTINGS_COPY_FLOW:-0}" = 1 ]; then
        # /media is initialized during guest boot. Bind the disposable fixture
        # immediately before Engine, after the native setup scripts finish.
        python3 - "$R/usr/Engine/Scripts/engine" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1]); text = p.read_text()
line = next(line for line in text.splitlines() if '/usr/Engine/Engine -d0' in line)
setup = '\tmkdir -p /media/T7-USB\n\tgrep -q " /media/T7-USB " /proc/mounts || mount --bind /data/t7-drive /media/T7-USB\n\tt7_extra=\n\t[ "$(cat /data/t7-flow-stage 2>/dev/null)" = undo ] || t7_extra=-extraDriveDir=/media/T7-USB\n'
text = text.replace(line, setup + line.replace('/usr/Engine/Engine -d0', '/usr/Engine/Engine -d0 $t7_extra'))
p.write_text(text)
PY
    else
        sed -i 's@/usr/Engine/Engine -d0@/usr/Engine/Engine -d0 -extraDriveDir=/data/t7-drive@' "$R/usr/Engine/Scripts/engine"
    fi
}
settings_copy_observe() {
    local deadline=$((SECONDS + 70))
    until [ "$SECONDS" -ge "$deadline" ]; do
        guest /bin/journalctl -u engine.service --no-pager > "$W/t7-engine.log"
        if grep -aq "T7 FAIL|" "$W/t7-engine.log"; then break; fi
        if [ "${NSPAWN_SETTINGS_COPY_FLOW:-0}" = 1 ]; then
            [ ! -f "$W/data/t7-flow-done" ] || break
        else
            grep -aq 'T7 DONE' "$W/t7-engine.log" && break
        fi
        sleep 0.2
    done
    grep -a 'T7 ' "$W/t7-engine.log" | cut -c1-170
    find "$W/data/t7-drive" -maxdepth 3 -type f > "$W/t7-drive-files.txt"
    cat "$W/t7-drive-files.txt"
    if [ "${NSPAWN_SETTINGS_COPY_FLOW:-0}" != 1 ] && grep -aq 'T7 DONE' "$W/t7-engine.log"; then
        cmp "$W/data/t7-native-before.profile" "$W/data/t7-native-after.profile"
        echo 'T7 NATIVE_PROFILE_BYTES_EQUAL'
    fi
    if [ -f "$W/data/t7-drive/Engine Library/user.profile" ]; then
        cp "$W/data/t7-drive/Engine Library/user.profile" "$W/user.profile"
    fi
    if [ "${NSPAWN_SETTINGS_COPY_FLOW:-0}" = 1 ]; then
        [ -f "$W/data/t7-flow-done" ] && ! grep -aq 'T7 FAIL|' "$W/t7-engine.log" || return 1
        python3 - "$W/data" <<'PYPROOF'
import base64, hashlib, json, sys
from pathlib import Path
root=Path(sys.argv[1]); snap=json.loads((root/'t7-undo-snapshot.json').read_text())
files=(snap['nativeFiles'] or {}) | {'enginex/ui/settings.conf':snap['settings'], 'enginex/ui-scale.env':snap['scale']}
files['enginex/settings-copy/EngineX-Settings/Engine Library/user.profile']=snap['profile']
for name, before in files.items():
    path=root/'t7-prestart-files'/name
    assert path.exists()==before['present'], name+' presence'
    if before['present']:
        data=path.read_bytes()
        assert data==base64.b64decode(before['bytes']), name+' bytes'
        assert path.stat().st_mode & 0o777==before['mode'], name+' permissions'
        print('T7 PRESTART_BYTES_EQUAL', name, hashlib.sha256(data).hexdigest())
print('T7 PRESTART_SNAPSHOT_EXACT',len(files),'files including absence')
PYPROOF
    else
        grep -aq 'T7 DONE' "$W/t7-engine.log" && ! grep -aq 'T7 FAIL|' "$W/t7-engine.log"
    fi
}
