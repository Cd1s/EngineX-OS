# Minimal Club Mode 1.1.0

Official MIT-licensed, reversible optional-service pause for
[#136](https://github.com/Cd1s/EngineX-OS/issues/136) (static Go helper in
1.1.0 for [#208](https://github.com/Cd1s/EngineX-OS/issues/208)).
The decks have no Python, compiler, or package manager: this Mod ships a static
Go helper (`bin/enginex-mod-helper-armv7` for `linux/arm` `GOARM=7` on all
gen-1 and gen-2 armhf decks, and `bin/enginex-mod-helper-arm64` for PRIME 4 G2
/ SYSTEM ONE), picked by busybox `sh` `start.sh` via `uname -m`. Requires
Linux, `systemctl` (present on the deck) with service-management permissions,
and QtQuick 2.12 / Controls 2.5 / Layouts 1.12. No dependencies are installed.
Uploading 1.1.0 in the web console replaces an installed 1.0.0 in place
(keeping its enabled state and settings).

Loader lifecycle: `sh start.sh start` / `stop`; manual foreground usage:
`sh start.sh serve`. QML connects to `127.0.0.1:17837`; host component is
`EngineXMods/minimal-club-mode/Panel.qml`. Starting this Mod only starts its
control helper; **club mode itself requires an explicit button press**.

Enable records which optional units are currently active before stopping them:

- enginex-deemixd.service (5.3.0.5 and later)
- sc5000-deemixd.service, enginex-sc5000-deemixd.service (earlier releases)
- stems-processor.service
- icecast-stream.service
- enginex-download-queue.service
- enginex-streaming-sync.service

Absent units are skipped. No wildcard service matching, process-name killing,
permanent masks, network shutdown or audio scheduling changes. Engine, audio,
rb-bridge and Pro DJ Link are never stopped. **The combined stream-sync service
remains running**, because it also hosts the web controls and bridge-related
functionality. Downloads inside that shared process cannot safely be paused by
this package alone; only separate optional units are paused. This is a scoped
implementation, not a claim of complete #136 acceptance or 100% available CPU.

Disable restores only services recorded as active. Failed stops trigger rollback;
failed restores retain a retryable `restore-needed` record. State is kept at
`/data/enginex/mods-state/minimal-club-mode/paused.json` (override parent with
`ENGINEX_MOD_STATE`). A helper restart retains intent. Disabling/uninstalling the
Mod stops its helper and runs a one-shot restore; incomplete restores return an
error rather than discarding the record. Stopping foreground execution alone
does not automatically resume optional services; use disable or lifecycle stop.

Local API: `GET /status`; JSON `POST /action` with `{"action":"enable"}` or
`{"action":"disable"}`. Real device service naming, CPU/RAM savings and sustained
USB/SD/Rekordbox/Pro DJ Link performance are **not hardware-validated**.
