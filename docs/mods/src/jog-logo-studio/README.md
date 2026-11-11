# Jog Logo Studio 1.1.0

Official MIT-licensed Mod inspired by
[#137](https://github.com/Cd1s/EngineX-OS/issues/137) (static Go helper in
1.1.0 for [#208](https://github.com/Cd1s/EngineX-OS/issues/208)).
The decks have no Python, compiler, or package manager: this Mod ships a static
Go helper (`bin/enginex-mod-helper-armv7` for `linux/arm` `GOARM=7` on all
gen-1 and gen-2 armhf decks, and `bin/enginex-mod-helper-arm64` for PRIME 4 G2
/ SYSTEM ONE), picked by busybox `sh` `start.sh` via `uname -m`. Requires
Linux, an existing `ffmpeg` executable on the deck, and QtQuick 2.12 / Controls
2.5 / Layouts 1.12. It never installs dependencies. Uploading 1.1.0 in the web
console replaces an installed 1.0.0 in place (keeping its enabled state and
settings).

The Mod loader calls `sh start.sh start` / `stop`; manual foreground usage is
`sh start.sh serve`. The QML panel connects to `127.0.0.1:17835` and must be
loaded by the host from `EngineXMods/jog-logo-studio/Panel.qml`.

Enter a source image path under `/media` or `/mnt`, then center crop. ffmpeg
creates an actual 600×600 `logo.png`. Enter media-volume paths, enable custom
logo and optionally synchronize all listed volumes. With sync disabled only the
first path is used. Each volume must already contain `Engine Library`.

Applying preserves the original logo as `.enginex-logo-original.png` plus an
ownership marker, then atomically replaces `Engine Library/logo.png`. Disabling
restores the original, or removes only this Mod's logo if no original existed.
Backup/target symlinks are rejected. Multi-drive writes are sequential: if a
drive is removed or fails, earlier drives may already be updated; reconnect and
retry or disable those drives. Removing the Mod does not modify detached media.
State: `/data/enginex/mods-state/jog-logo-studio`, override parent using
`ENGINEX_MOD_STATE`.

**Reload the media profile on each player to display the new logo.** The path,
format and size follow the
[Engine DJ community guide](https://community.enginedj.com/t/how-to-add-custom-artwork-or-logo-to-jog-wheel-display/14033).
Devices without jog displays cannot display a jog logo. Hardware/model-specific
display behavior is not validated; "All 16 Models" is an installation target.

Local API: `GET /status`; JSON `POST /action` with
`{"action":"crop","source":"/media/USB/image.jpg"}` or
`{"action":"apply","volumes":["/media/USB"],"sync":false,"enabled":true}`.
