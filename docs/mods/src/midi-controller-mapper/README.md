# Generic MIDI Controller Mapper (`midi-controller-mapper`) 1.1.0

Maps external USB MIDI controllers (targeting the **Pioneer CDJ-400** in USB `PC MODE` first) directly to EngineX OS internal decks (`/Engine/Deck3/...` or `/Engine/Deck4/...` in SC5000 4-deck mode) without switching hardware layers or contending with the player's physical top panel.

## Runtime

The decks have no Python, compiler, or package manager ([#208](https://github.com/Cd1s/EngineX-OS/issues/208)). Version 1.1.0 ships a static Go helper (`bin/enginex-mod-helper-armv7` for `linux/arm` `GOARM=7` on all gen-1 and gen-2 armhf decks, and `bin/enginex-mod-helper-arm64` for PRIME 4 G2 / SYSTEM ONE), picked by busybox `sh` `start.sh` via `uname -m`, keeping the same loopback HTTP contract (`127.0.0.1:17838`) and `Panel.qml` as before. Uploading 1.1.0 in the web console replaces an installed 1.0.0 in place (keeping its enabled state and settings).

## Architecture & Reverse-Engineered Findings

1. **Why This Mod Uses an ALSA Sequencer Helper + StateMap Bridge**:
   - In stock `Engine` 5.3.0 (`JP07`), the top-level `"Assignments"` constructor (`0x003c0570..0x003c18dc`) initializes `PresetPath` (`[r4 + 0x94..0x9c]`) from `/Client/Locations/AssignmentPath` (`0x00d07b34..0x00d08150` → `/usr/Engine/AssignmentFiles`) + `/PresetAssignmentFiles/` (`0x003c0d3c..0x003c0e90`, UTF-16LE literal at `0x01f61ef4`), and initializes `UserAssignmentPath` (`[r4 + 0xa0..0xa8]`, copied to per-device `AssignmentManager` `[r6 + 0xac]` at `0x015a6fd4..0x015a6ff8`) from the empty C-string `""` (`0x01ec32a0` at `0x003c1240..0x003c1268` and `0x003c1420..0x003c1444`). When `resolveAssignmentUrl` (`0x015a3160`) finds no local file in `UserAssignmentPath` or `PresetPath`, it falls back to `qrc:/airAssignments/DefaultAssignment.qml` (`0x015a37bc`), for which `QFileInfo(url.toLocalFile()).exists()` (`0x015a7ee4`, `0x015a8670`) is `false`, logging `Not starting MIDI thread for <name> because the assignments don't exist.` (`0x015a86c0..0x015a86e8`). Because a writable `/data` user-assignment directory is not active in stock `Engine` without custom VFS/QRC injection, native assignment loading is not confirmed usable as a standalone drop-in path.
   - In addition, stock `JogOutput.qml` (`airAssignments 1.0`) only implements stateless per-message modes (`type 0..3`) and has no continuous time-step integrator (`dt`) for the CDJ-400's continuous velocity jog protocol (`Bn 10`, where `64 = stop` and non-64 values keep rotating at constant velocity until `64` arrives).
2. **Low-Priority ALSA Sequencer Helper + On-Demand 60 Hz StateMap Bridge**:
   - The static Go helper runs at `nice 19` and talks to the Linux ALSA sequencer kernel interface directly (`/dev/snd/seq`, with no Python or `libasound` runtime dependency) to subscribe to the external USB MIDI controller (matched strictly by preset `match.alsaClientPatterns` or `match.usbIds` via `/proc/asound/cardN/usbid` in `auto` mode, never falling back to unrelated ALSA ports) and to `System:Announce` (`0:1`) for kernel hot-unplug events (`SND_SEQ_EVENT_CLIENT_EXIT` / `SND_SEQ_EVENT_PORT_EXIT`) matching the connected client/port.
   - When the Mod is disabled, both `EmbeddedMain.qml`'s `scMidiPollTimer` and `Panel.qml`'s repeating timer remain stopped (`running: false`), incurring zero background polling.
   - When enabled, the helper decodes ALSA sequencer MIDI events directly, integrates continuous jog velocity (`Δpos_sec = (velocity / 63.0) * dt * scratchSensitivitySecPerSec`, with `dt` capped at `0.5 s` across `250 ms` idle ticks), and coalesces updates to `/Engine/Deck{N}/...`.
   - **Empirical Jog Calibration (`26,325` ticks/s at 44.1 kHz output)**: In `Engine` 5.3.0, `ExternalScratchWheelMove` (`0x01f44060`, bound at `0x01aeb9c4..0x01aeba30` and handled at `0x00ef18fc` → `0x00ef1b00..0x00ef1ba4`) feeds the 33⅓ RPM (`33.3333320618` at `0x00ef24f8`) scratch interpolation queue rather than a static `.rodata` constant. Measured in the 5.3.0 sandbox at a **44,100 Hz output sample rate** on a paused 44,100 Hz track on Deck 3 (`pos0 = 1,323,000` samples = `30.000 s`), `ExternalScratchWheelMove.incValue(9600.0, 0.0, 0.0, false, true, 0.0)` advanced `TrackPosition.unnormalized` to `pos1 = 1,339,082` samples (`+16,082` samples = `+0.364671 s` at 44.1 kHz output, or `1.6752` samples/tick; at 48 kHz output the same call corresponds to `+17,500` samples, scaling sample displacement by `48000 / 44100`), yielding an empirical baseline of `9600 / (16082 / 44100) ≈ 26,325` ticks/s at `scratchSensitivitySecPerSec = 1.0`. `scratchSensitivitySecPerSec` in the preset serves as a user-tunable sensitivity multiplier around this baseline.

## Pioneer CDJ-400 Controls (`presets/pioneer-cdj-400.json`, verified against DRB1452 p.20)

| Control | MIDI Message (Hex / Dec) | Engine StateMap Target / Action |
|---|---|---|
| Jog Platter Touch | `9n 20` (Note 32 / `0x20`) | `/Engine/DeckN/ExternalScratchWheelScratchEnable` + `ExternalScratchWheelTouch` |
| Jog Wheel Scratch / Turn | `Bn 10` (CC 16 / `0x10`, `64` = stop) | Continuous velocity integrator `Δpos = (v/63)·dt·sens` → `/Engine/DeckN/ExternalScratchWheelMove` (fallback `Track/SetSongPosition`) |
| Jog Outer Ring Pitch Bend | `Bn 30` (CC 48 / `0x30`, `64` = stop) | `/Engine/DeckN/Track/NudgeBackward` / `NudgeForward` (sustained until `64` arrives) |
| Tempo Slider | `Bn 1D` (CC 29 / `0x1D`, `64` = 0%) | `/Engine/DeckN/Speed` (`setValue(v, 1, 0)` `HARDEXTERNAL`) with Soft Takeover (`SpeedOffsetUp` ▲, `SpeedOffsetDown` ▼, `SpeedNeutral` ●) |
| Play / Pause | `9n 00` (Note 0 / `0x00`) | `/Engine/DeckN/Play` |
| Cue | `9n 01` (Note 1 / `0x01`) | `/Engine/DeckN/Track/CueAction` |
| Search `>>` / `<<` | `9n 02` / `9n 03` (Note 2 / 3) | `/Engine/DeckN/Track/SeekForward` / `SeekBackward` |
| Track Search `>>|` / `|<<` | `9n 04` / `9n 05` (Note 4 / 5) | `/Engine/DeckN/Track/NextTrack` / `PreviousTrack` |
| Loop In / Out / Reloop | `9n 06` / `9n 07` / `9n 08` (Note 6 / 7 / 8) | `/Engine/DeckN/Track/LoopIn`, `LoopOut`, `Reloop` |
| Tempo Range (`±6/10/16/100%`) | `9n 10` (Note 16 / `0x10`) | Cycles `/Engine/DeckN/SpeedRange` |
| Master Tempo (Key Lock) | `9n 11` (Note 17 / `0x11`) | `/Engine/DeckN/Track/KeyLock` |
| Jog Mode (Vinyl / CDJ) | `9n 12` (Note 18 / `0x12`) | Toggles Vinyl scratch mode and `/Engine/DeckN/ExternalScratchWheelScratchEnable` |
| Loop Divide `1/2X` / Double `2X` | `9n 16` / `9n 17` (Note 22 / 23) | `/Engine/DeckN/Track/LoopHalveDouble` (`decValue` / `incValue`) |
| Hold (Beat Sync) | `9n 22` (Note 34 / `0x22`) | `/Engine/DeckN/Sync` / `UnSync` |
| Back (Toggle Browse View) | `9n 32` (Note 50 / `0x32`) | `/GUI/ShowBrowseView` |
| Encoder Push (Load Track) | `9n 33` (Note 51 / `0x33`) | `/Client/Librarian/LoadToDeckN` |
| Rotary Selector (Browse Scroll) | `Bn 4F` (CC 79 / `0x4F`) | `/Qml/ControlFocus/Increment` / `Decrement` |

## Hardware LED Feedback Limitation

In standard USB MIDI mode (`PC MODE`), the **Pioneer CDJ-400** is transmit-only for button/LED states and does not illuminate hardware LEDs in response to external MIDI Note/CC messages. Deck playback status, Vinyl/CDJ jog mode, and soft-takeover **`▲ UP` / `▼ DN` / `● NEUTRAL`** indicators are displayed directly on the SC5000 screen. The preset schema (`ledFeedback`) includes optional LED mapping definitions for controllers that support incoming MIDI LED feedback.
