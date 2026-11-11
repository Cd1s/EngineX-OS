# DMX Studio 1.1.0

Official, source-included Mod for EngineX OS. MIT licensed. Community requests:
[#134](https://github.com/Cd1s/EngineX-OS/issues/134),
[#137](https://github.com/Cd1s/EngineX-OS/issues/137), and
[#208](https://github.com/Cd1s/EngineX-OS/issues/208).

## Runtime

The decks have no Python, compiler, or package manager (#208). Requires Linux,
busybox `sh`, QtQuick 2.12 / Controls 2.5 / Layouts 1.12, and a compatible USB
or network interface. The bundled helper is a static Go binary
(`bin/enginex-mod-helper-armv7` for `linux/arm` `GOARM=7` on all gen-1 and
gen-2 armhf decks, and `bin/enginex-mod-helper-arm64` for PRIME 4 G2 / SYSTEM
ONE), selected in `start.sh` via `uname -m`. No dependencies are installed.
Uploading 1.1.0 in the web console replaces an installed 1.0.0 in place (keeps
its enabled state and settings).
The loader invokes `sh start.sh start` / `sh start.sh stop`. Start detaches a
`nohup` helper and checks readiness; stop verifies the saved PID's command line
before sending SIGTERM. Manual foreground use: `sh start.sh serve`.
The QML panel connects only to `127.0.0.1:17834`; it is not a remote web server.
The host UI must load `EngineXMods/dmx-studio/Panel.qml` from the Mod overlay.
State: `/data/enginex/mods-state/dmx-studio/show.json` (override the state parent
with `ENGINEX_MOD_STATE`). Restarts always stop outputs and disarm smoke.

## Patch and output

Outputs JSON example (internal universes are **1-based**, Art-Net wire addresses
are 0-based; sACN wire universes retain the internal value):

```json
[
  {"protocol":"artnet","universe":1,"host":"192.168.1.50"},
  {"protocol":"sacn","universe":2,"host":"239.255.0.2"},
  {"protocol":"ftdi","universe":3,"device":"/dev/ttyUSB0"},
  {"protocol":"udmx","universe":4,"bus":1,"address":2}
]
```

FTDI/OpenDMX uses an existing Linux tty driver and Linux termios2 at 250000 baud,
8N2, 120 µs break + 12 µs mark-after-break, sending a 513-byte frame (~38 Hz,
no audio impact, validated on a Mixstream Pro+ NH08S in #208). uDMX selects
VID/PID `16c0:05dc` by USB bus/address via Linux `usbfs` directly (no `libusb`
needed; requires device permissions). At most 8 outputs and 128 fixtures are
accepted; USB devices cannot be assigned twice. Both network protocols send
512-channel frames, nominally at 30 Hz. Timing is best effort in a non-realtime
static Go helper process and must be measured on each target.

Patch example:

```json
[
  {"profile":"Moving Head 3-in-1","universe":1,"address":1,"x":0.2,"y":0.5,"gobo":0,"beam":0,"mode":0},
  {"profile":"LED Par","universe":1,"address":30,"x":0.8,"y":0.5},
  {"profile":"Smoke","universe":1,"address":100,"x":0.5,"y":0.8}
]
```

Fixtures are generic templates, **not verified vendor personalities**. The
`profiles` status field lists the actual channel order. Check and adapt
`feature.go:Profiles` to the fixture manual before output; do not guess mode,
strobe, smoke, gobo or beam channel semantics. Patch overlap and channel overflow
are rejected. Stop output before changing routes or patch.

Circle, 8-Figure and Pan-Tilt Sweep move `speed` beats per cycle (default 4).
With **Follow MASTER deck beat** on (the default), the panel reads the MASTER
deck's Engine `BeatNumber`, `CurrentBPM` and `TrackPosition` ten times a second
and posts them with their read time as `{"action":"clock","deck":1..4,
"playing":bool,"beat":n,"bpm":n,"position":n,"at":unixMs}` (#209). The helper
keeps this clock in memory only (it never writes `show.json`), extrapolates
between updates, takes phase errors below half a frame of slewing at once and
slews larger ones (a nudge, a seek, a MASTER handoff to an unsynced deck) in at
half the tempo instead of jumping. A paused MASTER holds the look; with no deck
playing (`"deck":0`) or no update for 2 s, the manually entered BPM takes over
from the current phase. A playing non-MASTER deck is followed when the MASTER
is paused. The clock state is in `/status` as `clock`. Pan/tilt
center and width/height can be set through `/action` configuration. RGB and
intensity apply to the generic fixtures. The 2D canvas is a diagnostic preview,
not a calibrated beam simulation; the monitor shows all 512 values per universe.

Smoke requires an explicit Smoke fixture, arming and an applied output level.
Interval/duration use seconds; duration 0 disables scheduled smoke while leaving
momentary flash available. SMOKE FLASH renews a 0.5s lease while held; losing the
UI expires the lease. Scheduled smoke remains active while explicitly armed.
Blackout, helper termination and output errors attempt zero frames on all routes
and disarm smoke. Hardware behavior after cable loss is fixture-specific.

## HTTP API and verification boundary

Local `GET /status`; `POST /action` with JSON:
`{"action":"configure","config":{...}}`, `{"action":"start"}`,
`{"action":"stop"}`, `{"action":"flash","held":true}`.
Only local Host headers / QML origins are accepted. No shell evaluation.

Packet layout references (independent implementation, no upstream code copied):
[Art-Net implementation](https://github.com/OpenLightingProject/ola/blob/master/plugins/artnet/ArtNetNode.cpp),
[ETC E1.31 layout](https://github.com/ETCLabs/sACN/blob/main/src/sacn/private/pdu.h).
FTDI/OpenDMX output was validated on a Mixstream Pro+ NH08S in #208 (~38 Hz, no
audio impact); uDMX, Art-Net/sACN fixtures and all remaining model variants
remain **unverified on hardware**. Do not infer full hardware validation from
manifest validation or registry model targeting.
