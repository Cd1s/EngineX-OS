# Generic MIDI Controller Mapper (`midi-controller-mapper`)

Maps external USB MIDI controllers (targeting the **Pioneer CDJ-400** in USB `PC MODE` first) directly to EngineX OS internal decks (`/Engine/Deck3/...` or `/Engine/Deck4/...` in SC5000 4-deck mode) without switching hardware layers or contending with the player's physical top panel.

## Architecture & Reverse-Engineered Findings

1. **Why Native `UserAssignmentPath` / `MidiAssignment` Cannot Load Third-Party USB Controllers Directly**:
   - In `Engine` 5.3.0 (`0x3c1240` / `0x15a3160`), `UserAssignmentPath` is hardcoded to `""` and `PresetPath` is read-only.
   - `DeviceManager` (`0x15d326c` / `0x165a268`) filters ALSA MIDI clients against `KnownDevices.vfsb` (`0x28e3d00`), requiring a matching ALSA sound card sysfs entry and a Denon manufacturer SysEx Universal Device Inquiry reply (`F0 7E .. 06 02 00 02 0B .. F7`).
   - `JogOutput.qml` (`airAssignments 1.0`) processes single MIDI CC packets as discrete pulses and has no continuous time-step integrator (`dt`) for the CDJ-400's continuous velocity jog protocol (`Bn 10`, where `64 = stop` and non-64 values continue moving at constant velocity until `64` arrives).
2. **Low-Priority ALSA Sequencer Helper + 60 Hz StateMap Bridge**:
   - `feature.py` runs at `os.nice(19)` and opens `libasound.so.2` (`snd_seq_open`) to subscribe to the external USB MIDI controller AND to `System:Announce` (`0:1`) for instant kernel hot-unplug events (`SND_SEQ_EVENT_CLIENT_EXIT` / `SND_SEQ_EVENT_PORT_EXIT`).
   - Decodes raw MIDI events via `snd_midi_event_decode`, integrates continuous jog velocity (`Δpos = (velocity / 63.0) * dt * sensitivity`) at 60 Hz (`16 ms` poll only while the platter is moving; `0%` CPU when idle), and coalesces property updates to `/Engine/Deck{N}/...`.

## Pioneer CDJ-400 Controls (`presets/pioneer-cdj-400.json`)

| Control | MIDI Message | Engine StateMap Target / Action |
|---|---|---|
| Jog Platter Touch | `9n 20` (`7F` touch / `00` release) | Engages Vinyl touch scratch (pauses playback while held, resumes on release) |
| Jog Wheel Scratch / Turn | `Bn 10` (`<64` rev, `64` stop, `>64` fwd) | Continuous velocity integrator `Δpos = (v/63)·dt·sens` → `/Engine/DeckN/Track/SetSongPosition` |
| Jog Outer Ring Pitch Bend | `Bn 30` (`<64` rev, `64` stop, `>64` fwd) | `/Engine/DeckN/Track/NudgeBackward` / `NudgeForward` |
| Tempo Slider | `Bn 1D` (`00..7F`, `40` = 0%) | `/Engine/DeckN/Speed` with Soft Takeover (`SpeedOffsetUp` ▲, `SpeedOffsetDown` ▼, `SpeedNeutral` ●) |
| Play / Pause | `9n 00` | `/Engine/DeckN/Play` |
| Cue | `9n 01` | `/Engine/DeckN/Track/CueAction` |
| Search `<<` / `>>` | `9n 03` / `9n 02` | `/Engine/DeckN/Track/SeekBackward` / `SeekForward` |
| Track Search `|<<` / `>>|` | `9n 05` / `9n 04` | `/Engine/DeckN/Track/PreviousTrack` / `NextTrack` |
| Loop In / Out / Reloop | `9n 06` / `9n 07` / `9n 08` | `/Engine/DeckN/Track/LoopIn`, `LoopOut`, `Reloop` |
| Loop Divide `1/2X` / Double `2X` | `9n 16` / `9n 17` | `/Engine/DeckN/Track/LoopHalveDouble` (`decValue` / `incValue`) |
| Tempo Range (`±6/10/16/100%`) | `9n 10` | Cycles `/Engine/DeckN/SpeedRange` |
| Master Tempo (Key Lock) | `9n 11` | `/Engine/DeckN/Track/KeyLock` |
| Jog Mode (Vinyl / CDJ) | `9n 12` | Toggles Vinyl scratch mode and `/Engine/DeckN/ExternalScratchWheelScratchEnable` |
| Beat Sync | `9n 13` | `/Engine/DeckN/Sync` / `UnSync` |
| Rotary Selector (Turn / Push / Back) | `Bn 4F` / `9n 32` / `9n 33` | `/Qml/ControlFocus/Increment`/`Decrement`, `/Client/Librarian/LoadToDeckN`, `/GUI/ShowBrowseView` |

## Hardware LED Feedback Limitation

In standard USB MIDI mode (`PC MODE`), the **Pioneer CDJ-400** is transmit-only for button/LED states and does not illuminate hardware LEDs in response to external MIDI Note/CC messages. Deck playback status, Vinyl/CDJ jog mode, and soft-takeover **`▲ UP` / `▼ DN` / `● NEUTRAL`** indicators are displayed directly on the SC5000 screen. The preset schema (`ledFeedback`) includes optional LED mapping definitions for controllers that support incoming MIDI LED feedback.
