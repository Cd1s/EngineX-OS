## EngineX OS 5.3.0.4

Custom system rebuilt on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units — now unified on per-model `*-Update.img` release files across all 8 first-generation models (`SC5000`, `SC5000M`, `SC6000`, `SC6000M`, `PRIME 4`, `PRIME 2`, `PRIME GO`, `Mixstream Pro`) and all 8 second-generation models (`SC LIVE 4`, `SC LIVE 2`, `PRIME 4+`, `PRIME GO+`, `Mixstream Pro+`, `Mixstream Pro Go`, `PRIME 4 G2`, `Rane SYSTEM ONE`, unlocked once over USB via `EngineX-Installer` and updated from a USB stick or on-screen OTA from then on), alongside the command-line `EngineX-Installer` for Windows, macOS, and Linux.

- **Online User Guide (English / 简体中文 / 繁體中文)**: [https://enginexdj.com](https://enginexdj.com)

---

### 5.3.0.4 r2 — fixed re-release (2026-11-12)

`5.3.0.4 r2` is the same version with fixes. The deck still shows `5.3.0.4`, the file names are unchanged, and all 28 files below come from the r2 build.

**Decks already on `5.3.0.4` receive r2 as a normal on-screen update** (`Settings → About / Update`). Starting with this build the update check compares the Release tag (`v5.3.0.4-r2`) with the tag built into the installed system, so r2 is offered once and not again after it is installed. Older versions update straight to r2.

**Fixed in r2**

- **Mac-formatted (HFS+) USB sticks**: since `5.3.0.3`, journaled HFS+ sticks (always mounted read-only) lost their writable `Engine Library` view, so a rekordbox export on such a stick was never converted and showed no playlists. They work again. `APFS` sticks are not supported; the user guide now says so.
- **#216 — rekordbox 7 USB exports**: reads rekordbox 7's encrypted `exportLibrary.db`, including Hot Cues, Memory Cues, and loops. Track titles and playlist names in Chinese, Japanese, Korean, and other scripts that could come out garbled are imported intact, and the temporary copy of a large library is kept in device storage instead of RAM.
- **#182 — Playlists on the first insert**: a rekordbox stick is converted before Engine sees the drive, so its playlists appear the first time it is inserted, with no eject and re-insert. Two sticks inserted together are both converted, and drives without a rekordbox export mount as fast as before.
- **No `Choose your Database` dialog**: inserting a rekordbox stick never asks. The converted `Engine Library` is used when it holds tracks; otherwise the stick opens through Engine's own rekordbox reading and switches to the `Engine Library` on a later insert once converted.
- **Faster first import** of large rekordbox libraries.
- **Follow-ups to the `/data/enginex` move**: services that use the data folder now start after it is prepared (a blank or freshly updated deck could come up with the stock UI for one boot); a streaming download that was just saved is no longer removed again when storage is nearly full; `EngineX-Installer --uninstall` also removes a 2nd-gen install that predates `5.3.0.3`.

**New in r2**

- **#209 — DMX Studio follows the MASTER deck's beat**: the `dmx-studio` Mod reads the MASTER deck's beat, BPM, and play state directly, without opening a panel on the deck, and adds phrase scenes and MIDI clock output.

**Known effect**

- HFS+ library copies made by earlier builds are not reused: the first insert on r2 converts the stick again, and cue edits made on the deck that lived only in the old copy are not carried over.

---

### New

- **#215 — Unified `*-Update.img` for All 8 Second-Generation Models & One-Click Unlock in `EngineX-Installer`**: All 16 standalone models now ship unified `<PREFIX>-5.3.0.4-EngineX-OS-<Model>-Update.img` release packages (with `*-Data-Bundle.tar.gz` also published alongside them in `5.3.0.4` / `5.3.0.5` so older `5.3.0.2` / `5.3.0.3` Gen-2 decks one-tap OTA upgrade seamlessly on screen). On a stock 2nd-gen unit (`SC LIVE 4`, `SC LIVE 2`, `PRIME 4+`, `PRIME GO+`, `Mixstream Pro+`, `Mixstream Pro Go`, `PRIME 4 G2`, `Rane SYSTEM ONE`), connect over USB in `Firmware Update Mode` and run **`EngineX-Installer`** once — it automatically performs **`[1/2] One-Click Unlock`** of the `.img` update chain and **`[2/2] Flashes *-Update.img`** into `/data/enginex` without modifying the read-only stock system partition. Once unlocked (existing 2nd-gen EngineX installs unlock automatically on boot), every subsequent update installs straight from a `FAT32` / `exFAT` USB stick or one-tap on-screen OTA (**`Settings → About / Update → Update`**) just like 1st-gen units, with no computer needed, and survives Factory Reset.
- **#210 — Native Spotify Audio & Zero-Config Login**: Spotify Premium tracks play straight from Spotify as 320 kbps Ogg Vorbis (or 160 / 96 kbps when unavailable), with BPM, key, and waveforms imported into the Engine library and no fallback to any other streaming service. Sign in by picking **`EngineX OS`** in Spotify Connect on the same Wi-Fi / LAN, or scan the on-screen QR code to approve on Spotify's official device page — no Developer App or Client ID setup required (legacy Web API tokens need one fresh pairing or QR scan).
- **#201 — `Linked View` Selector & `Paired Player` 2-Deck View (`SC5000 / SC6000`)**: `Layout → ENGINEX VIEW PRESET → Linked View` offers `Stock` / `Paired Player` / `4 Decks` on standalone players and `Stock` / `4 Decks` on 4-deck all-in-one consoles, replacing the separate switches so returning to `Stock` is always one step away (with a hint under `Stock` reminding standalone-player DJs that `Shift + Layer` or `Layout → PERFORMANCE LAYOUT → Deck Layout` switches single and dual layer views). `Paired Player` shows two full-height lanes on two linked `SC5000 / SC6000` players: this player's active `Layer A / B` on top and the partner player's active layer below as a live shadow lane that follows the partner's `Layer` button (`No paired player` when unlinked; requires `4-Deck Role` `Auto`, `1+3`, `2+4`, `1`, or `2`). Switching between `Stock` and the linked views restarts Engine once; switching between `Paired Player` and `4 Decks` is live with no restart.
- **#49 — `Shadow Deck Mode` (`Auto` / `All Engine Mode` / `All CDJ Mode`) with Live Switching**: In 4-deck and Paired Player views, `Layout → ENGINEX VIEW PRESET → Shadow Deck Mode` defaults to `Auto`: copyable tracks from another EngineX deck or a CDJ's USB/SD drive load locally (muted) and render with Engine's own waveform, beat grid, overview, `Slip` timeline, `Auto Loop`, `Key Offset`, `Stems` icon, green `Sync` `BPM`, and `LEAD` badge, showing track info and a progress ring while copying. Uncopyable CDJ sources (streaming, rekordbox Link, CD, or 2nd USB) automatically display CDJ waveforms with a one-line status reason and stock-matching deck headers. Switching `Shadow Deck Mode` takes effect live without restarting Engine or interrupting local playback.
- **Pro DJ Link CDJ-3000 Waveforms on All-in-One `Line In` Channels**: Switching a mixer channel input to `Line In` on all-in-one consoles (such as `PRIME 4` and `PRIME 2`) maps connected Pioneer CDJ-3000 waveforms and live playheads onto that channel in player-number order.
- **#168 — `PRIME 4` Hardware FX Selector PX Effects**: Adds `Spiral (PX)`, `Pitch Echo (PX)`, `Slip Roll (PX)`, `Dub Echo (PX)`, and `Space (PX)` to the `PRIME 4` (`JC11`) hardware FX selector with click-free switching and independent 11-step `Time/Rate`, `Parameter`, and `Wet/Dry` controls per FX unit.
- **#162 — PX Delay Time in Milliseconds (`10–4000 ms`)**: `Profile → Playback → PX Delay Time Mode` (synced with the `:8080` web console; off by default) switches the three delay-based PX effects (`Spiral`, `Pitch Echo`, `Dub Echo`) on all-in-one consoles to a shared BPM-independent `10–4000 ms` delay (`Time/Rate`, `Time (ms)` readout, and `−/+` buttons in `10 ms` steps on `PRIME 4`, or `PX Delay Time (ms)` presets on other consoles), while stock Beat FX remain beat-synced.
- **#171 — `SC LIVE 4` AUX Routing to Channel 3 / 4**: `Mixer Menu → Channels → Input` on `SC LIVE 4` routes the physical `AUX` input through Channel 3 or 4 for channel `CUE`, fader, EQ, and Sweep FX control while keeping `MIC 2` on its stock path.
- **#166 — `Mixstream Pro / Pro+ / Pro Go` Physical `FILTER` Sweep FX & `Beat Jump Pad Bank`**: Physical `FILTER` knobs control native `Filter`, `Dub Echo`, `Noise`, `Filter Roll`, and `Wash` via each deck's `SWEEP FX` carousel; touch Beat Jump is restored alongside `Layout → ENGINEX VIEW PRESET → Beat Jump Pad Bank` and per-deck `EQ / STEMS` knob switching.
- **#137 — USB MIDI Controller Mapper Mod (`midi-controller-mapper`)**: Optional official Mod in the Mod Hub (with a built-in **Pioneer CDJ-400** USB `PC MODE` preset plus custom JSON presets) mapping an external USB MIDI controller directly to internal `Deck 3` or `Deck 4` in 4-deck mode without switching hardware layers, including continuous jog wheel control, pitch fader soft takeover with on-screen `▲▼` arrows, and unplug auto-stop.
- **Computer High-Quality `htdemucs` Stems Default, On-Deck Status Hint & Bundled Worker**: Computer-assisted LAN Stems now defaults to the high-quality `htdemucs` neural model, displays live status under `Profile → Library` and `Settings → About / Update` (`High-quality stems: <host> (htdemucs)`), and bundles ready-to-run `stems/` scripts inside every `EngineX-Installer` archive.
- **Web Console Verbose Logging & Redacted Log Download**: Adds a `Verbose logging` switch (off by default) and one-click `Download logs` button in the `:8080` web console, saving the last 30 minutes of logs with access codes, tokens, passwords, and credentials automatically removed — no SSH required.
- **Multi-Deck Relay Hub & Zero-Domain Cloudflare Quick Tunnel**: Optional `enginex-relay` adds a `Connected decks` status dashboard across linked players, and both `:8080` on the deck and `enginex-relay` can open a temporary public HTTPS address via Cloudflare Quick Tunnel for mobile QR streaming logins over 4G/5G without a custom domain.

---

### Fixed

- **#84 / #206 — Recording with Streaming Tracks on All-in-One Consoles**: All-in-one consoles with a built-in recorder (`PRIME 4 / 4+ / 4 G2`, `PRIME 2`, `PRIME GO / GO+`, `SC LIVE 4 / 2`, `Mixstream Pro / Pro+ / Pro Go`, `SYSTEM ONE`) can now start recording while a streaming track is loaded and continue loading streaming tracks while recording is active. *(Streaming services' terms may not allow keeping recordings of their tracks; keep recordings for personal use.)*
- **#178 — Rekordbox MP3 Beat Grid, Cue & Loop Alignment**: Compensates for MP3 encoder start delay (`Xing` / `Info` and `LAME` headers, ~26–51 ms at 44.1 kHz) when importing Rekordbox USB sticks and copying CDJ shadow tracks so beat grids, Hot Cues, Memory Cues, and Loops align with audio transients; WAV, FLAC, AIFF, and unaffected MP3s stay untouched, and previously imported USB sticks re-convert affected MP3s once automatically.
- **#182 (PR #183) — Legacy rekordbox 5 USB Library Compatibility**: Imports legacy rekordbox 5 Device Library USB sticks cleanly even when tracks contain unanalysed BPM values, orphaned playlist entries, duplicate playlist names, or broken parent folders.
- **#208 / #134 — Official Mods `1.1.0` Run Without Python & Upgrade In Place**: Rebuilds all 5 official Mods (`dmx-studio`, `jog-logo-studio`, `sampler-pad-studio`, `minimal-club-mode`, `midi-controller-mapper`) with self-contained static Go helpers for both 32-bit and 64-bit decks so they start reliably on all 16 models (which ship without Python), drives uDMX USB interfaces directly without extra system libraries, and upgrades installed `1.0.0` Mods in place when `1.1.0` is uploaded in `:8080` while keeping settings and enabled state.
- **#202 (PR #203) & #170 — Web Console Mod Hub on LAN & Mod Executable Permissions**: Fixes `403` errors when browsing or uploading Mods in `:8080` from another device on the LAN (#202), and preserves executable permissions on helper binaries and scripts inside `.tar.gz` Mod archives (#170).
- **#140 — Boot Recovery Safety Net & Power-Cut Protection**: If Engine stalls during startup (180 s without progress) or crashes 3 times within 5 minutes, the deck automatically falls back to stock Engine with Mods quarantined — never triggering Factory Reset or touching your music library — and exports a redacted boot report to any writable USB stick or SD card (with public recovery status and `Retry EngineX` on `:8080`). Also clears interrupted UI caches cleanly on the second boot after an abrupt power cut.
- **#49 — Standalone Player `Sampler Output` (`Layer A / Layer B`) & Stock 4-Deck Controls**: `SC5000 / SC6000` players keep `Settings → Mixer → Sampler Output` selectable between `Layer A` and `Layer B` in both 2-deck and 4-deck views, alongside `Settings → Device → Player #`, dual-layer `Profile → Deck Colors` (`Layer A / B`), `Layout → DECK BEHAVIOR → Layer B`, `Browse` encoder press-to-load into the active layer, `Engine Connect`, and `Touch FX` `Mute`.
- **#49 — Shadow Deck `Sync` Alignment, `>+100%` Tempo Tracking, `MASTER` Hand-Off & Clean Unload**: Silent shadow slots never steal `MASTER` when a real deck pauses or unloads; shadow `Sync` locks phase smoothly and follows synced source decks even above the `+100%` pitch fader limit; powering off or ejecting on the partner player unloads the shadow lane cleanly without blocking streaming source switches; streaming shadow tracks show real `Artist - Title` metadata; and paused shadow detail waveforms render cleanly without blank frames or stretched overviews.
- **#49 / #151 — Steady Deck Header Row 2 & Whole `Cue Countdown`**: Locks pitch range and pitch percentage column widths in compact deck headers so moving the pitch fader or switching `Pitch Range` never shifts neighbouring items, keeps the `Hot Cue Countdown` capsule in a fixed slot with whole `BARS.BEATS` numbers, prevents `Auto Loop` / `Beat Jump` readouts from crowding track titles, and applies `Settings → Device → Hide Title/Artist` across both local and shadow deck headers.
- **#135 — Stock `Link Beat Jump / Loop Size Controls` Unlocked & `Swap Loop / Beat Jump` Fix**: Unlocks Engine's stock `Profile → Cues/Loops → Link Beat Jump / Loop Size Controls` setting across all models without ever overwriting your saved `On` / `Off` preference, and fixes `Layout → ENGINEX VIEW PRESET → Swap Loop / Beat Jump` on `SC5000`.
- **#85 — Perceptual RGB Pad Dimming & Stock Single-Color Key Brightness**: `Settings → Device → Pad LED Brightness` (`100%`–`20%`) dims RGB LEDs (pads, mode buttons, `Layer`, platter rings, and on-screen pads) smoothly while keeping single-color hardware keys (`SYNC`, `KEYLOCK`, `SLIP`) at stock brightness.
- **#105 & #150 — OmniSource Playlist Isolation, SoundCloud Fallback & Spotify Connect Discovery**: Keeps each streaming service's own playlist page free of USB and cross-service playlists while unified search still spans everything (#105); keeps SoundCloud `Liked Tracks & Sets` loading when an OAuth token expires and advertises Spotify Connect reliably over both Ethernet and Wi-Fi (#150); isolates per-service download caches (sharing lossless cache between Deezer and *Deemix First* NetEase), preserves Beatport lossless upgrades on cold cache, and matches non-set SoundCloud tracks when no Go+ subscription is bound.
- **#175 — Full-Screen Boot & Shutdown Screens Under `UI Scale`**: Boot splash and shutdown overlays stretch cleanly across the full display when `UI Scale` is below 100% (down to 75%) on all 16 models.
- **#176 & #177 — `SC LIVE 4` & 2nd-Gen On-Deck Stems Detection & Cache Alignment**: Tapping **Generate Stems** on read-only-rootfs 2nd-gen decks (such as `SC LIVE 4`) detects the bundled Stems processor instead of jumping to Settings (#176), and 2nd-gen data bundles align shader and UI cache limits with 1st-gen firmware (#177).
- **#118 — First-Gen Boot Splash & First-Boot Stems Prompt**: Embeds the EngineX boot splash across all 8 first-generation `*-Update.img` images (#118) and suppresses the stock first-boot Stems download prompt on first-generation units.
- **#161 / #164 / #169 — `EngineX-Installer` USB & LAN Discovery Across Gen-1 and Gen-2**: Fixes Windows USB discovery for `SC LIVE 4`, adds `Mixstream Pro+`, `Mixstream Pro Go`, `PRIME 4+`, and 1st-gen Computer Update Mode USB detection, recognises installed decks behind the `:8080` access code, unpacks images on macOS/Windows without Linux filesystem utilities, documents the Windows `WSL 2 + usbipd-win` workflow, and adds read-only diagnostic helpers.
- **Time Sync & Offline Time Zone Retention**: `Settings → Device → Time/Date → Sync Time` uses mainland-China and global NTP pools, keeps the system clock advancing across power cuts on units without a battery-backed clock, and preserves your chosen `Settings → Device → Time Zone` (including manual `UTC`) across offline reboots.
- **Complete `zh-Hans` / `zh-Hant` Settings Translations & Smoother UI**: Completes Simplified and Traditional Chinese translations across all deck Settings tabs and option values (keeping `Layer A`, `Layer B`, and `3-Band` in English), merges the track-swipe `Stems` action into a single localized button, and eliminates brief UI stutters when pressing `MASTER` / `SYNC`.

---

### Changed & removed

- **Phased Gen-2 `*-Update.img` unification (`#215`, 28 release artifacts)**: `5.3.0.4` publishes all 16 `*-Update.img` files (8 Gen-1 + 8 Gen-2), 8 legacy-OTA `*-Data-Bundle.tar.gz` archives, and 4 `EngineX-Installer` archives (`28` artifacts + `SHA256SUMS.txt`). `EngineX-Installer` and unlocked Gen-2 decks use `*-Update.img` by default, while `*-Data-Bundle.tar.gz` stays in `5.3.0.4` and `5.3.0.5` so older `5.3.0.2` / `5.3.0.3` Gen-2 decks can one-tap OTA upgrade on screen before `*-Data-Bundle.tar.gz` is retired in `5.3.0.6+`.
- **Pioneer-style (PX) effects removed from standalone players (`SC5000 / SC5000M / SC6000 / SC6000M`)**: `Color FX Style` and the `PX Delay` rows are hidden under `Profile → Playback` on standalone players and any previously saved value acts as `Off`; all-in-one consoles keep them.
- **`Auto-Analyze Drives` row removed**: Removed the non-functional `Auto-Analyze Drives` entry from `Profile → Library` and the `:8080` web console.
- **Jog wheel & `Browse` / zoom controls back to stock (`#147`, `#157`)**: Restored stock `Shift + Vinyl`, Beat Grid Edit jog touch, and `Browse` encoder zoom steps and hold actions.
- **Non-master `MASTER` button LED back to stock**: Restored Engine's stock dim brightness on non-master `MASTER` buttons across all models (`Jog Light Mode` also removed).
- **`Waveform Style` `3-Band` and `Layer A / Layer B` stay in English**: Displayed as `3-Band`, `Layer A`, and `Layer B` in Simplified Chinese, Traditional Chinese, and English.
- **Standalone DMX512 moved out of core OS (`#134`)**: Multi-universe USB / Art-Net / sACN DMX lighting now ships as the optional `dmx-studio` Mod (`1.1.0`, zero memory or background processes when disabled). Stock **Engine Lighting** remains untouched and off by default.
- **Python removed from official Mods (`#208`)**: Official Mods (`1.1.0`) use static Go helpers instead of Python; custom Mods with helpers must ship static `armv7` / `arm64` binaries.
- **Menu & runtime tidy-up**: `Live Stream` lives under `Settings → Services` (removed from the Control Center to prevent accidental mid-set taps), `Waveform Style` is no longer duplicated under `Layout`, and runtime files live under `/data/enginex` (with `/data/sc5000-custom` kept as a compatibility symlink).

---

### Not in this release

- **#181** — Pitch fader pickup / takeover changes (on hold; stock pitch takeover behavior is kept).
- **#204** — Serato waveform style.
- **#205** — Copying or syncing settings across linked decks.
- **On-deck touchscreen Mod panels** — Mods are managed from the `:8080` web console in `5.3.0.4`; on-deck touchscreen Mod panels are planned for `5.3.0.5` (alongside `#172`, `#195`, and stock Beat FX millisecond mode).

---

### How to update

- **Already running EngineX OS (`5.3.0.4`, `5.3.0.3`, `5.3.0.2`, `5.3.0.1.x`, or `5.0.4.2`) — all 16 1st-gen & 2nd-gen models**: Connect the deck to Wi-Fi or Ethernet, open **`Settings → About / Update`**, and tap **`Update`** on screen (1st-gen models and unlocked `5.3.0.3` 32-bit 2nd-gen models upgrade via `*-Update.img`; `5.3.0.2` and locked-`5.3.0.3` 2nd-gen models automatically pick up the `*-Data-Bundle.tar.gz` included in this release and unlock `.img` updates on boot, while preserving your library, streaming accounts, saved layout views, and Mods). *(Note: if your 2nd-gen deck is on `5.3.0.3` `PRIME 4 G2` / `SYSTEM ONE` with `gen2-unlocked` set, or already installed the earlier same-day `5.3.0.4` `b536671` build, run `EngineX-Installer-5.3.0.4-*` once over LAN SSH or USB to activate the `#215` `.img` update chain.)*
- **First-time install on 1st-gen (`SC5000 / SC5000M / SC6000 / SC6000M / PRIME 4 / PRIME 2 / PRIME GO / Mixstream Pro`)**: Download your model's `*-Update.img` below, place it unrenamed in the root of a `FAT32` or `exFAT` USB drive, plug it into the deck, and open **`Settings → About / Update → Firmware Update Mode → Options → USB/SD`**.
- **First-time install on a stock 2nd-gen unit (`SC LIVE 4 / SC LIVE 2 / PRIME 4+ / PRIME GO+ / Mixstream Pro+ / Mixstream Pro Go / PRIME 4 G2 / Rane SYSTEM ONE`)**: Update to official **Engine OS 5.3.0** first, download `EngineX-Installer-5.3.0.4-*` for your platform, extract the archive, and run the command-line launcher (`Install-EngineX.bat` on Windows, `Install-EngineX.command` on macOS, or `./install-enginex.sh` on Linux) with the deck in **`Firmware Update Mode`** over USB (or over LAN / Wi-Fi if root SSH is already open). It automatically runs **`[1/2] One-Click Unlock`** of the `.img` update chain and **`[2/2] Flashes *-Update.img`** (writing a fresh 2nd-gen `/data` image over USB erases internal `/data` storage — back up first and unplug USB/SD drives; on Windows 2nd-gen USB installs, forward the USB device into WSL 2 with `usbipd-win` and run the Linux installer as root via `wsl -u root`; for `PRIME 4 G2` and `SYSTEM ONE`, power-cycle the deck after USB writing finishes). Once unlocked, every future update works directly from a USB stick or on-screen OTA with no computer needed!

---

### SHA-256 Checksums (28 Release Artifacts)

#### One-Click Installer (`EngineX-Installer`)

| Platform | File | SHA-256 |
| --- | --- | --- |
| Windows 10/11 x64 | `EngineX-Installer-5.3.0.4-windows-amd64.zip` | `9d60850bde68d00072cefb670f088efed50219f9c4ab3546f617cb282c9ff9cc` |
| macOS Apple Silicon (M1–M4) | `EngineX-Installer-5.3.0.4-macos-arm64.zip` | `f9eb9b5f0cf96cba2dc14fecd608d9e9c87c41ea3776518132bad16a02df50dd` |
| macOS Intel | `EngineX-Installer-5.3.0.4-macos-amd64.zip` | `a8bd9ea3b8cc6f6d032b1c48f2a5ec3898499b607376e6f3b93b3b8afe35c670` |
| Linux x86_64 | `EngineX-Installer-5.3.0.4-linux-amd64.tar.gz` | `fcc8f10d06d34ef1823b873d49f80dc7ad77abcb50045b4b90eda02b686be69f` |

#### First-Generation Firmware Images (`*-Update.img`)

| Model | Board | File | SHA-256 |
| --- | --- | --- | --- |
| Denon DJ SC5000 Prime | JP07 | `SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `b124a02777a556f97ab7e9f04600ca09cb72de00e613ce1a2d00d7d410de11de` |
| Denon DJ SC5000M Prime | JP08 | `SC5000M-5.3.0.4-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `b124a02777a556f97ab7e9f04600ca09cb72de00e613ce1a2d00d7d410de11de` |
| Denon DJ PRIME GO | JP11 | `PRIMEGO-5.3.0.4-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `b124a02777a556f97ab7e9f04600ca09cb72de00e613ce1a2d00d7d410de11de` |
| Denon DJ SC6000 Prime | JP13 | `SC6000-5.3.0.4-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `16f1f01cb466eb3d20ccbc021eb9b47b2a43763eab2c57f15f4b8fc8e1ae1d85` |
| Denon DJ SC6000M Prime | JP14 | `SC6000M-5.3.0.4-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `16f1f01cb466eb3d20ccbc021eb9b47b2a43763eab2c57f15f4b8fc8e1ae1d85` |
| Denon DJ PRIME 4 | JC11 | `PRIME4-5.3.0.4-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `3c4e179b4ec620a0a9df370f1a03d314148be7ff1b2cb208a2027fa7a7d72c23` |
| Denon DJ PRIME 2 | JC16 | `PRIME2-5.3.0.4-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `3c4e179b4ec620a0a9df370f1a03d314148be7ff1b2cb208a2027fa7a7d72c23` |
| Numark Mixstream Pro | NH08 | `MIXSTREAMPRO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `3c4e179b4ec620a0a9df370f1a03d314148be7ff1b2cb208a2027fa7a7d72c23` |

#### Second-Generation Unified Update Images (`*-Update.img`, Primary)

| Model | Board | File | SHA-256 |
| --- | --- | --- | --- |
| Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.4-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `e7fc500f5f3be87fa9556fa3dc795d26d88f451967362392a2268349680111e1` |
| Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.4-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `27d39877006d887782f0ecb346175eb65f07e65b654fdc4e9c91ee6dbfd7bb16` |
| Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.4-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `bff95b0dde5ff8bab510540eac34c0d47c14e0b752c1b6e3416e1f897a479847` |
| Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.4-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `f35d89da0fb5f98b11a4c731ac6c52bc2b5d51a477c6028fc11132ab8bb45c3c` |
| Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `4a2f35c3e3a2b0d28a8b97f41630dabcf23902144d2584ace1894a2d53ec1eae` |
| Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `7ca457d4e0bd83ab6cdd7f2cd859a7af8ffd4a7fb1929f09b170f0fbcb80a9f2` |
| Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.4-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `d15865ea5cf7c9382aed013e63affcfbebecd104cedb94ea846902bd28966b06` |
| Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.4-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `9aeb2fa6207de43eb9f2895000aaf79ba240ac06045c02e32836fc72bba7f413` |

#### Second-Generation Legacy OTA Data Bundles (`*-Data-Bundle.tar.gz`, for `5.3.0.2` / `5.3.0.3` OTA Compatibility)

| Model | Board | File | SHA-256 |
| --- | --- | --- | --- |
| Denon DJ SC LIVE 4 | JP21 | `SCLIVE4-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `59f79382bc3e79b288dff973555e89d605aca325dd9c99bc928f7f6ace4ef887` |
| Denon DJ SC LIVE 2 | JP20 | `SCLIVE2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `2543d53a0f81811151907ed5b99a10c2d638fdccfb994e7de60ed6f5ea07ad55` |
| Denon DJ PRIME 4+ | JC11S | `PRIME4PLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `9646dabe7d6c81288bcaf0fdb0787754d6488685651d5749a01287fff42c3a7d` |
| Denon DJ PRIME GO+ | JP11S | `PRIMEGOPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `107a1793d26e06251b8cbfb7e83925b31da349a640313b2df129e77911c182e0` |
| Numark Mixstream Pro+ | NH08S | `MIXSTREAMPROPLUS-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `8687156c12d958c95381080597ae9a28fa15721c931b4c316d2f1beebd928de3` |
| Numark Mixstream Pro Go | NH10 | `MIXSTREAMPROGO-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `93df0ea991f7c629862445d20c166f9dcb7fdafff2805d89db195f958ea7aea9` |
| Denon DJ PRIME 4 G2 | JP24 | `PRIME4G2-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `f2ba7eb718713b45f5f865e082a04a470aa2e9bbf98e82aa9678a3eb1cfa4b4a` |
| Rane SYSTEM ONE | RMZ2 | `SYSTEMONE-5.3.0.4-EngineX-OS-Data-Bundle.tar.gz` | `8de360319582647daaa209453f543f518e472a63a8fb92a3badb5e4bed4e460d` |
