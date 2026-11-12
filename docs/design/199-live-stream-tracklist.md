# Design Note: One-Tap Live Streaming & Automatic Set Tracklist (#199)

## 1. Problem

DJs need two distinct workflows depending on hardware architecture:

1. **All-in-one consoles (`PRIME 4 / 4+ / 2 / GO / GO+`, `SC LIVE 4 / 2`, `Mixstream`, `AZ04` `PRIME 4 G2 / SYSTEM ONE`)**: the unit mixes all channels internally and has the master bus. A single switch should stream the master output to Icecast/Shoutcast (and optionally save a local recording for Mixcloud upload) while writing an accurate timestamped tracklist of tracks that were **actually audible** on the master bus (playing, channel fader up, and crossfader not cutting that channel). Starting/stopping the stock Engine recorder with `Auto Tracklist` enabled should likewise capture an aligned tracklist.
2. **Standalone media players (`SC5000 / SC5000M / SC6000 / SC6000M`, boards `JP07 / JP08 / JP13 / JP14`)**: each player outputs individual deck layers to an external mixer and has **no internal master mix**. Audio streaming from the player itself is impossible and must be stated clearly in the UI and documentation. Instead, linked standalone players should produce **one shared, merged timestamped tracklist** across both units via the EngineX peer link (`#49` / `#201`), using a networked Denon `X1800 / X1850` mixer over StageLinQ (or Pioneer DJM over Pro DJ Link, or sustained play duration when using an analog mixer) to determine what is on air, visible and downloadable from the `:8080` web console of either player as plain text, CUE sheet, Mixcloud tracklist, or JSON.

## 2. Stock Capability Check

- **Stock Recorder (all-in-ones)**: `Config.audio.hasRecorder` is true on consoles. Stock Engine writes WAV files under `<drive>/Sessions/` and exposes `/Engine/Mixer/Recorder/Active` (`bool`) and `/Engine/Mixer/Recorder/RecordTime` (`string`), with streaming-track blocks already lifted by `#206`. Stock Engine does **not** write `.cue`, `.txt`, or Mixcloud tracklists alongside recordings.
- **Stock History (`hm.db`)**: Engine's `Historylist` / `HistorylistEntity` tables store local `trackId` + wall-clock `startTime` per unit only, omit peer tracks and set-relative offsets, and must never be written (or contended with) externally while Engine holds the library open (`CONTRIBUTING.md`).
- **Stock Engine Mixer StateMap (all-in-ones)**: Engine exposes `/Engine/Mixer/Channel1..4/Volume/Level` (upfader `0..1`), `/Engine/Mixer/Channel1..4/Crossfader/PostCrossfadeLevel` (Engine's computed post-crossfade gain `0..1` incorporating crossfader position, curve, reverse, `EnableXfader`, and channel A/B/Thru assignment), `/Engine/Mixer/EnableXfader`, and `/Engine/Mixer/CrossfaderPosition`.
- **Standalone Player External Mixer State**: Engine exposes `/Engine/Deck1..4/ExternalMixerVolume` when Engine Connect is paired, Denon `X1800 / X1850` mixers announce on StageLinQ (`UDP 51337` `"airD"` + TCP `"StateMap"` `"smaa"`), and Pioneer DJM mixers broadcast On-Air packets (`UDP 50001` type `0x03`, already parsed by `pkg/prolink.ParseDJMOnAirPacket`).
- **Existing EngineX Broadcast (`#50`, `#108`)**: `pkg/broadcast` and `pkg/webui/broadcast.go` already provide `CPU3`-guarded Icecast/Shoutcast streaming, `/live.mp3`, `/live.wav`, and `Settings -> Services -> Live Stream` (`objectName: "ControlCenter_LiveStream"`). Note that `release-gate.sh` asserts `SCPROBE tile|ControlCenter_LiveStream|1|` across `JP07`, `JP13`, and `JC11`, so `ControlCenter_LiveStream` must remain visible on all models.

## 3. Options Considered

1. **Read Engine's `hm.db` SQLite history during the set**:
   - Rejected: risks SQLite lock contention on slow FAT32/exFAT drives, lacks sub-second relative timestamps, misses unimported/peer tracks, and depends on Engine's fixed history threshold rather than live mixer audibility.
2. **Track audibility purely in QML on each unit without backend peer merge**:
   - Insufficient for standalone players: two SC5000/SC6000 units with an external X1800/X1850 mixer need a single merged timeline shared across both players' `:8080` consoles and direct network mixer monitoring.
3. **Chosen — Dual-mode architecture (`pkg/tracklist` + `pkg/stagelinq` + `pkg/broadcast` + `pkg/webui` + QML console audibility)**:
   - Cleanly separates all-in-one master streaming + console mixer audibility from standalone player tracklist-only mode + peer-merged StageLinQ/DJM/duration on-air tracking.

## 4. Chosen Design

### 4.1 All-in-One Logic (`capabilities.mixer == true`)

- **One-Tap Control Center Tile & Settings Sync**:
  - Add a one-tap `Live Stream` tile (`objectName: "ControlCenter_OneTapLive"`, `visible: root.scHasMixer`) in `ControlCenterTab.qml` showing `OFF` or `LIVE · MM:SS`, synchronized with `Settings -> Services -> Live Stream` (`ControlCenter_LiveStream`) and `:8080`.
  - Toggling `Live Stream` on starts `pkg/broadcast` (Icecast/Shoutcast + `/live.mp3` + `/live.wav`, plus optional local set file recording `RecordSet` writing `<set>.wav` and companion `.txt` / `.cue` / `.mixcloud.txt` / `.json` sidecars) **and** opens an automatic timestamped `tracklist.Session`.
  - Add `Auto Tracklist` (`auto_tracklist`, default `0` = Off) in `Settings -> Services` and `:8080`: when enabled on an all-in-one, starting/stopping the stock Engine recorder (`/Engine/Mixer/Recorder/Active`) also automatically opens/closes a timestamped tracklist session aligned to `00:00` of the recording and writes companion export files to `/data/enginex/state/tracklists/` (and alongside writable media `Sessions/` when present).
- **Audible ("Played") Rule from Engine's Own Mixer State**:
  - Exclude shadow slots (`scShadowIsShadowSlot(d)`).
  - Deck `d` (`1..4`) is audible iff `/Engine/Deck{d}/PlayState == true` (true playback, excluding held CUE preview) **and** `/Engine/Mixer/Channel{d}/Volume/Level > 0.05` **and** `/Engine/Mixer/Channel{d}/Crossfader/PostCrossfadeLevel > 0.05` (falling back to `/Engine/Mixer/EnableXfader` + `/Engine/Mixer/CrossfaderPosition` if `PostCrossfadeLevel` is unavailable).
  - Continuous audibility is debounced (`1.5 s`) to ignore momentary scratch cuts while backdating the recorded `OffsetSec` to the exact instant the channel became audible (`±1 s` accuracy).
  - Updates Icecast/Shoutcast metadata (`UpdateMetadata`), appends to the active tracklist session, updates `/live/nowplaying.txt` (OBS text source), and sets `streamingNotice: true` when a streaming-service track plays during an active broadcast.

### 4.2 Standalone Player Logic (`capabilities.mixer == false`: `SC5000 / SC5000M / SC6000 / SC6000M`)

- **Explicit UI & Documentation**:
  - `GET /api/broadcast` and `GET /api/tracklist` report `audioSupported: false` (`mode: "tracklist_only"`) on `JP07 / JP08 / JP13 / JP14`.
  - On the deck (`Settings -> Services`) and in `:8080`, standalone players display a clear notice that the player has no internal master mix (no audio stream from the player itself) and that toggling the switch records a **shared set tracklist** across linked players (`ControlCenter_LiveStream` remains visible so all release gates stay green).
- **StageLinQ X1800 / X1850 Mixer Monitor (`pkg/stagelinq`, MIT clean-room Go)**:
  - Passively listens on UDP `51337` for `"airD"` (`DISCOVERER_HOWDY_`) announcements from Denon mixers (`X1800`, `X1850`, or devices advertising `"StateMap"` mixer paths), connects to the advertised TCP directory port to look up `"StateMap"`, and subscribes (`"smaa"` type `0x000007d2`) to `/Mixer/CH1faderPosition`..`/Mixer/CH4faderPosition`, `/Mixer/ChannelAssignment1`..`4`, and `/Mixer/CrossfaderPosition`.
  - Computes `ChannelOnAir(ch int) (onAir bool, known bool)` from channel upfader (`> 0.05`) and crossfader side assignment (`Thru`, `A`, `B` vs `CrossfaderPosition`).
- **Merged Two-Player Tracklist & On-Air Priority**:
  - Every `1 s` while a tracklist session is active on a standalone player, `stream-sync` inspects **both** this player's physical decks (`s.shadow.own`, never shadow copies) and the linked partner player's physical decks (`s.shadow.peerDecks`).
  - Channel mapping: uses the player/layer's assigned mixer channel (`1..4` from `four_deck_role` / player number + Layer A/B).
  - On-air decision priority:
    1. Denon StageLinQ mixer `ChannelOnAir(ch)` (or `/Engine/Deck{d}/ExternalMixerVolume` carried in `deckState` when Engine Connect is active): `1.5 s` debounce, backdated to audibility onset.
    2. Pioneer DJM Pro DJ Link On-Air (`UDP 50001` `0x03`, `fresh < 5 s`): `1.5 s` debounce, backdated to onset.
    3. Analog / unmonitored mixer fallback: continuous `Playing && !Cue` for `>= minPlaySec` (default `10 s`, configurable), backdated to playback start (`now - minPlaySec`).
  - **Bidirectional Peer Sync (`POST /api/tracklist/peer-sync`)**: session start, stop, and newly committed entries replicate automatically to active EngineX peers (`s.shadow.peers`) over LAN HTTP, deduplicated by `(sessionID, entryKey)` and ordered by `OffsetSec`, so `:8080` on **either** player always displays and exports the identical merged tracklist (`Player 1 · Deck 1`, `Player 2 · Deck 2`, etc.).

### 4.3 Export Formats (`pkg/tracklist`)

- **Plain Text (`.txt`)**: `HH:MM:SS  Artist - Title  [Player 1 Deck 1]`
- **CUE Sheet (`.cue`)**: standard `PERFORMER`, `TITLE`, `FILE`, `TRACK NN AUDIO`, `INDEX 01 MM:SS:FF` (escaping quotes, 75 fps CUE index)
- **Mixcloud Tracklist (`.mixcloud.txt` & JSON `mixcloudSections`)**: `MM:SS Artist - Title` / `H:MM:SS Artist - Title` pasteable timestamp list plus `sections-N-{artist,song,start_time}` fields
- **JSON (`.json`)** and **OBS Now-Playing (`GET /live/nowplaying.txt`)**

## 5. Risks & Mitigations

- **Real-time audio isolation**: zero hooks added to `libengine-ui-hook.so` or the 96 kHz audio thread; all StageLinQ, peer-sync, and file export work runs inside `stream-sync` (`CPU3` / `SCHED_IDLE` guarded for audio capture).
- **Shadow deck duplicate suppression**: standalone merge reads only `s.shadow.own` (local physical decks) and `s.shadow.peerDecks` (remote physical decks); on all-in-ones, `scShadowIsShadowSlot(d)` is explicitly skipped so muted/mirrored shadow lanes never enter the tracklist.
- **Release gate & UI translation parity**: preserve `objectName: "ControlCenter_LiveStream"` (`visible: true`), verify `PatchDict` has zero duplicate keys, and update `tools/ui-mod/i18n.tsv`, `pkg/webui/index.html` (`en` / `zh-Hans` / `zh-Hant`), `docs/site/content.js`, and all three `README*.md` files in the same commit.
- **LAN security**: `/api/tracklist/peer-sync` accepts requests only from private/link-local LAN peers (matching `shadow` peer endpoints), caps body size (`64 KiB`), and sanitizes control characters in exported filenames/CUE strings.

## 6. Test Plan

1. **`pkg/stagelinq` unit tests**:
   - UDP `"airD"` discovery encode/decode, TCP directory `"StateMap"` port lookup, `"smaa"` subscribe & JSON state emission against a mock X1800/X1850 TCP+UDP server, verifying channel fader + crossfader A/B/Thru audibility matrix.
2. **`pkg/tracklist` unit tests**:
   - Session lifecycle, backdated transition timestamps (`±1 s` accuracy), duplicate-track suppression during fader cuts, peer entry merge & deterministic ordering, and exact `.txt`, `.cue` (`MM:SS:FF` >60 min sets!), `.mixcloud.txt`, and `.json` formatting.
3. **`pkg/webui` integration tests**:
   - All-in-one (`JC11`) vs standalone player (`JP07`) `/api/broadcast` and `/api/tracklist`: verify `JP07` reports `audioSupported: false` and never opens audio capture when starting a tracklist session, while `JC11` starts both streaming/local recording and tracklist capture.
   - Two-player (`Player 1` + `Player 2`) simulated 30-minute set with shadow decks + StageLinQ X1850 fader transitions + analog fallback: verify merged tracklist on **both** servers has exact order, zero shadow duplicates, and downloadable `.txt` / `.cue` / `.mixcloud.txt` / `.json` + `/live/nowplaying.txt`.
4. **UI pack & existing regression suites**:
   - `python3 -m unittest discover -s tools/az01-packer` (verifying `build_packs.py`, `i18n.tsv` completeness, `PatchDict`, and QML JS harness tests).
   - `gofmt -l . && go vet ./... && go test -race ./pkg/stagelinq ./pkg/tracklist ./pkg/broadcast ./pkg/webui`.
