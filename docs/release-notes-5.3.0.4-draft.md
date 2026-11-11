## EngineX OS 5.3.0.4

A custom system rebuilt on the official Engine OS 5.3.0+ base for 16 Denon DJ, Numark and Rane standalone units: `.img` firmware for SC5000 / SC5000M / SC6000 / SC6000M / PRIME 4 / PRIME 2 / PRIME GO / Mixstream Pro, a `/data` bundle (Beta) for SC LIVE 4 / SC LIVE 2 / PRIME 4+ / PRIME GO+ / Mixstream Pro+ / Mixstream Pro Go / PRIME 4 G2 / Rane SYSTEM ONE, and the 1-click desktop `EngineX-Installer` for Windows, macOS and Linux.

基于官方 Engine OS 5.3.0+ 底层重构的定制系统，覆盖 Denon DJ、Numark 与 Rane 全系 16 款独立设备：8 款一代机提供完整 `.img` 刷机包，8 款二代机提供 `/data` 数据包（Beta），并提供 Windows / macOS / Linux 跨平台一键安装器 `EngineX-Installer`。

- **Documentation & User Guide / 官方三语文档站**：[https://enginexdj.com](https://enginexdj.com) · [https://cd1s.github.io/EngineX-OS/](https://cd1s.github.io/EngineX-OS/)
- **On-Deck OTA Update / 屏幕一键在线升级**：Decks on `5.3.0.3`, `5.3.0.2`, `5.3.0.1.1`, `5.3.0.1` or `5.0.4.2` (both 1st-gen `.img` and 2nd-gen `/data`-bundle units) can update directly from `Settings → About / Update`.

---

### What's New in 5.3.0.4 / 5.3.0.4 更新清单

- **#49 — Shadow Deck Mode (`Auto` / `All Engine Mode` / `All CDJ Mode`), Stock Display Header, Standalone 4-Deck Player UI & Clean Unload**:
  - `Control Center → Layout → ENGINEX VIEW PRESET → Shadow Deck Mode` adds `Auto` (default: copies EngineX or copyable CDJ tracks and loads a muted local Engine copy drawn with stock components, falling back to rekordbox waveforms with a one-line lane reason when uncopyable), `All Engine Mode` and `All CDJ Mode`, with immediate source metadata and stock `ProgressRing` while copying (`1 GiB` copy cap and stale cache eviction).
  - Engine-mode shadow headers mirror the source deck's real `Title`, `Artist` (respecting `Hide Title/Artist` and `Display File Name`), `Key`, `Key Offset`, `Auto Loop`, `Stems` badge, green `Sync` `BPM` and `LEAD` badge, draw the source's `Slip` timeline on the overview, and exclude muted shadow slots from `MASTER` election (`0x16fbc38`).
  - `All CDJ Mode` and CDJ fallback lanes use stock deck header and waveform geometry (album art, scrolling title/artist, tap-to-flip time + `TimeBar`, key color + `Key Lock`, green `Sync` `BPM`, `Pitch Range` / pitch %, `▲▼`, `LEAD`, `Auto Loop`, `Beat Jump`, `Hot Cue Countdown`, and `Track End Warning` red flash), accepting whole-second truncated `beatData` overviews and negative pre-roll main cues.
  - Standalone players (`SC5000 / SC5000M / SC6000 / SC6000M`) in 4-deck mode retain stock standalone player behavior: `Settings → Device → Player #`, dual-layer `User Profile → Display → Deck Color` (`Layer A / B`), `User Profile → Playback → Enable for Dual Deck`, `Browse` encoder press-to-load into the active layer, `Engine Connect` and `Touch FX` `Mute`.
  - Peer power-off / reboot / eject sends a UDP goodbye packet for debounced `Unload` + `UnSync` + `MASTER` return without deadlocking `RemoteState`, resolves real `Artist - Title` for streaming tracks from `stm.db`, and fixes blank paused detail waveforms.
- **#49 (`322839d`) — Shadow-Copy `Sync` Beyond `+100%` Pitch**: When a synced source deck runs faster than the `+100%` pitch fader limit (where `SpeedState` clips at `+100.0%`), the sender reports the exact `currentBpm / originalBpm - 1` ratio and the linked Engine-mode shadow copy follows speeds above `+100%` via native `Sync` while keeping song-position lock and releasing `Sync` cleanly on unsync or unload without taking `MASTER`.
- **#49 / #151 (`207dc55`) — Deck Header Row 2 Worst-Case Sizing & Unmoving `Cue Countdown`**: Compact `SpeedInfo` columns across 2-deck `Main`, 4-deck `GroupedMini`, `SplitMini` and shadow headers size from worst-case `±100` and `-100.00%` `TextMetrics` (preserving stock widths outside compact rows) so pitch fader and `Pitch Range` changes never shift neighbouring items; the `Hot Cue Countdown` capsule stays in a fixed slot so it never moves, its `BARS.BEATS` number is always shown whole while long cue names elide first, and `Auto Loop` / `Beat Jump` never propagate `fillWidth` to steal track-title space.
- **#85 — Perceptual RGB LED Brightness & Single-Color Button Passthrough**: `Settings → Device → Pad LED Brightness` (`100%`–`20%`) dims RGB LEDs (pads, mode buttons, `Layer`, platter rings and on-screen pads) perceptually while passing single-color `NoteOn` hardware keys (`SYNC`, `KEYLOCK`, `SLIP`, etc.) through at stock brightness.
- **#105 — OmniSource Playlist Isolation**: Unified search still spans streaming services and USB drives, while each streaming service's own playlist page stays isolated from `[USB: ...]` entries and other services' playlists.
- **#118 — First-Generation Custom Boot Splash**: Embeds the EngineX 5.3.0 boot splash across all 8 first-generation `*-Update.img` firmware images.
- **#134 — Standalone DMX Extracted to `dmx-studio` Mod**: Moves the standalone multi-universe DMX512 stack (Art-Net / sACN / FTDI / uDMX) out of the core binary into the optional official `dmx-studio` Mod with zero RAM or background process overhead when disabled.
- **#135 — Auto Loop Knob Link & Beat Jump Swap Fix**: Turning the Auto Loop encoder links `Beat Jump` size (`LinkBeatJumpLoopSize`) and fixes `beatJumpEnabled` on JP07 when `Swap Loop / Beat Jump` is on.
- **#137 — Generic USB MIDI Controller Mapper Mod (`midi-controller-mapper`, Hardware Validation Pending)**: Optional Mod Hub package (with a built-in **Pioneer CDJ-400** `PC MODE` preset and custom JSON presets) mapping external USB MIDI controllers directly to internal `Deck 3 / 4` at `os.nice(19)` (`60 Hz` StateMap bridge when enabled, zero polling when disabled), with velocity jog integration, `HARDEXTERNAL` pitch soft takeover + `▲▼` indicators, and USB disconnect / MIDI storm auto-disable.
- **#150 — SoundCloud Liked Tracks Fallback & Dual-NIC Spotify Connect mDNS**: Falls back to the app token and public profile API when SoundCloud OAuth refresh returns `HTTP 400`, advertises `_spotify-connect._tcp.local.` over both Avahi and a built-in pure-Go responder across `eth0` + `wlan0` with the standard 768-bit DH key, and fixes post-OAuth `410` track downloads.
- **#161 / #164 — Gen-2 & Gen-1 Installer Discovery, WSL `usbipd-win` Guide & `debugfs`-Free Unpack**: Fixes multi-line Windows `pnputil` parsing for SC LIVE 4, adds strict EAAS UDP discovery with port reuse plus NH08S (`15e4:103f`/`303f`, verified on physical Mixstream Pro+) and Mixstream Pro Go (`15e4:2059`) USB IDs and `fastboot 35+` compatibility, restores 1st-gen Computer Update Mode USB ID fallback when `fastboot getvar product` is absent, and decouples `az01_packer.py unpack` from `debugfs` on macOS/Windows.
- **#162 — PX Effect Time in Milliseconds (`10–4000 ms`, Off by Default, Hardware Validation Pending)**: `Profile → Playback → PX Delay Time Mode` (`Default (Off)` / `Milliseconds (ms)`, synced with the `:8080` web console switch) enables a shared BPM-independent `10–4000 ms` delay time across the three delay-based PX effects — `Spiral (PX)`, `Pitch Echo (PX)` and `Dub Echo (PX)` (with `PX Delay Time (ms)` presets also controlling the legacy master `Color FX Style`, including AZ04). On PRIME 4, `Time/Rate` (`11` steps from `10` to `4000 ms`), the `Time (ms)` readout and on-screen `−/+` buttons (`10 ms` steps) share the same value; stock Beat FX stay in beats, `Slip Roll (PX)` / `Space (PX)` keep their timing, and `Default (Off)` keeps DSP output byte-for-byte identical.
- **#166 — Mixstream Pro / Pro+ / Pro Go Physical `FILTER` Sweep FX & `Beat Jump Pad Bank`**: Binds physical `FILTER` knobs to native Sweep FX (`Filter` / `Dub Echo` / `Noise` / `Filter Roll` / `Wash` via each deck's `SWEEP FX` carousel), restores touch Beat Jump, and exposes `Layout → View → Beat Jump Pad Bank` plus per-deck `EQ / STEMS` toggle.
- **#168 — PRIME 4 (`JC11`) Native Hardware FX Selector PX Effects & Independent `Time/Rate` (Hardware Validation Pending)**: Adds `Spiral (PX)`, `Pitch Echo (PX)`, `Slip Roll (PX)`, `Dub Echo (PX)` and `Space (PX)` to the JC11 hardware FX selector with click-free delay-bank crossfades and independent 11-step `Time/Rate`, `Parameter` and `Wet/Dry` controls.
- **#169 — PRIME 4+ (`JC11S`) Activation Checks & Read-Only Diagnostics**: Hardens installer and bootstrap checks for JC11S `/data` targets, `fscrypt` first boot and ALSA paths, and adds read-only diagnostic and Pro DJ Link capture scripts.
- **#170 — Mod Archive Executable Bit Preservation**: Preserves executable permissions (`0111`) when installing `.tar.gz` Mods in the web console while stripping `setuid`/`setgid`/`sticky` bits.
- **#171 — SC LIVE 4 (`JP21`) AUX Routing to Channel 3 / 4 (Hardware Validation Pending)**: Routes AUX through `Mixer Menu → Channels → Input` on Channel 3 or 4 for channel `CUE`, fader, EQ and Sweep FX control while preserving `MIC2` and hiding unverified Bluetooth/Computer entries on JP21.
- **#175 — Full-Screen Boot & Shutdown Overlays Under `UI Scale`**: Anchors `SplashScreen` and the shutdown overlay to fill the parent canvas cleanly across `75%`–`110%` `UI Scale` on all 1st-gen and 2nd-gen units.
- **#176 — SC LIVE 4 & 2nd-Gen Read-Only Rootfs Stems Processor Detection (Hardware Render Confirmation Pending)**: Redirects `statx` and `open`/`open64`/`openat`/`openat64` checks for `/usr/local/bin/stems-processor` inside `Engine` to the executable `/data` bundle wrapper when the stock path is absent on the read-only root partition, so tapping **Generate Stems** detects the bundled processor instead of jumping to Settings.
- **#177 — 2nd-Gen `engine.service` Environment Alignment**: Pre-creates `0700` `/tmp/sc5000-engine-home` and sets `HOME=/tmp/sc5000-engine-home`, `MESA_SHADER_CACHE_MAX_SIZE=32M` and `QML_DISABLE_DISK_CACHE=1` in the 2nd-gen `engine.service` drop-in to match 1st-gen firmware.
- **#181 — CDJ-Style Pitch Fader Pickup (`Stock` Default, Real-Deck Feel Validation Pending)**: `Profile → Playback → Pitch Fader Pickup` (`Stock` / `CDJ`, default `Stock`) parks the pitch fader whenever the deck's tempo was changed elsewhere (`Sync` off, `Pitch Range`, on-screen tempo, other `Layer`, or a tempo-resetting load) until the fader reaches or crosses the deck's current tempo in one pass in either direction with zero jump, keeping the stock `▲▼` pickup arrows and leaving `Stock` 100% unchanged (applies within 10 s without restarting).
- **#182 / PR #183 — Legacy rekordbox 5 Device Library (`export.pdb`) USB Support (Pending Reporter Stick Validation)**: Accepts unanalysed BPM (`0xFFFFFFFF → 0`), ignores orphaned playlist entries, and handles duplicate playlist names and broken parent folders cleanly on legacy rekordbox 5 sticks.
- **Verbose Logging & Sanitized Log Download**: Adds a web-console `Verbose logging` toggle (`QT_LOGGING_CONF=/data/logging.ini` with `qt.*.debug=false`, off by default) and 1-click `Download logs` exporting the last 30 minutes (up to 5,000 lines / 2 MiB) with tokens, ARLs, passwords, cookies, signed URLs and access codes redacted.
- **Streaming Cache Isolation, SoundCloud Go+ Routing, Brand Unification & Performance Tuning**: Per-service download cache namespaces (with shared Deemix lossless cache when NetEase uses *Deemix first*), Beatport cold-cache lossless upgrade retention, SoundCloud Go+ / cross-service lossless matching, unified `/data/enginex` runtime + `enginex-relay` Multi-Deck Hub + 1-click Cloudflare Quick Tunnel, cached `scTr` QML translators, zero-fork `sc5000-perf-tune`, and a de-duplicated localized track-swipe `Stems` button.

---

### 1-Click Desktop Installer (`EngineX-Installer`, Recommended for 2nd-Gen)

| Platform | Download | SHA-256 |
|---|---|---|
| **Windows 10/11 x64** | [`EngineX-Installer-5.3.0.4-windows-amd64.zip`](https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.4/EngineX-Installer-5.3.0.4-windows-amd64.zip) | `<SHA256-EngineX-Installer-5.3.0.4-windows-amd64.zip>` |
| **macOS Apple Silicon (M1–M4)** | [`EngineX-Installer-5.3.0.4-macos-arm64.zip`](https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.4/EngineX-Installer-5.3.0.4-macos-arm64.zip) | `<SHA256-EngineX-Installer-5.3.0.4-macos-arm64.zip>` |
| **macOS Intel** | [`EngineX-Installer-5.3.0.4-macos-amd64.zip`](https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.4/EngineX-Installer-5.3.0.4-macos-amd64.zip) | `<SHA256-EngineX-Installer-5.3.0.4-macos-amd64.zip>` |
| **Linux x86_64** | [`EngineX-Installer-5.3.0.4-linux-amd64.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/download/v5.3.0.4/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz) | `<SHA256-EngineX-Installer-5.3.0.4-linux-amd64.tar.gz>` |

---

### SHA-256 Checksums

> Placeholder table (20 release artifacts total: 4 installers above + 8 first-gen `.img` + 8 second-gen `Data-Bundle.tar.gz` below). Replace `<SHA256-...>` from the final `SHA256SUMS.txt` before publishing `v5.3.0.4`.

#### First-Generation Firmware Images (`*-Update.img`)

| Model | Board | File | SHA-256 |
|---|---|---|---|
| SC5000 Prime | JP07 | `SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `<SHA256-SC5000-5.3.0.4-JP07-Update.img>` |
| SC5000M Prime | JP08 | `SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `<SHA256-SC5000M-5.3.0.4-JP08-Update.img>` |
| PRIME GO | JP11 | `PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `<SHA256-PRIMEGO-5.3.0.4-JP11-Update.img>` |
| SC6000 Prime | JP13 | `SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `<SHA256-SC6000-5.3.0.4-JP13-Update.img>` |
| SC6000M Prime | JP14 | `SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `<SHA256-SC6000M-5.3.0.4-JP14-Update.img>` |
| PRIME 4 | JC11 | `PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `<SHA256-PRIME4-5.3.0.4-JC11-Update.img>` |
| PRIME 2 | JC16 | `PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `<SHA256-PRIME2-5.3.0.4-JC16-Update.img>` |
| Mixstream Pro | NH08 | `MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `<SHA256-MIXSTREAMPRO-5.3.0.4-NH08-Update.img>` |

#### Second-Generation Data Bundles (`*-Data-Bundle.tar.gz`, Beta)

| Model | Board | File | SHA-256 |
|---|---|---|---|
| SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-SCLIVE4-5.3.0.4-Data-Bundle.tar.gz>` |
| SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-SCLIVE2-5.3.0.4-Data-Bundle.tar.gz>` |
| PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-PRIME4PLUS-5.3.0.4-Data-Bundle.tar.gz>` |
| PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-PRIMEGOPLUS-5.3.0.4-Data-Bundle.tar.gz>` |
| Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-MIXSTREAMPROPLUS-5.3.0.4-Data-Bundle.tar.gz>` |
| Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-MIXSTREAMPROGO-5.3.0.4-Data-Bundle.tar.gz>` |
| PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-PRIME4G2-5.3.0.4-Data-Bundle.tar.gz>` |
| SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `<SHA256-SYSTEMONE-5.3.0.4-Data-Bundle.tar.gz>` |
