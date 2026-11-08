# Standalone DMX (#134)

The lighting engine is built into stream-sync's Go WebUI server. It does not
need the Python dmx-studio Mod. Leave that Mod disabled when using this engine.
The default is off: no DMX worker, timer, socket, USB open, or loaded QML component.
No service or SoundSwitch property is changed.

Open the authenticated Web Console, then **DMX Lighting** (`/dmx`). Stop
SoundSwitch before enabling standalone DMX. Add outputs and fixture patches,
set intensity, choose a color/effect, and apply. Universe and fixture addresses
start at 1. Art-Net's wire Port-Address is one less than the UI universe.

Outputs are Art-Net UDP 6454 and E1.31/sACN UDP 5568, at 30 Hz with 512 slots.
Each route selects its destination IPv4 address and universe. For multicast
sACN, enter `239.255.H.L`, where H/L are the high/low bytes of the universe.
Up to eight outputs and 128 non-overlapping fixture patches are supported.
The profiles match the generic channel order in dmx-studio 1.0.0; they are
not manufacturer personalities. Check the fixture manual first.

Linux USB routes are optional. Select an FTDI/OpenDMX `/dev/ttyUSB*` or
`/dev/ttyACM*` node, or a uDMX bus/address. uDMX validates VID/PID 16c0:05dc
before vendor control requests. Missing or inaccessible USB devices are
reported in status and skipped while other routes continue. FTDI uses raw
250000 baud 8N2 and a break; uDMX uses usbfs. Neither needs Python/libusb.
Actual USB hardware timing remains to be verified on the deck. To pick up an
adapter plugged in later, disable and re-enable DMX; after replugging, check its
device node or bus/address.

Sync uses the selected master deck's OriginalBPM adjusted by SpeedState pitch
and fractional BeatNumber, with timestamped extrapolation between samples.
The bar is unwrapped for effects longer than four beats. Pause freezes phase;
master changes adopt the new phase. Samples older than one second cause full
blackout. Turn sync off to use manual BPM. This works outside four-deck mode:
the independent QML samples the same cached Engine translators at 100 ms.
Existing `/v1/sc5000/decks` posts also feed the clock when that mode is active.

After opting in, a **DMX Lighting** button appears at the bottom right of the
deck. The standalone pure QtQuick file loads from the loopback HTTP endpoint;
it adds no qrc node. It offers blackout/resume, master dimmer, four movement
effects and color palettes. Fixture patching and intensity live in the Web
Console. The three-language strings use the existing i18n.tsv dictionary.

Blackout sends zero on every slot. Disabling, reconfiguring routes, or closing
the engine sends three zero frames (sACN includes Stream_Terminated) and closes
all adapters. Live color/dimmer/effect changes preserve sockets; speed and manual BPM changes
can jump the movement phase, matching the original plugin.
Restart retains the opt-in and patch but restores blackout. SoundSwitch's
presence prevents activation; if it starts later, standalone output stops
within one second. It never stops or reconfigures SoundSwitch.

`GET/POST /api/dmx` uses the console's existing authentication and origin rules.
`GET/POST /v1/sc5000/dmx`, `GET /v1/sc5000/dmx.qml`, and
`POST /v1/sc5000/dmx/clock` require direct loopback requests through the streaming
gateway (a literal loopback Host and no forwarding headers).
Web POST actions are `configure` (with the complete `config` and `revision` from
GET), `control` (a partial `control` object), `blackout`, and `disable`. Stale
full configurations are rejected with 409. Native POST is performance-only:
`control` accepts blackout, master, effect and RGB; it cannot edit routes,
patches or the opt-in. Blackout/disable remain effective if persistence fails.
The clock accepts the existing array of deck-state objects.
Configuration is atomically stored in `ui/dmx.json`.

## Two SC5000 verification (PM deployment only)

1. On deck B or another reachable capture host (including Batam), run:
   `tcpdump -ni any -s0 -XX 'udp port 6454 or udp port 5568'`.
   With DMX disabled on deck A, confirm no DMX output from A.
2. In A's Web Console, create an Art-Net route to B/capture host, universe 1,
   and an LED Par at address 1. Start with low intensity, set master to full,
   choose a color, enable DMX, and apply. Expect 530-byte ArtDMX UDP payloads,
   `Art-Net\0`, OpDmx 00 50, revision 00 0e, Port-Address 00 00 and length
   02 00. Slots start at byte 18 and follow dimmer/R/G/B.
3. Repeat with sACN. Expect 638-byte payloads, `ASC-E1.17` at byte 4,
   priority 100 at byte 108, universe 00 01 at bytes 113–114, null start code
   at byte 125 and slots at byte 126. Both protocols should average 30 Hz.
4. Patch a generic Moving Head Beam in a separate universe, or use its slots
   solely for capture. Set Circle, four beats/cycle. At 120 BPM a cycle takes
   two seconds; at +10% pitch it takes about 1.818 seconds (132 BPM). Pause,
   resume, cue/loop and switch master between decks; inspect pan/tilt bytes
   against the master beat. Repeat with four-deck mode off and on.
5. On A, check the native button, languages, colors, dimmer and blackout.
   Blackout must zero all 512 slots. Disable and confirm three final zero
   frames, then no more output. Stop clock delivery for over one second and
   confirm automatic blackout. Restart stream-sync through the PM's normal
   deployment procedure and confirm the saved patch returns blacked out.
6. With SoundSwitch running, confirm standalone enable is refused and stock
   lighting remains usable. With no USB adapter connected, add an absent USB
   route alongside the network route; network output must continue.

## Limits and uncertainties

No deck deployment is performed by this task. Native HTTP QML loading/touch
placement, Engine master selection, receiver interoperability, and FTDI/uDMX
electrical timing still require hardware verification. Process detection uses
the `SoundSwitch` process name and may need adjustment if firmware runs a
resident but inactive host under that name. This MVP does not implement vendor
fixture libraries, smoke arming/flash/timers, show sequencing, or pixel mapping;
the generic Smoke profile deliberately emits zero.
