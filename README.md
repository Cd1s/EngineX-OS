<div align="center">

<img src="./docs/assets/hero-banner.svg" alt="EngineX OS 5.3.0.4" width="100%">

<br>

<a href="./README.md"><img src="https://img.shields.io/badge/English-00e676?style=for-the-badge" alt="English"></a>
<a href="./README.zh-CN.md"><img src="https://img.shields.io/badge/%E7%AE%80%E4%BD%93%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="简体中文"></a>
<a href="./README.zh-TW.md"><img src="https://img.shields.io/badge/%E7%B9%81%E9%AB%94%E4%B8%AD%E6%96%87-30363d?style=for-the-badge" alt="繁體中文"></a>

<br>

**[Unlocked Features](#features)** · **[Download Firmware](#install)** · **[Screenshots](#gallery)** · **[Before vs. After](#benchmarks)** · **[Sponsor](#sponsor)**

📖 **Online User Guide**: <https://enginexdj.com>

</div>

<br>

**EngineX OS 5.3.0.4** is a custom system rebuilt on the official Engine OS 5.3.0+ base, for 16 Denon DJ, Numark and Rane standalone units:
`SC5000` · `SC5000M` · `SC6000` · `SC6000M` · `PRIME 4` · `PRIME 2` · `PRIME GO` · `Mixstream Pro` · `SC LIVE 4` · `SC LIVE 2` · `PRIME 4+` · `PRIME GO+` · `Mixstream Pro+` · `Mixstream Pro Go` · `PRIME 4 G2` · `Rane SYSTEM ONE`.

The first eight take a USB-stick `.img`; the eight second-generation units check the firmware signature in software before anything is written, so they take a `/data` bundle instead (Beta, see [How to Install](#install)).

Everything works right inside the stock Denon interface — flash it from a USB stick and unlock the features below.

---

<a id="features"></a>

## Unlocked Features

### 1. 4-Part Stems on Every Model (Vocals / Melody / Bass / Drums)
Official firmware locks standalone Stems to select hardware. EngineX OS unlocks the native `Stems` playlist and Performance Pads 4-part mute/unmute across **all SC5000, SC6000, and PRIME models**:
- Swipe left on any USB or streaming track to split **Vocals, Melody, Bass, and Drums** right on the deck;
- Hit the Performance Pads to mute vocals for live mashups or isolate drums for transitions;
- Splits tracks directly on the deck with no computer required, or automatically uses your home PC / cloud GPU for high-speed AI separation when on the same Wi-Fi.

### 2. 7 Streaming Services Built In (Phone QR Login)
No need to copy tracks to a USB drive before every set:
- **NetEase Cloud Music, Spotify, Deezer, SoundCloud, TIDAL, Beatport, and Apple Music** sit right next to your USB and SD drives in the Source menu;
- Scan the on-screen QR code with your phone to log in and browse your playlists, liked tracks, and search results;
- Switch between **Lossless FLAC and 320 kbps MP3**. Tracks you play are cached locally on the deck so loading them again is instant without re-downloading.
- **Cache isolation and shared Deemix cache**: Spotify (`spotify-<id>.*`), Beatport (`beatport-<id>.*`), SoundCloud (`soundcloud-<id>.*`) and NetEase in *Official* mode (`netease-<id>.*`) each download through their own channel into their own cache namespace. A song you already fetched from Deezer or NetEase is downloaded again, and cached separately, when you play it from Spotify. When NetEase is set to *Deemix First*, NetEase and Deezer share the same lossless Deemix source, so a song played from one and then from the other reuses the cached file without downloading again; with NetEase on *Official*, the two caches stay independent.
- **SoundCloud Go+ and high-quality routing**: DJ sets and long mixes (over 10 minutes) always play SoundCloud's own stream, the uploader's original 320k / WAV first, then 256k / 160k AAC or progressive MP3. For Go+-only tracks and ordinary non-set tracks: with a **SoundCloud Go+** account bound, the deck plays SoundCloud's own high-quality stream; without Go+ (the official stream gives Go+-only tracks just a 30-second preview), it matches the song on whichever of Deezer / NetEase / Beatport you are signed in to, pulls the lossless / 320k audio and caches it as `soundcloud-<id>.*`; if none of them is signed in, or the track is a SoundCloud-exclusive upload, it plays from SoundCloud directly, and for a Go+-only preview with no usable service signed in the deck prompts you to scan a QR code for SoundCloud Go+ or another streaming account.
- **Beatport** uses your own account and plan: Professional downloads FLAC or 256k AAC, Advanced downloads 128k AAC, and Essential streams 128k AAC. Beatport tracks split into Stems like any other track.
- **Apple Music**: paste any playlist or album link on your phone, no account needed; add your `media-user-token` to browse your own library too. Playback uses Engine 5.3.0's own Apple Music source, which EngineX unlocks; tracks are not matched to Deezer or NetEase.

### 3. Pioneer Rekordbox USB Plug & Play (No Desktop Conversion)
Bring the Rekordbox USB stick you exported for CDJs and plug it straight into your Denon deck:
- Reads all your playlists, Hot Cues, Memory Cues, Loops, and beat grids in **under 2 seconds** — no need to convert your library in Engine DJ Desktop;
- Works with any drive format: Windows `FAT32` / `exFAT` / `NTFS` and Mac `HFS+` / `APFS`;
- Aligns Rekordbox beat grids, Hot Cues, Memory Cues and loops on MP3s with `Xing` / `Info` and `LAME` headers to Engine's decoder start offset (~26–51 ms at 44.1 kHz, leaving WAV / FLAC / AIFF and headerless MP3s untouched, `#178`);
- Automatically repairs corrupted databases from unmounted drives and removes the "improper eject" warning popup.

### 4. Pioneer-Style 3-Band Waveforms & 3 Languages
- **3 Waveform Styles**: Switch between stock Denon green, high-contrast **RGB**, and Pioneer-style **3-Band (Blue / Orange / White)** waveforms in the Control Center (`Waveform Style` tile) or under `Profile → Display → Waveform Style`, synced across both views (all 16 models);
- **3 Languages**: Switch between **English, Simplified Chinese (简体中文), and Traditional Chinese (繁體中文)** (all 16 models).

### 5. Click-Free FX Switching & Smoother Long Sets
- Eliminates the audible clicks/pops that happen on stock firmware when toggling or switching DJ effects, even when triggering FX on both decks at once;
- Defaults anonymous usage statistics to off and disables background telemetry disk writes so all memory and CPU power stay focused on audio playback and touchscreen response.

### 6. Smart Sync & Gigabit Ethernet
- **Smart Sync**: pull down the Control Center and switch the Sync mode between **Bar / Beat / Tempo** with one tap. With Sync on, when the BPM gap between two tracks is wider than the current pitch range, the deck steps Pitch Range up until it fits (up to `±100%`);
- **Gigabit Ethernet**: the wired port runs at **1 Gbps (1000 Mbps) full duplex** instead of the stock `100 Mbps` limit, with 8 MiB TCP buffers for large transfers;
- **Wi-Fi**: the deck's built-in Wi-Fi module is limited by the board's 25 MHz SDR12 SDIO bus to about `9–10 MB/s` in practice. Use the Gigabit port to copy large libraries.


### 7. New in 5.3.0.4
- **Verbose logging and log download**: in the web console’s deck settings, enable **Verbose logging** for troubleshooting (off by default). Engine restarts and playback stops when you switch it. **Download logs** saves a redacted text attachment from the last 30 minutes (up to 5,000 entries / 2 MiB); no SSH needed. Turn verbose logging off after reproducing the issue.
- **4-deck link: clean unload and real streaming titles (#49)**: when the other player powers off, reboots or ejects its track, it sends a goodbye packet; after a 350 ms debounce this deck unloads its shadow deck (`Unload` + `UnSync`, handing `MASTER` back automatically) and clears the leftover waveform and track. The `RemoteState` subscription that could deadlock is gone, so a partner powering off never blocks you from switching streaming sources. Streaming tracks now show the real `Artist - Title` on the shadow deck instead of an internal numeric cache file name;
- **Smoother UI under load**: a cached `scTr` QML property translator and a 500 ms throttle on `QuickCue` remove the brief stutter when pressing `MASTER` / `SYNC` and cut the CPU use of Engine's `EMain` GUI thread;
- **One Stems button in the track-list swipe menu**: local tracks that already carry Engine's own `Stem` action are de-duplicated into a single localized `Stems` button (`分轨 Stems` / `分軌 Stems`), so swiping left on a local, streaming or LAN track looks and works the same;
- **Stock Link Beat Jump / Loop Size Controls unlock and Beat Jump swap fix (#135)**: EngineX unlocks the stock `Profile → Cues/Loops → Link Beat Jump / Loop Size Controls` setting on models where stock Engine does not offer it (default loop size and beat jump size move together; you choose `On` or `Off`, and EngineX never writes its value), and fixes the `beatJumpEnabled` check on JP07 when the separate `Layout → ENGINEX VIEW PRESET → Swap Loop / Beat Jump` switch (which swaps the knob functions) is enabled;
- **OmniSource playlist isolation (#105)**: with OmniSource on, the search bar still searches all streaming services and every USB drive, but a streaming service's own playlist page no longer mixes in `[USB: ...]` entries or other services' playlists;
- **SoundCloud Liked Tracks fallback (#150)**: when the user's OAuth `refresh_token` has expired and SoundCloud answers `HTTP 400`, the deck falls back to the app token and the public profile API to load `Liked Tracks & Sets`;
- **Spotify Connect on the LAN (#150)**: the deck advertises `_spotify-connect._tcp` over Avahi mDNS plus a built-in pure-Go `_spotify-connect._tcp.local.` responder in `stream-sync` (found on both wired `eth0` and wireless `wlan0`) with the standard 768-bit Diffie-Hellman public key, so the Spotify app on a phone finds `EngineX OS` on the local network; the track-download `410` after OAuth login is fixed;
- **Native Spotify audio and zero-config login (#210, 5.3.0.4)**: Spotify Premium audio uses the pure-Go `go-librespot` protocol stack and AES decryption, preferring 320 kbps Ogg Vorbis (then 160/96 kbps), with no Deemix fallback. Pair with **`EngineX OS`** in Spotify Connect or scan the direct official Device Auth QR; no Developer App setup is needed. Pure-Go `.ogg` analysis supplies BPM, key and waveforms for the Engine library.
- **Cache isolation and shared Deemix cache**: Spotify, Beatport, SoundCloud, and NetEase in *Official* mode each download through their own channel into their own cache namespace; when NetEase is set to *Deemix First*, NetEase and Deezer share the lossless cache without re-downloading;
- **SoundCloud Go+ and high-quality routing**: DJ sets and long mixes (over 10 minutes) always stream from SoundCloud directly; ordinary tracks and Go+ exclusives play SoundCloud's high-quality stream if signed in to Go+, or automatically match lossless / 320k audio across signed-in Deezer / NetEase / Beatport to avoid 30-second preview cutoffs;
- **Brand unification & zero-domain remote connection**: user-facing components are unified as `enginex-relay` (formerly `sc5000-relay`), `enginex-stemsd` (formerly `sc5000-stemsd`), and `enginex-suite`, and run under `/data/enginex` (with `/data/sc5000-custom` preserved as a compatibility symlink); 1-click Cloudflare Quick Tunnel provides domain-free public access for mobile streaming QR code logins; the relay gains a `Connected decks (Multi-Deck Hub)` card monitoring all connected players, and `Settings → About / Update` shows live LAN Stems compute node status;
- **Mixstream Pro / Pro+ / Pro Go (#164, #166)**: physical FILTER knobs control native Filter / Dub Echo / Noise / Filter Roll / Wash via `SWEEP FX` in each deck’s Pads bar; touch Beat Jump is restored with `Layout → ENGINEX VIEW PRESET → Beat Jump Pad Bank`; channel EQ knobs switch to Stems EQ via `EQ / STEMS`; `EngineX-Installer` supports strict EAAS discovery with port reuse, fastboot 35+, Windows WSL 2 + usbipd-win flashing, and recognises Mixstream Pro Go (`15e4:2059`) on USB; verified on a physical NH08S unit;
- **1st-gen USB fastboot line flashing & debugfs decoupling**: restores computer line flashing for SC5000, SC6000 and PRIME 4 units when `fastboot getvar product` is unimplemented by falling back to USB IDs; unpacks firmware images on macOS and Windows without debugfs;
- **PRIME 4 hardware FX selector with independent PX Time/Rate (#168; hardware validation pending) and PRIME 4+ activation verification (#169)**: refreshes the JC11 hardware FX selector with 5 PX effects (`Spiral (PX)` / `Pitch Echo (PX)` / `Slip Roll (PX)` / `Dub Echo (PX)` / `Space (PX)`); connects the native `Time/Rate` event (`0–10`, 11 steps via `BEAT −/+` or touch Time/Rate) to control PX timing independently using relative steps rather than beat synchronization (the stock beat labels on the screen/OLED indicate the selected relative step, not delay in beats), while `Parameter` controls character and feedback and `Wet/Dry` controls the blend, with rearming and wet fade burst protection; hardens PRIME 4+ (JC11S) activation files and data target checks; retains Beatport lossless upgrade on cold cache lookups, and exits `deemixd` gracefully when `stream-sync` binds port 6595;
- **Shadow Deck Mode (`Auto` / `All Engine Mode` / `All CDJ Mode`), loading progress ring and Engine-mode header mirroring (#49; verified on dual SC5000s, physical console / CDJ validation pending)**: in the 4-deck layout Engine loads every copyable shadow track (muted) and draws it with the same stock components as a local deck: an EngineX unit's track is copied from that unit, a CDJ / Pro DJ Link device's track from the CDJ's USB drive or SD card. While copying, the shadow deck immediately displays the source track's title, artist, album art, BPM, key and duration with the stock `ProgressRing`. In Engine mode, the shadow header mirrors the source deck's real title, artist, key and key shift (`Key Offset`), `Auto Loop`, `Stems` icon, green `Sync` `BPM` and `LEAD` badge (shown only where the source header shows `LEAD`), and the overview waveform draws the source's active `Slip` timeline. A CDJ track with no file to copy (streaming, rekordbox Link, CD or 2nd USB, or a copy that failed or timed out) falls back to its CDJ (rekordbox) waveform with one line giving the reason. `Layout → ENGINEX VIEW PRESET → Shadow Deck Mode`: `Auto` (the default on every model; CDJ-3000 waveforms on a console's Line In work as before), `All Engine Mode` (Engine loads every source) or `All CDJ Mode` (no track is loaded: display-only CDJ view);
- **CDJ display-only shadow waveforms**: CDJ tracks show their cached rekordbox waveforms, beat grids and cues without downloading audio; tracks without analysis keep metadata only. On PRIME 4 / PRIME 4+ / PRIME 4 G2 and SC LIVE 4, enable `4-Deck Waveform` to use decks 3–4 as shadows while local decks 1–2 stay available; lanes without a remote source remain usable locally. A custom rb-bridge `--media-cache` must match `SC5000_PROLINK_MEDIA_CACHE` in the WebUI service environment; both default to the same directory. Disabling `prolink_share` also disables cached display reads;
- **Rekordbox MP3 encoder-delay alignment (#178) & legacy rekordbox 5 Device Library USB support (#182; pending validation on reporter's USB stick)**: shifts Rekordbox beat grids, Hot Cues, Memory Cues and loops on MP3s with `Xing` / `Info` and `LAME` headers by the exact decoder start offset (~26–51 ms at 44.1 kHz) to match audio transients while leaving WAV / FLAC / AIFF and headerless MP3s untouched and updating only affected MP3s on previously imported sticks (#178); reads USB sticks exported in the legacy rekordbox 5 Device Library (`export.pdb`) format with unanalysed BPM values (`0xFFFFFFFF`) recorded as `0`, orphan playlist entries ignored, and duplicate playlist names or broken parent folders handled cleanly (#182);
- **Perceptual RGB LED brightness scaling (#85)**: `Settings → Device → Pad LED Brightness` (`100%`–`20%`) dims RGB LEDs (pads, mode buttons, `Layer`, platter ring and on-screen pads) across 5 perceptual steps, while single-colour keys (`SYNC`, `KEYLOCK`, `SLIP`, etc.) only have bright/dim hardware states and keep their stock brightness;
- **SC LIVE 4 AUX (#171; hardware validation pending)**: the native `Mixer Menu → Channels → Input` selector sends AUX through channel 3 or 4 for CUE, fader, EQ and Sweep FX control; in AUX mode, selecting either Line-In suppresses the direct AUX feed while MIC2 keeps its stock route. Both Line-In channels share one AUX source and can sum if both faders are raised. Mixer Menu (Beta) moves some Preferences controls and enables loading tracks on Line-In channels; JP21 hides unverified Bluetooth/Computer choices. See [routing and hardware checks](docs/issue171-sclive4-aux.md);
- **All-in-one console CDJ-3000 waveforms via Line In (hardware validation pending)**: on all-in-one consoles (such as PRIME 4 and PRIME 2), switching a channel input to Line In maps connected Pioneer CDJ-3000 (Pro DJ Link) waveforms directly onto that channel (mapped in ascending order of CDJ player number to Line channel number); SC5000/SC6000 standalone player behavior is unchanged;
- **Full-screen boot and shutdown overlay scaling (#175)**: when UI Scale is set below 100% (e.g. 75%), the boot splash screen and shutdown overlay stretch to fill the enlarged canvas via full-parent anchoring, eliminating black borders across all 1st-gen and 2nd-gen models (including SC LIVE 4);
- **Shadow deck detail waveform fixes**: paused shadow tracks loaded at non-zero positions no longer appear blank before playback starts, and the scrolling waveform lane no longer stretches the whole-song overview image while high-detail tiles are still pending, keeping the display clean until detail tiles finish loading;
- **Mod Hub LAN access fix (#202), executable bit preservation (#170) & `dmx-studio` Mod (#134)**: routes Web Console Mod Hub requests through authenticated `/api/mods` and `/api/mods/upload` so LAN browsers no longer hit `403` (#202); preserves executable file permissions (`0111`) from `.tar.gz` archives while stripping setuid/setgid/sticky bits (#170); moves standalone multi-universe DMX512 out of core into the on-demand `dmx-studio` Mod (zero RAM or background processes when disabled, #134);
- **SC5000 / SC6000 4-deck mode preserves stock player UI, deck colors and `MASTER` LED dim level (#49)**: setting `Layout → ENGINEX VIEW PRESET → Linked View` to `4 Decks` on standalone players (`SC5000 / SC5000M / SC6000 / SC6000M`) keeps all stock player behaviors intact — deck colors follow `Settings → Device → Player #` (Pioneer: `Player No.`) and `Layer A / B` from `Profile → Deck Colors` (retaining the dual-layer `Layer A / B` color selector), pressing the `Browse` encoder in the library loads directly into this unit's active layer (`Layer A` or `Layer B`), and `Settings → Device → Player #`, `Layout → DECK BEHAVIOR → Layer B` (shows `Enable for Dual Deck` when off; Pioneer: `Dual Deck`), `Settings → Mixer → Sampler Output` (`Layer A` / `Layer B`), the remote deck picker, `Engine Connect` and the `Touch FX` `Mute` button are all restored; the `MASTER` button LED reflects the local deck's master state on 4-deck standalone players while keeping stock dim illumination when non-master across all models, and jog-wheel controls stay stock;
- **Stock deck header and waveform lane in `All CDJ Mode` and CDJ fallback shadow decks (#49)**: when `Layout → ENGINEX VIEW PRESET → Shadow Deck Mode` is set to `All CDJ Mode`, or when `Auto` mode falls back to CDJ waveform display for an uncopyable CDJ track (as well as CDJ waveforms on an all-in-one console's `Line In` channel), the shadow deck header and waveform lanes match Engine's stock components: album art (`Layout → PERFORMANCE LAYOUT → Display Album Art`), scrolling song title and artist (`Settings → Device → Hide Title/Artist` and `Profile → Display → Track Title`), remaining / elapsed time (tap to flip), musical key and key color following the stock display settings with the `Key Lock` icon (Pioneer: `Master Tempo`), large `BPM` digits that turn green when `Sync` (Pioneer: `Beat Sync`) is on, `Pitch Range` (Pioneer: `Tempo Range`) and pitch percentage, soft-takeover arrows `▲▼`, the stock `LEAD` badge (Pioneer: `MASTER`), `Auto Loop` and `Beat Jump` readouts, `Hot Cue Countdown`, and track-end red flashing timed by `Profile → Display → Track End Warning`; the scrolling lane and overview match stock pitch-scaled span, 2.2× / 1.3× bottom-anchored overview scaling, `TimeBar`, upper/lower `HotCueTriangle` markers and labels, outlined bar numbers, and stock `DeckTabs` active state;
- **Generic USB MIDI Controller Mapper Mod (`midi-controller-mapper`, #137; hardware validation pending)**: maps external USB MIDI controllers (targeting the **Pioneer CDJ-400** in USB `PC MODE` first, plus custom JSON presets) directly to internal `Deck 3` or `Deck 4` in 4-deck mode without switching hardware layers. Runs at `os.nice(19)` with zero background polling when disabled and an on-demand 60 Hz StateMap bridge when enabled (`~1.6 ms` p50 helper-to-QML bridge latency measured in the 5.3.0 sandbox), integrates CDJ-400 continuous velocity jog messages (`Bn 10`, `64=stop`), enforces tempo slider soft takeover with on-screen `▲` / `▼` (`SpeedOffsetUp` / `SpeedOffsetDown` / `SpeedNeutral`) indicators, and auto-disables on USB disconnect or MIDI storms (note: CDJ-400 in standard USB MIDI mode is transmit-only for button LEDs, so status and `▲▼` indicators are shown on the SC5000 screen);
- **Official Mods 1.1.0 with static Go helpers and in-place upgrade (#208, #134)**: official Mods (`dmx-studio`, `jog-logo-studio`, `sampler-pad-studio`, `minimal-club-mode`, `midi-controller-mapper`) previously used a Python helper and failed to start because none of the 16 deck models include Python (reported on Mixstream Pro+ NH08S). Every official Mod now ships static Go binaries (`bin/enginex-mod-helper-armv7` for gen-1 and gen-2 armhf decks, `bin/enginex-mod-helper-arm64` for PRIME 4 G2 / SYSTEM ONE) selected by busybox `sh` `start.sh` via `uname -m`, keeping the same HTTP contract and `Panel.qml`; DMX Studio drives uDMX directly over Linux `usbfs` without `libusb` and keeps the FTDI/OpenDMX timing validated on a real deck in #208 (`termios2` 250000 8N2, 120 µs break / 12 µs MAB, ~38 Hz, no audio impact). Upload the `1.1.0` `.tar.gz` in the `:8080` web console to upgrade an installed `1.0.0` in place while keeping its enabled state and settings. Mod authors: the deck has no Python, compiler or package manager — build static `CGO_ENABLED=0` helpers for `GOARCH=arm GOARM=7` and `arm64` using [`docs/mods/src/helper-template/`](docs/mods/src/helper-template/);
- **PX effect time in milliseconds & `Profile → Playback` FX declutter (#162; default `Default (Off)`, hardware validation pending)**: `Profile → Playback → PX Delay Time Mode` (`Default (Off)` / `Milliseconds (ms)`, shown only on 5.3.0 PRIME 4 or when `Color FX Style` is set to `Spiral`, `Pitch Echo` or `Dub Echo`, and synced with the `:8080` web console switch) enables a shared BPM-independent `10–4000 ms` delay time across `Spiral (PX)`, `Pitch Echo (PX)` and `Dub Echo (PX)` (expanding `PX Delay Time (ms)` presets when enabled, while hidden entries retain saved values and act as `Off`; `Color FX Style` on the 11 consoles without the native `(PX)` selector applies directly to the main audio output at a fixed mix until set back to `Off`; hidden and forced `Off` on SC5000 / SC6000); on PRIME 4, `Time/Rate` (`11` steps from `10` to `4000 ms`), the `Time (ms)` readout and on-screen `−/+` (`10 ms` steps) share the same value, while stock Beat FX stay in beats and `Slip Roll (PX)` / `Space (PX)` keep their existing timing ([scope and verification](docs/issue162-px-ms.md));
- **Deck header row 2 worst-case sizing and fixed `Cue Countdown` (#49, #151)**: compact `SpeedInfo` columns across 2-deck `Main`, 4-deck `GroupedMini`, `SplitMini` and shadow headers lock to worst-case `±100` and `-100.00%` `TextMetrics` with right-aligned range labels (preserving stock widths elsewhere) so pitch movements never shift neighbouring controls, the `Cue Countdown` capsule never moves and always keeps its `BARS.BEATS` number whole (eliding long cue names first), and `Auto Loop` / `Beat Jump` never propagate `fillWidth` to steal track-title space;
- **Shadow-copy `Sync` beyond `+100%`, phase-ramp tracking and silent shadow `MASTER` exclusion (#49)**: keeps muted local shadow slots out of `MASTER` election, tracks the source deck's native `Sync` phase-pull ramps and verifies active-playback seek landings, and when a synced source deck runs faster than the `+100%` pitch fader limit (where `SpeedState` clips at `+100.0%`), reports the exact BPM ratio so the Engine-mode shadow copy follows speeds above `+100%` through native `Sync` while keeping song-position lock and releasing `Sync` cleanly on unsync or unload without taking `MASTER`;
- **SC LIVE 4 and 2nd-gen read-only rootfs Stems detection (#176; hardware render confirmation pending)**: redirects `statx` and `open` checks for `/usr/local/bin/stems-processor` inside `Engine` to the executable `/data` bundle wrapper when the stock path is absent on the read-only root partition, so tapping **Generate Stems** detects the bundled processor instead of opening Settings (see [details and hardware scope](docs/issue176-sclive4-stems.md));
- **2nd-gen `engine.service` environment alignment (#177)**: `/data` bundles now pre-create `0700` `/tmp/sc5000-engine-home` and set `HOME=/tmp/sc5000-engine-home`, `MESA_SHADER_CACHE_MAX_SIZE=32M` and `QML_DISABLE_DISK_CACHE=1` in the `engine.service` drop-in to match 1st-gen firmware;
- **Boot recovery safety net and USB report export (#140)**: after 180 s without UI readiness or startup progress, or three Engine failures within 300 s, EngineX persistently falls back to stock Engine without its preload hook or UI pack and quarantines Mods (never triggering Factory Reset or erasing your library); inserting a writable USB stick or SD card exports a sanitized `EngineX-boot-report-<time>.txt`, while `:8080` shows recovery status publicly and offers `Retry EngineX` once signed in;
- **Mainland-China & global NTP sync, persistent clock floor and offline time-zone retention**: `Settings → Device → Time/Date → Sync Time` uses mainland-China and global NTP servers, advances a persistent monotonic clock floor across power cuts on units without a battery-backed RTC, and prevents early-boot offline fallback to `UTC` from overwriting a saved `Settings → Device → Time Zone` (while honouring an explicit manual `UTC` choice);
- **1st-gen first-boot Stems prompt suppression & EngineX boot splash (#118)**: 1st-gen `5.3.0` images mark the bundled Stems processor factory-installed so first boot on a blank `/data` no longer shows an invalid `Download` button, and 1st-gen `.img` images embed the EngineX boot splash screen.

### 8. New in 5.3.0.3
- **Remote access with Tailscale / Headscale (#130)**: the web console's `Remote access (Tailscale / Headscale)` card downloads a trimmed Tailscale client only when you ask for it (about 4.3 MB, checksum verified), signs in with a login link or a one-time auth key, and shares only what you tick (SSH / SFTP, the web console) with your own tailnet. No public IP or router changes needed, and your own Headscale server works too. Off by default. It runs in userspace mode because the deck kernel has no TUN device. Tested end to end with Headscale; CPU and memory use on a real deck during playback are not measured yet;
- **Sort streaming playlists (#98)**: Engine's own sort menu now opens for every streaming service: BPM, key, rating, title or artist, ascending or descending;
- **Record with streaming tracks (#84, #206)**: on all-in-ones with a recorder (`PRIME 4 / 4+ / 4 G2`, `PRIME 2`, `PRIME GO / GO+`, `SC LIVE 4 / 2`, `Mixstream Pro / Pro+ / Pro Go`, `SYSTEM ONE`), Record starts while a streaming track is loaded, and streaming tracks still load while you record (the standalone players have no recorder). Streaming services' terms may not allow keeping recordings of their tracks: keep them for your own use;
- **BPM in the track list (#80)**: `Layout → ENGINEX VIEW PRESET → Dual-Column Browser` shows each track's BPM next to the second column;
- **Fader Echo and Smart Sync (#104)**: on all-in-one consoles with a mixer (`PRIME 4 / 4+ / 4 G2`, `PRIME 2`, `PRIME GO / GO+`, `SC LIVE 4 / 2`, `Mixstream Pro / Pro+ / Pro Go`, `SYSTEM ONE`), the stock Control Center `Fader Echo` tile stays active (never duplicated) and syncs bidirectionally with `Settings → Mixer → Fader Echo` (`Off` / `All Faders` / `Upfader Only`) and `Profile → Playback → Echo Out` (hidden on standalone players `SC5000 / SC5000M / SC6000 / SC6000M`). With Sync on, Smart Sync also tries half and double time (0.25×–4×) and widens Pitch Range only as far as the closest match needs (all 16 models);
- **Control Center performance row**: `Waveform Style` and `Smart Sync` on all 16 models, alongside the stock `Fader Echo` tile on consoles with Transition FX. `Live Stream` moved to `Settings → Services`, and `Waveform Style` stays in the Control Center and `Profile → Display → Waveform Style` in sync (no duplicate in `Layout → ENGINEX VIEW PRESET`);
- **Pioneer-style Beat FX & Sound Color FX (#88, #168)**: on JC11 PRIME 4 with Engine 5.3.0, choose `Spiral (PX)`, `Pitch Echo (PX)`, `Slip Roll (PX)`, `Dub Echo (PX)` or `Space (PX)` at the end of the FX selector. The controls apply to that FX unit and its assigned channels: `Time/Rate` (`0–10` via `BEAT −/+` or touch Time/Rate) independently adjusts PX timing using relative steps (not beat-synchronized; the screen/OLED beat labels indicate the selected relative step, not delay in beats), `Parameter` adjusts character/feedback, and `Wet/Dry` adjusts the blend. Original FX remain available. SC5000 / SC6000 standalone players do not offer Pioneer-style effects: `Color FX Style` is hidden there and a saved style acts as `Off`. AZ04 keeps the legacy `Color FX Style` setting; PRIME 4 hardware verification is pending;
- **UI scale (110%–75%), tracklist waveform toggle and pad LEDs (#153, #117, #85)**: `Layout → ENGINEX VIEW PRESET → UI Scale` scales Engine's whole interface from 110% down to 75%, with the Control Center and pop-ups still filling the screen; `Profile → Library → Tracklist Waveform` hides the mini waveform in the track list so track titles expand across the full row; `Settings → Device → Pad LED Brightness` dims RGB LEDs (pads, mode buttons, Layer, platter ring and on-screen pads) from 100% down to 20%, while single-colour keys (`SYNC`, `KEYLOCK`, `SLIP`, etc.) only have bright/dim hardware states and keep their stock brightness;
- **Physical 3-band Stems EQ Mode & hardware shortcuts (#152)**: on mixer consoles with Stems EQ (`PRIME 4 / 4+`, `PRIME 2`, `PRIME GO / GO+`, `SC LIVE 4 / 2`, `Mixstream Pro / Pro+ / Pro Go`, `SYSTEM ONE`; hidden on standalone players and `PRIME 4 G2`), `Profile → Playback → Stems EQ Mode` or **Shift + Cue (PFL)** on any mixer channel switches that channel's physical `HIGH / MID / LOW` EQ knobs to **Stems Level** (`Vocal`, `Melody + Bass`, `Drums`) with native soft takeover and on-screen status indicator; rapid double-pressing `Hot Cue` on PRIME 2 / PRIME 4 latches `STEMS` pad mode cleanly without bouncing back to `CUES`;
- **PRIME 4 / PRIME 4+ / SC5000 / SC6000 cross-unit 4-deck LAN linking (#160)**: `Linked View` (`Stock` / `4 Decks`, plus `Paired Player` on standalone players), `4-Deck Role` (`Auto`, `1+3`, `2+4`, `1+2`, `3+4`, `1`, `2`), and `Shadow Deck Mode` are shown on 4-deck hardware (`SC5000 / SC5000M / SC6000 / SC6000M`, `PRIME 4 / PRIME 4+ / PRIME 4 G2`, `SC LIVE 4`; hidden on 2-deck consoles), so a PRIME 4 / PRIME 4+ can play Decks 1+2 locally while Decks 3+4 mirror linked SC5000 / SC6000 players over LAN; `Swap Loop / Beat Jump` is available on all units with an Auto Loop encoder (hidden on `Mixstream Pro / Pro+ / Pro Go`), and `Beat Jump Pad Bank` is shown on `Mixstream Pro / Pro+ / Pro Go` only;
- **Streaming track load reliability (#159)**: Engine's local streaming account now carries non-SSO tokens and bypasses the 150-second Engine DJ Profile SSO wait, fixing streamed tracks that downloaded to `/data/sc5000-custom/Stream` but stalled on deck load with `"provider disconnected from Engine DJ profile"`;
- **Five saved views (#99)**: `Save View` in `Layout` stores the current layout as one of `View 1`–`View 5`, to recall in one tap;
- **Shadow waveforms for streaming tracks**: in the 4-deck layout, a streaming track loaded on the other player shows its real waveform on your shadow deck, from Engine's own download and analysis. `4-Deck Role` set to `Auto` now always follows Engine's Player number: odd players take decks 1 / 3, even players decks 2 / 4. The library's bottom deck bar now runs `1, 2, 3, 4` left to right (stock order is `1, 3, 2, 4`), lights only this player's two decks and greys out the shadow decks so they can't be loaded by mistake (#90, #123);
- **djay libraries (#112)**: with OmniSource on, Algoriddim djay libraries on a USB / SD drive show their playlists in the streaming browser;
- **Shorter console access code**: the web console code is now 8 characters (`XXXX-XXXX`, no 0/O or 1/I, case and dashes ignored). Upgrading replaces the old long code, so signed-in browsers need the new one from `Settings → About / Update`. Five wrong codes in a minute lock that device out for a minute;
- **Second-generation installer (#89, #110, #129)**: EngineX-Installer finds second-generation decks on its own, including a PRIME 4+ that new `android-tools` list as `Android Fastboot`, using its bundled `fastboot`;
- **Access code on second-generation decks (#132)**: on PRIME 4+ and other second-generation units, `Settings → About / Update` showed no web console address or access code and updates reported `GitHub unreachable`, because restarting Engine at boot also killed the EngineX services started just before it. The services now run as their own systemd units, so restarting Engine no longer touches them. A later `KillMode=process` attempt left the stock Engine on screen and broke power-off on SYSTEM ONE; it was removed (#116).
- **SYSTEM ONE / PRIME 4 G2 over USB (#116)**: these decks have no fastboot; their Firmware Update Mode is USB `1cc5:2027`. EngineX-Installer now builds a fresh 64 MiB `/data` image and writes it with the official `write-part` protocol (verified on a Rane SYSTEM ONE), then asks you to power-cycle the deck. `host-install-userdata.py` gains `--ssh-key` for root SSH.
- **Experimental, API only**: the following have a web console API but no console card or deck menu yet, and have not been tested on a real deck: WebDAV / Subsonic (Navidrome) / Plex playlists through OmniSource (#82), Hot Cue names and colours (#85), USB `/mods` QML plug-ins with safe-mode quarantine (#87), copying a loop to a Sampler pad (#120), and LAN RTP / Teleport audio plus USB DAC / USB-B UAC2 output (#83, #113).

### 9. Previous Release Highlights
- **Zoom further out on the waveform (#77)**: the all-in-ones (PRIME 4, PRIME 2, PRIME GO, Mixstream Pro, SC LIVE) zoom out to 30 seconds like the SC5000 / SC6000, instead of Engine's 10 s, so more of the upcoming beat grid is on screen. Turn the Browse knob on the deck screen to zoom when `Settings → Device → Encoder Behavior` is `Waveform Zoom`;
- **Turn Off from the screen (#78)**: `Settings → Device → Power → Turn Off` opens Engine's own "Turn Off" confirmation, so a deck in a booth can be shut down without reaching for the rear power button;
- **Relocate Missing Tracks (#74)**: after files were moved or renamed on the same drive, the web console finds each missing track's new place (same name and size, then the name ignoring case, then a cleaned-up name). `Scan` only previews; `Relocate` backs up `m.db` first, then rewrites the paths. Reload the drive on the deck afterwards;
- **Apple Music sign-in (#75)**: the phone page signs in with Apple Music (MusicKit JS). Where Apple does not allow that, it offers a bookmarklet that copies your token from music.apple.com;
- **Console Access Code on the deck (#79)**: `Settings → About / Update` shows the web console address and access code. A streaming QR code logs in only that service and never opens the console, so guests who scan the screen in a club get no control of your deck;
- **Silent Cue and Master Limiter**: the Censor button can be set to `Silent Cue` (`Profile → Playback → Censor Button Behavior`), and PRIME GO / Mixstream Pro get Engine's own `Master Limiter` (`Settings → Mixer`, Off / 0–18 dB), next to the EngineX `Master Soft Clipper`;
- **Apple Music** joins the streaming services (7 in total), logged in with the deck QR code like the others;
- **Wi-Fi Music Drop**: drag MP3 / FLAC / WAV / AIFF / M4A / AAC / OGG / OPUS files from a computer onto the deck's web console (`http://<deck IP>:8080`) and they are saved to the internal drive, SD card or USB of your choice under `Wi-Fi Drop/<playlist>/`. FLAC / MP3 on the library drive are analysed and added to that playlist; existing files are never overwritten and 256 MiB always stays free;
- **OmniSource**: the playlists and tracks on your USB / SD drives appear right inside the streaming browser, next to your streaming playlists, and one search finds both. Drive tracks play straight from the drive at full quality (`Profile → Library → OmniSource (Drives in Streaming)`, off by default);
- **Pioneer CDJ / Pro DJ Link (Beta)**: read a CDJ's rekordbox USB library over LAN / Wi-Fi (`Settings → Services → Pro DJ Link Library`) and share the deck's own library with CDJs (`Settings → Services → Pro DJ Link Export to CDJ (Beta)`). There is one tempo master across the SC5000 and the CDJs: MASTER is handed over either way, and as master the deck sends its BPM and beats for the CDJs' BEAT SYNC. All off by default;
- **Cleaner sound**: `Master Soft Clipper` keeps the master output under -0.1 dBFS with a smooth curve and zero latency, untouched below -1.5 dBFS; the Flanger no longer dips the volume (level matched to the dry track, up to +6 dB); `Echo Out` (on all-in-one consoles with a mixer; hidden on standalone players) leaves an echo tail when you pull a fader down; `Auto-Gain (0 dB Normalize)` brings quiet tracks up toward 0 dBFS (at most +9 dB, slow and smooth, never lifts silence below -42 dBFS) and always ends in the soft limiter, so it cannot clip (off by default). 21 real-time DSP hooks in total, with no added latency;
- **Paired Player 2-deck view (#201, pending hardware validation)**: for two linked SC5000 / SC6000 players, `Layout → ENGINEX VIEW PRESET → Linked View → Paired Player` shows two full-height lanes: this unit's active layer on top (the Layer button still moves it between Layer A and Layer B) and the other player's active deck below as a shadow lane that follows that player's layer switches; inactive decks are hidden and lane 2 reads `No paired player` without a partner. Needs `4-Deck Role` `Auto`, `1+3`, `2+4`, `1` or `2`; `Linked View` is `Stock` by default and `Stock` is always one step back: the stock single / dual layer views then switch as before with `Shift + Layer` or `Layout → PERFORMANCE LAYOUT → Deck Layout` (`Single` / `Dual (Stacked)` / `Dual (Grouped)`). Switching between `Stock` and the other two restarts Engine; between `Paired Player` and `4 Decks` it does not.
- **Native 4-deck layout on SC5000 / SC6000**: Engine's own 4-deck view on a standalone player, with the sound card left at 2 channels. Each player plays the two decks of its role (`Auto`, `1+3`, `2+4`, `1+2`, `3+4`, `1`, `2`, set in `Layout → ENGINEX VIEW PRESET → 4-Deck Role`). The other decks are silent **shadow decks** that follow a second EngineX player or a CDJ: track, play / CUE, position, pitch, master and sync;
- **Live Stream** to Shoutcast / Icecast servers;
- **Library**: a rekordbox export (6.8.5 included) still imports when a few of its files are damaged, and those tracks are analysed when loaded; Engine 5.3.0's "Connect to your Engine DJ Profile to use Tidal" popup no longer appears while another service is selected;
- **Spotify login (#210)**: Connect LAN pairing and the official Device Auth QR use the built-in desktop client without a custom Client ID. If device authorization is unavailable, PKCE/paste and public playlist links remain available. Old Web API-only tokens need one new Connect pairing or QR authorization for native audio.
- **Steadier streaming and shadow decks**: a streaming playlist left open on the deck keeps loading after the background service restarts, instead of failing with "Track not found" (`#73`); shadow decks show the real track name, also for files without tags, and follow tracks the other player loaded from its streaming browser; the pads stay in HOT CUE when a track loads (`#71`).

> **Pioneer interop status**: the link with the Pioneer CDJ-3000 (library sharing, 3-band waveforms, 60 fps playhead, Master / Beat Sync) was tested between a real Denon SC5000 and [**OpenProLink**](https://github.com/Cd1s/OpenProLink), the maintainer's Pro DJ Link toolkit (a built-in Virtual CDJ-3000 simulator, plus `prolink-probe` to capture the protocol of Pioneer CDJ / XDJ players and DJM mixers). No real Pioneer CDJ-3000 has been on hand yet, so how real Pioneer players behave still needs testing. If you own a Pioneer CDJ-3000, CDJ-2000NXS2, XDJ-AZ / XZ or a DJM-A9 / DJM-V10 / DJM-900NXS2 mixer, real-hardware test reports and device / mixer protocol captures are very welcome at [Cd1s/OpenProLink](https://github.com/Cd1s/OpenProLink) or here, to improve cross-brand libraries, the mixer's Beat FX tempo sync, Quantize and On-Air linking!


---

<a id="gallery"></a>

## On-Deck Screenshots

<table>
<tr>
<td colspan="2" align="center" valign="top">
<img src="./docs/assets/screenshots/4deck-sc5000-cdj3000.png" width="49%" alt="Native 4-deck view with shadow decks"> <img src="./docs/assets/screenshots/opencdj-3000.png" width="49%" alt="OpenProLink Virtual CDJ-3000"><br>
<b>Native 4-Deck View: Two SC5000s + a CDJ-3000 on One LAN</b><br>
<sub>Right SC5000 plays decks 2 and 4; deck 3 is a CDJ-3000 (OpenProLink simulator) in BEAT SYNC, its waveform and tempo followed live over Pro DJ Link</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/01-deck-stems-official-pads.png" width="100%" alt="4-Part Stems Playback"><br>
<b>4-Part Stems & 3-Band Waveforms</b><br>
<sub>Vocal, Melody, Bass, and Drums pad controls on the main view</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/02-library-native-stems-list.png" width="100%" alt="Stems Playlist in Library"><br>
<b>Dedicated Stems List in the Library</b><br>
<sub>All split tracks are organized automatically in the sidebar</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/03-sources-5-streaming-platforms.png" width="100%" alt="7 Streaming Services"><br>
<b>7 Streaming Services in the Source Menu</b><br>
<sub>TIDAL · NetEase · Spotify · Deezer · SoundCloud · Beatport · Apple Music alongside USB & SD</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/04-streaming-qr-login.png" width="100%" alt="Phone QR Login"><br>
<b>Phone QR Code Login</b><br>
<sub>Scan with your phone to sync playlists without typing passwords</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/05-streaming-playlists-bpm-swipe.png" width="100%" alt="Streaming Playlists with BPM & Key"><br>
<b>BPM & Key Preview + Swipe Left for Stems</b><br>
<sub>See track BPM and key before loading, and swipe left to split Stems</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/06b-preferences-device.png" width="100%" alt="Device Settings"><br>
<b>Device Settings Menu</b><br>
<sub>Language, pad LED brightness and power control</sub>
</td>
</tr>
<tr>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/06-preferences-enginex-github-update.png" width="100%" alt="One-Click On-Deck Update"><br>
<b>Settings → About / Update: One-Click Online Upgrade</b><br>
<sub>Tap "Update" on the screen to download and flash new releases with the native progress screen</sub>
</td>
<td width="50%" align="center" valign="top">
<img src="./docs/assets/screenshots/en/07-bluetooth-audio.png" width="100%" alt="Native Bluetooth Audio"><br>
<b>Native Bluetooth Audio Input & Playback Controls</b><br>
<sub>Stream from your phone with track info, play/pause and skip on the deck</sub>
</td>
</tr>
</table>

---

<a id="benchmarks"></a>

## Before vs. After Refactoring (Measured on SC5000)

The entire system has been refactored and slimmed down so all extra features take minimal space and leave more free memory than stock firmware:

| Metric | Stock / Before Refactoring | `EngineX OS 5.3.0.4` (Measured on SC5000) | What It Means for DJs |
| :--- | :--- | :--- | :--- |
| **Firmware Image Size (`*-Update.img`)** | Official stock 5.3.0: `147.79 MB`<br>Earlier custom build: `~210 MB` | **`151.44 MB`** (SC6000 `151.58 MB`, PRIME 4 `151.52 MB`) | **Only `3.64 MB` larger than stock firmware** |
| **Internal Storage Used After Clean Flash** | `~243 MB` left behind by older builds | **`17.2 MB` for core app + 3-language UI** (`45.6 MB` including extra drive format drivers) | Frees up **nearly `200 MB`** of internal flash space |
| **Free RAM Available (out of `2 GB`)** | `~680 – 780 MB` free | **`1,042 MB` free** (main player uses `~150 MB` less RAM; all background services use just **`29.2 MB`**) | **`~260 MB` more free memory** — zero lag during long sets |
| **Audio Core Interruption by Downloads** | `5% – 25%` spikes when downloading or indexing | **`0.0%`**: all downloads and Stems splitting run on a separate background core | **96 kHz audio playback is never interrupted** |
| **Rekordbox USB Reading Speed** | ~8 min first import, 15–24 s every re-plug | **`< 2 seconds` first plug**, **`0.03 seconds`** after that | Plug in your Rekordbox stick and play right away |
| **4-Minute Track Stems Split Time** | Locked / unavailable on SC5000 | **`~35 seconds`** via PC / cloud; **`1.4×` faster than playback** on the deck alone | Split vocals on the fly even mid-set |
| **Sync Across Large BPM Gaps** | Sync cannot reach the other tempo once the gap is wider than the pitch range; widen Pitch Range by hand | **Smart Sync** steps Pitch Range up automatically (up to `±100%`); Bar / Beat / Tempo switch in the Control Center | Sync tracks far apart in tempo without opening settings |
| **Network Speed** | Ethernet limited to `100 Mbps` | Ethernet **`1 Gbps` (1000 Mbps)** full duplex, 8 MiB TCP buffers; Wi-Fi about `9–10 MB/s` (25 MHz SDR12 SDIO bus) | Copy large libraries over the wired port |

---

<a id="install"></a>

## How to Install & Update

### Option 0: EngineX-Installer for All 16 Models

EngineX-Installer discovers supported first- and second-generation decks without entering an IP or selecting a model. It downloads the matching `.img` or data bundle and offers Wi-Fi SSH or USB Fastboot installation.

- [Windows 10/11 x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-windows-amd64.zip)
- [macOS Apple Silicon M1–M4](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-arm64.zip)
- [macOS Intel](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-amd64.zip)
- [Linux x86_64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz)

Extract the archive and start `Install-EngineX.bat`, `Install-EngineX.command`, or `./install-enginex.sh` respectively. Network discovery depends on StageLinQ or the EngineX Web UI; SSH requires root access. Fastboot requires separate tools, including `fastboot` and Python 3; second-generation image creation also needs e2fsprogs. On Windows use the Linux package in WSL with USB access for that path. Back up your deck first: Fastboot can destroy data, particularly when creating a new second-generation `/data` image. First-generation rootfs flashing is experimental and has not been validated across all models.

### Option 1: USB Stick Install (First-Time Setup)
1. Click the link for your exact player model below to download the `.img` file directly (or visit [**Releases**](https://github.com/Cd1s/EngineX-OS/releases/latest)):
   - **Denon DJ SC5000 Prime**: [`SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img)
   - **Denon DJ SC5000M Prime**: [`SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img)
   - **Denon DJ SC6000 Prime**: [`SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img)
   - **Denon DJ SC6000M Prime**: [`SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img)
   - **Denon DJ PRIME 4**: [`PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img)
   - **Denon DJ PRIME 2**: [`PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img)
   - **Denon DJ PRIME GO**: [`PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img)
   - **Numark Mixstream Pro**: [`MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img)
2. Copy the `.img` file to the **root folder of a FAT32 or exFAT USB drive**.
3. Plug the USB drive into your deck (stock Engine OS 4.3.x and 5.0.x both work), open **Settings → About / Update → Firmware Update Mode**, choose **USB/SD** when the deck shows `Found 5.3.0.4 on USB/SD`, and let the player install and reboot. If it only asks *Reboot into firmware update mode?*, the deck did not see the file: check the model, keep the original file name in the root folder, then unplug and replug the drive.

### Option 1b: Second-Generation Models (`/data` Bundle, Beta)

**EngineX-Installer automatic installer (recommended):** finds the deck without entering its IP or choosing its model, using Wi-Fi SSH or USB Fastboot. Download the package for your computer:

- [Windows 10/11 x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-windows-amd64.zip)
- [macOS Apple Silicon M1–M4](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-arm64.zip)
- [macOS Intel](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-macos-amd64.zip)
- [Linux x64](https://github.com/Cd1s/EngineX-OS/releases/latest/download/EngineX-Installer-5.3.0.4-linux-amd64.tar.gz)

1. Update the deck to official **Engine OS 5.3.0**.
2. Extract the package and start `Install-EngineX.bat` on Windows, `Install-EngineX.command` on macOS, or `./install-enginex.sh` on Linux.
3. The installer looks for your deck and downloads its matching bundle. For a stock deck, select **Settings → About / Update → Firmware Update Mode**, reboot and connect USB; Fastboot identifies the model and flashes `/data` after confirmation. For a deck with root SSH enabled, connect both devices to the same network: StageLinQ (UDP 51337) and an EngineX Web UI scan discover it, then the bundle is installed under `/data/enginex` while preserving your library and settings. Discovery may depend on network configuration and StageLinQ availability.
4. After installation, future releases can be installed on the deck using **Settings → About / Update → Update** over Wi-Fi (#86).

SSH requires root access. USB Fastboot requires `fastboot`, Python 3 and e2fsprogs installed separately; on Windows, use the Linux package in WSL as root (`wsl -u root`) with USB access (via `usbipd-win`). **Flashing a new `/data` image erases the data on that partition, including its music, library and settings.** Back up the deck first and disconnect USB drives and SD cards before installation. To preserve `/data`, use `--userdata-backup` with a full image from the same deck. This Beta has not been tested on real hardware.

**SC LIVE 4, SC LIVE 2, PRIME 4+, PRIME GO+, Mixstream Pro+, Mixstream Pro Go, PRIME 4 G2 and Rane SYSTEM ONE** (#61, #68, #69, #76) do not have a hardware eFuse lock; their stock updater (`az01-update` / `az0x-splash`) checks the `AZ0x` container RSA signature (`denon-1` / `rane-1`) in software before writing. On a stock unit, a modified `.img` or a first-generation `.img` (`PRIME4` / `PRIMEGO` / `MIXSTREAMPRO`) is simply rejected in software at the `Verifying` step—nothing is written and the unit is never bricked. For the initial install, use your model's `Data-Bundle.tar.gz` or `EngineX-Installer` (which installs into `/data/enginex` without touching the read-only system partition); once installed, later releases update in one tap from **Settings → About / Update**.

<details>
<summary>Advanced: manual installation and removal</summary>

Download the bundle for your model and check its SHA-256 against `SHA256SUMS.txt` on the Release page:
   - **Denon DJ SC LIVE 4** (JP21): [`SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ SC LIVE 2** (JP20): [`SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME 4+** (JC11S): [`PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME GO+** (JP11S): [`PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Numark Mixstream Pro+** (NH08S): [`MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Numark Mixstream Pro Go** (NH10): [`MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Denon DJ PRIME 4 G2** (JP24): [`PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)
   - **Rane SYSTEM ONE** (RMZ2): [`SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz`](https://github.com/Cd1s/EngineX-OS/releases/latest/download/SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz)

**Method 1 (recommended): install on the unit.** Keeps your music, settings and library. Needs a root shell on the unit (SSH or serial console).

1. Copy the `.tar.gz` to a USB drive and plug it into the unit (or copy it over the network).
2. In the unit's shell, unpack and install it (replace `<model>` and the drive name):
   ```sh
   cp "/media/<USB drive>/<model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz" /tmp/
   tar -xzf /tmp/<model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz -C /tmp
   sh /tmp/<model>-5.3.0.4-EngineX-OS-Data-Bundle/install.sh
   reboot
   ```
3. After the reboot Engine starts once in stock form, then restarts by itself with EngineX OS. `Settings → Device` then shows the EngineX entries (`Language`, `Power → Turn Off`, …).

**Method 2: fastboot flash from a computer.** Only if you have no shell on the unit. Needs a Linux computer (or WSL as root) with `python3`, `e2fsprogs` (`debugfs`) and `fastboot`.
- **Recommended (EngineX-Installer)**: Run the installer. It detects the deck in fastboot, formats a fresh `/data` partition automatically, injects the bundle and flashes it (**music, library and settings stored on the deck's `/data` are erased**, and the installer asks you to type `yes`; USB drives and SD cards are unaffected). If you already have a full `/data` backup of this same deck, pass `--userdata-backup <image>` to keep them. Reverting to stock later removes only the bundle and does not depend on a pre-install backup.
- **Manual steps (requires a `/data` backup)**:
  1. Keep your backup untouched and work on a copy: `cp my-userdata-backup.img userdata-enginex.img`
  2. Unpack the bundle on the computer and inject it into the copy (the two helper scripts are inside the bundle):
     ```sh
     tar -xzf <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz
     python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-userdata.py userdata-enginex.img --bundle <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz
     ```
  3. Put the unit into fastboot mode, connect it over USB, then flash and reboot:
     ```sh
     sh <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-fastboot.sh userdata-enginex.img
     ```

**Method 3: SYSTEM ONE / PRIME 4 G2 without fastboot (USB `1cc5:2027`).** Open `Settings → About / Update → Firmware Update Mode` → `Options` and connect USB, then run EngineX-Installer; it does the steps below. This writes a **new, empty** `/data` partition: music, library and settings on the internal storage are erased (USB drives and SD cards are not). By hand (needs `python3`, `e2fsprogs` and `pip3 install pyusb`):
   ```sh
   mke2fs -t ext4 -O encrypt -L data -b 4096 -F data.img 64M
   python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-userdata.py data.img --bundle <model>-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz --ssh-key ~/.ssh/id_ed25519.pub
   python3 <model>-5.3.0.4-EngineX-OS-Data-Bundle/host-install-usbupdate.py data.img
   ```
   `--ssh-key` is optional. Then press the power button to turn the unit off and on again.

**Uninstall**: `sh /data/enginex/bundle/uninstall.sh && reboot` removes only what the bundle added.

</details>

> **PRIME 4 G2 / SYSTEM ONE (64-bit Engine)**: the UI packs, the streaming gateway (Deezer, SoundCloud, Beatport) and the EngineX services are in. It also carries the 18 Engine binary patches (including the stock Apple Music unlock), the 14 audio and transition DSP guards, the 0 dB Master Soft Clipper, auto gain, Echo Out on the up-fader, SQLite tuning and the 64-bit `OfflineAnalyzer` `.perf` injection (#149). The 4-deck unlock is for the stand-alone players only, and Stems stay as the official firmware has them. This Engine needs the unit's secure element (OP-TEE) to start, so the sandbox checked the hook, the edits and the pack loading but not the screens.

### Option 2: One-Click On-Deck Update (When Already Running EngineX OS)
- Connect your deck to Wi-Fi or Ethernet and open **Settings → About / Update**;
- Tap **Update** on the screen and confirm — the deck will automatically launch the full-screen updater, download the new image, and reboot.
- **Second-generation models** (SC LIVE 4 / 2, PRIME 4+, PRIME GO+, Mixstream Pro+ / Pro Go, PRIME 4 G2, SYSTEM ONE): once the `/data` bundle is installed, later releases (for example `5.3.0.4`) are found and installed the same way from **Settings → About / Update**. The deck downloads that release's `Data-Bundle.tar.gz`, checks `SHA256SUMS`, swaps the files in one step and restarts the EngineX services; the system partition is never touched (#86).

### Optional: High-Quality Neural Stems with Your Computer
On-deck separation uses a fast basic spectral split. For high-quality neural Stems (`htdemucs` on GPU / Apple MPS / CPU), unzip `EngineX-Installer` on a Mac or PC on the same Wi-Fi / LAN as your deck and run one command (also available under `tools/stems/` in a source checkout; append `hpss` for lightweight spectral mode). Your deck discovers the computer automatically (UDP 17891 / mDNS, no IP address or password setup) and shows `High-quality stems: <host> (htdemucs)` in **Profile → Library** and **Settings → About / Update**:
```sh
# macOS / Linux (from unzipped EngineX-Installer; or tools/stems/run-home-worker.sh)
./stems/run-home-worker.sh

# Windows PowerShell (from unzipped EngineX-Installer; or tools\stems\run-home-worker.ps1)
powershell -ExecutionPolicy Bypass -File .\stems\run-home-worker.ps1
```

### Streaming Login: the Deck Alone Is Enough
No computer and no server are needed. Put the deck and your phone on the same Wi-Fi, open a service in the Source menu and scan the QR code on the screen. The login page on your phone is in English by default and switches to 简体中文 or 繁體中文 at the top right; every page has a step-by-step guide.

| Service | What you do |
| --- | --- |
| NetEase Cloud Music | Scan the deck's QR code with the NetEase Cloud Music app and confirm. If the code expires, tap it for a new one. |
| Spotify | **Premium required for audio.** Pick **`EngineX OS`** in Spotify Connect on the same LAN, or scan the deck QR to open Spotify's official Device Auth confirmation directly. No custom Client ID or loopback URL pasting is needed in the normal flow. Native AES-decrypted Ogg Vorbis prefers 320 kbps, then 160/96 kbps; pure-Go BPM/key/waveform analysis imports it into the Engine library. No Deemix fallback. |
| Deezer | Paste your **ARL** (the guide shows how to copy it from deezer.com on a computer). The same login unlocks lossless FLAC downloads (Deemix) for Deezer tracks; NetEase tracks use it only if you pick *Deemix First* for NetEase. Spotify, Beatport and Apple Music play from themselves with no fallback to another service; SoundCloud only borrows a signed-in Deezer / NetEase / Beatport for Go+-only and ordinary single tracks when you have no Go+ (see the SoundCloud row). A profile link alone shows public playlists without downloads. |
| SoundCloud | Enter your username or profile URL. No password, Artist Pro or API app needed. For private playlists, paste the `oauth_token` from soundcloud.com under *Advanced*. Each track plays at the best quality it offers: the uploader's original where downloads are allowed, then 256 kbps AAC, 160 kbps AAC, 128 kbps MP3. Go+-only tracks and ordinary single tracks (not DJ sets over 10 minutes) play SoundCloud's own stream when a Go+ account is bound; without Go+ they are matched on a signed-in Deezer / NetEase / Beatport instead (cached as `soundcloud-<id>.*`). |
| Apple Music | Engine 5.3.0's own Apple Music source is unlocked and plays from Apple Music itself; EngineX no longer looks the track up on Deezer or NetEase. On your phone you can still paste a playlist link or add a `media-user-token` to browse your library. |
| Beatport | Enter your Beatport email and password. The deck keeps only the login token. |

### Optional: Your Own Login Relay
The relay is `enginex-relay` (the old name `sc5000-relay` is still accepted). A relay lets a phone on mobile data finish logins and adds Deezer's in-browser login. Run it on any server with a public HTTPS address, then enter its address and password in the deck's web console (`http://<deck-ip>:8080`):
```sh
go build -o enginex-relay ./cmd/sc5000-relay
./enginex-relay -public https://relay.example.com -listen 0.0.0.0:6595
# the first console password is written to /var/lib/sc5000-relay/initial-password
```

**No domain? One-click public access with Cloudflare Quick Tunnel.** You do not need to buy a domain or set up a reverse proxy:
1. **On the deck**: open the console at `:8080` and click **Enable domain-free public tunnel (Cloudflare Quick Tunnel)**. The deck installs and starts `cloudflared` for its architecture and, within seconds, gives you an `https://*.trycloudflare.com` address. Open it from your phone on 4G/5G to reach the console remotely; the streaming-login QR codes on the deck screen then use that public address for their callback automatically.
2. **Self-hosted `enginex-relay`**: run `enginex-relay -public auto` (or `run-home-worker.sh --with-tunnel`). It opens a Cloudflare Quick Tunnel by itself, prints a public HTTPS relay address, and manages all your decks from it.

The `trycloudflare.com` address changes whenever the tunnel restarts; use your own domain if you need a fixed one.

---

## Security: Playing on Public Networks

EngineX OS is built to be safe on a club, bar or festival Wi-Fi shared with strangers.

- **No default SSH password.** Password login is switched off completely, and the public firmware ships with **no SSH key at all**, so SSH stays closed until you add your own key. Decks that ran an earlier public image have the key those images carried removed automatically when they update to 5.3.0.1.
- **Nobody on the same Wi-Fi can control your deck.** Everything the deck uses internally (playing, waveform and language settings, online update, streaming logins, Stems) only answers the deck itself (`127.0.0.1`). Someone else on the network cannot switch your tracks, change settings or restart the player.
- **The phone web console needs your deck's own access code.** The page at `http://<deck-ip>:8080` asks for a random code generated on your deck. The address and code are shown on the deck under `Settings → About / Update`. Streaming QR logins use one-time links that expire and never open the console.

For advanced users:

- To use SSH, put your public key in `/data/enginex/authorized_keys` (for example with a private build of the image), then reboot. Key login only, no passwords.
- The console access code is stored in `/data/enginex/state/access-token`. Delete that file and restart the deck to get a new one.
- If you play on a network you do not trust, you do not need Wi-Fi for USB, SD or Bluetooth playback: leave the deck offline.

---

<details>
<summary><b>Developer & Build Notes (Not needed for DJs)</b></summary>

<br>

- Full 642-issue historical audit manifest: [`docs/OPEN-SOURCE-FULL-MANIFEST.md`](./docs/OPEN-SOURCE-FULL-MANIFEST.md)
- Stress test and fix log: [`docs/ISSUE-FIXES-2026-11-04.md`](./docs/ISSUE-FIXES-2026-11-04.md)
- Contribution and release guide: [`CONTRIBUTING.md`](./CONTRIBUTING.md)

```sh
# Run test suites
go test -race ./...
bash tests/test_rootfs_overlay.sh

# Build the 5-in-1 static ARMv7 suite and C hooks
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/sc5000-suite ./cmd/sc5000-suite
sh tools/ui-mod/build.sh
sh tools/edisksd-hook/build.sh
```

</details>

---

<a id="sponsor"></a>

## Sponsor & Support

If **EngineX OS** makes your decks more fun to play on or saves you prep time before a gig, consider supporting the project or leaving a **Star** on the repo!

<table>
<tr>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-wechat.jpg" height="260" alt="WeChat Appreciation QR Code"><br>
<b>WeChat Pay</b><br>
<sub>Scan with WeChat (Cheek's 讚賞碼)</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-binance-pay.jpg" height="260" alt="Binance Pay QR Code"><br>
<b>Binance Pay</b><br>
<sub>Scan QR or send to Binance ID: <code>565380652</code> (0 fee)</sub>
</td>
<td width="33%" align="center" valign="top">
<img src="./docs/assets/sponsor-evm-usdt.jpg" height="260" alt="USDT EVM QR Code"><br>
<b>USDT / USDC (All Binance EVM Chains)</b><br>
<sub>BSC · ERC20 · Arbitrum · Optimism · Polygon · Base · opBNB</sub>
</td>
</tr>
</table>

> **Binance ID (UID — 0 fee via Binance Pay, click copy on the right):**
> ```text
> 565380652
> ```
>
> **USDT / USDC Wallet Address (Supports all Binance EVM networks: `BSC (BEP-20)` · `Ethereum (ERC-20)` · `Arbitrum One` · `Optimism` · `Polygon` · `Base` · `opBNB` · `Avalanche C-Chain` · `zkSync Era` · `Scroll` · `Linea`):**
> ```text
> 0x35e0c53dd887b37cd5db0064291f7c5a7442bc91
> ```

---

## Star History

<div align="center">
<a href="https://star-history.com/#Cd1s/EngineX-OS&Date">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=Cd1s/EngineX-OS&type=Date&theme=dark" />
    <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=Cd1s/EngineX-OS&type=Date" />
    <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=Cd1s/EngineX-OS&type=Date" width="100%" />
  </picture>
</a>
</div>

---

<div align="center">
<sub>MIT License · EngineX OS is an independent community open-source project and is not affiliated with or endorsed by inMusic Brands, Denon DJ, or Numark. No copyrighted stock firmware binaries are distributed in this repository.</sub>
</div>
