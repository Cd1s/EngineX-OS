## EngineX OS 5.3.0.5

Custom system built on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units. Build commit: `79d6e2415802c85c743ed80dc1499bb9b767a23b`. Rollback point: tag `v5.3.0.4-r5`.

> **Phase F final sandbox evidence complete.** The SHA-256 table below is from the signed rc6 `SHA256SUMS.txt` built at `79d6e2415802c85c743ed80dc1499bb9b767a23b`. Publication was intentionally not performed. Rows 359–368 and the other physical-deck checks still require the real-deck run. The full change record is `docs/ISSUE-FIXES-5.3.0.5.md`.

- **Online User Guide (English / 简体中文 / 繁體中文)**: [https://enginexdj.com](https://enginexdj.com)

---

### New

- **Native installer & signed updates**: `EngineX-Installer` flashes over USB without Python, fastboot, libusb, or WSL, binds Microsoft's signed WinUSB driver on Windows when needed, and verifies signed `SHA256SUMS.txt.sig` manifests alongside the on-screen updater (#255, #239).
- **Restore official firmware**: `Restore-Official-Firmware.bat` / `.command` / `.sh` (`enginex-installer --restore-stock`) returns a deck to stock Engine OS 5.3.0; boot 5.3.0.5 once and shut down normally before returning to stock.
- **Writable HFS+ drives**: Mac-formatted (`HFS+`) USB drives and SD cards that pass a read-only check mount read-write on all 16 models so Engine saves its library, cues, and loops on the drive *[1st-gen tested with a scratch stick; PRIME 4 G2 / SYSTEM ONE and 2nd-gen 32-bit units not yet verified on a real deck]*.
- **Computer rekordbox Link & unified Pro DJ Link**: `Settings → Services → Pro DJ Link` replaces the two old switches, pairs with CDJ-3000 without firmware warnings (#198) *[needs a real CDJ-3000]*, and lists rekordbox 7 (`Export` mode with `LINK` on) as `rekordbox (<computer name>)` over SMB (#300) or NFS *[not yet verified on a real deck]*.
- **Fast large rekordbox USB import**: large rekordbox sticks open playlists and cues within seconds while waveforms convert in the background and publish automatically after 30 seconds of idle time (#301) *[not yet verified on a real deck]*.
- **Rekordbox cue write-back**: writes Hot Cues, Memory Cues, and Loops edited on the deck back into the USB stick's analysis files and `exportLibrary.db` with a backup in `/.enginex-backup/` (`:8080` web console `Extras → Write cues back to rekordbox USB`, #271) *[not yet verified with a CDJ-3000 / rekordbox 7]*.
- **Channel FILTER Color FX**: assigns `Spiral`, `Pitch Echo`, `Slip Roll`, `Dub Echo`, or `Space` to each channel's `FILTER` knob on all-in-one consoles (`Profile → Playback → Color FX Style`; `Off` keeps stock Sweep FX) *[not yet verified on a real deck]*.
- **Apple Music helper & Spotify audio source**: caches lossless Apple Music tracks via `enginex-amd` on 2nd-gen decks (`/m/applemusic`, native sign-in on PRIME 4 G2 / SYSTEM ONE) and lets you pick the fallback service for Spotify tracks in the `:8080` web console.
- **Bluetooth speaker output**: selects which deck layer or mixer bus feeds a paired Bluetooth speaker (`Settings → Device → Bluetooth → Output Source`).
- **Copy settings between decks**: copies preferences over LAN with confirmation on both decks and includes EngineX settings in USB profile saves (`Settings → Device → Copy settings from another deck`, #205).
- **Mixstream hardware controls**: adds `Pitch Bend → Beat Jump`, `FX Slot 1–4`, a clean pad bar with a `△` Sweep FX button, and a per-channel `Stems EQ` row on Mixstream Pro / Pro+ / Pro Go (`Settings → Device` and `Profile → Playback`, #261, #286).
- **PRIME 4 FX & console controls**: keeps the `PRIME 4` FX carousel open while browsing (#285), adds `Jet Flanger (PX)` and a longer `Reverb` tail (#195, #207), enables `Fader Echo` on all consoles (#258), and drives motorized platters in 4-deck views (#263).
- **Per-deck Reverse Jog Wheel**: reverses jog direction per player or per side on 2-jog consoles (`Settings → Device → Reverse Jog Wheel`, #213).
- **Pitch plateaus, cue countdown & Remote Library sort**: adds whole-BPM pitch fader plateaus when `Profile → Display → BPM Decimals` is `2`, aligns hot cue countdown with the beat grid (#151), and restores `Date Added` sorting in Remote Library (#292).
- **On-deck & streaming Stems**: uses stereo spectral separation on the deck without a computer (#238), tags streaming Stems as "Artist - Title" on a private save partition with per-track progress, and supports RTX 50-series GPUs.
- **Web console, remote access & LAN tools**: redesigns the `:8080` console with an About QR code, Cloudflare Quick Tunnel (`Settings → Device → Remote access`), optional HTTPS (`:8443`, #242), set tracklist export (#268), `Remote Load (LAN API)` (#281), `cpu-ram-monitor` Mod (#296), and `deploy/one-click-backend.sh`.
- **Spanish UI & high-speed SD**: adds `Español` (`Settings → Device → Language`, #282) and an experimental 50 MHz SD mode on RK3288 decks (`Settings → Device → High-speed SD card`).

### Fixed

- **PRIME 4 load stutter**: prevents audio stutter on the playing deck when loading a track on another deck (#298) *[needs a deck check]*.
- **HFS+ safety**: prevents writable mounts on the stock 5.3.0.4 kernel HFS+ driver and allows clean eject and firmware updates from HFS+ drives.
- **Shadow deck & rekordbox**: follows jog nudges and loops accurately on the shadow lane *[needs two decks]*, renders full-height overviews on first load, follows USB hotplug in Pro DJ Link export, and keeps both computers visible when one turns `LINK` off.
- **Streaming & Apple Music**: keeps streaming signed in when using an Engine DJ profile (#304), shows the active streaming service icon and name in the deck header and `Track Info → Source` (#305), preserves sign-out state on Apple Music, and prevents TIDAL/Spotify row interference.
- **Hardware, network & settings**: disables Wi-Fi power saving on all decks at boot (#309), persists 2nd-gen root SSH passwords across reboots (#299), uses standard high-quality SBC bitpool on Bluetooth speakers, restores `Shift + View` on controllers, and asks before USB profile import (#234).
- **Start-up single-drive browse**: automatically browses a deck's only local drive at start-up on all 16 models—including late-mounting drives—instead of stopping on the Sources list. Thanks to @sklempert (#315).
- **PRIME 4 G2 firmware-update USB mode**: supports the `15e4:d014` USB identity in Firmware Update Mode (#311).
- **Clean Pro DJ Link stick eject**: releases the export root and blocks new reads after the first eject attempt, so a stick ejects without repeated `target is busy` failures.
- **Streamed-track BPM and grid**: the loaded track shows Engine's analysed BPM and grid once loaded; the provider's tempo remains only in the list label.
- **Copy BPM follows the source**: a streamed shadow copy refreshes a stale cached BPM and shows the source BPM.
- **Shadow streaming icon**: a streamed shadow copy keeps the source service icon in its header.
- **Streaming scope stays selected**: boot and an unavailable service cannot rewrite the selected source; it changes only when the DJ picks an enabled, signed-in service.
- **Hot Cue and rekordbox shadow edit retention**: preserves deck-set Hot Cues when a capped BridgeAnalysis database prunes the analysis row, and refreshes the shadow copy when PerfGen changes after a rekordbox edit.
- **Deezer OmniSource isolation**: keeps drives and other local sources visible in Deezer's OmniSource view while excluding playlists owned by other streaming services.
- **RGB mini-overviews for shadow copies and rebuilt sticks**: removes stale per-track RGB overview files before Engine starts, serves the source deck's Engine-built RGB overview, and lets shadow copies render the same colours.
- **Paused shadow-copy BPM**: a paused copy keeps the source's BPM while following the source.
- **Streaming source selection**: the selected source changes only when the DJ picks it and never moves to a service that is off or not signed in.

### Changed & Known Limits

- **OTA packages**: `*-Data-Bundle.tar.gz` is included one last time for 2nd-gen decks on `5.3.0.2` / `5.3.0.3`; the 5.3.0.5 updater uses `*-Update.img` only and enforces signature and downgrade checks (use `--image` / `--bundle` in the installer to downgrade intentionally).
- **Spotify & Apple Music**: Spotify direct audio is disabled due to upstream key restrictions (`librespot#1649`); playback matches tracks on a signed-in service. Apple Music plays on 2nd-gen units with a subscription and `enginex-amd` (1st-gen units can browse playlists only).
- **Hardware & scope notes**: Bluetooth adds ~150–250 ms latency (use for monitoring, not beatmatching); on-deck Stems have some bleed compared to computer `htdemucs`; do not unplug HFS+ drives during writes; WAV/AIFF over computer rekordbox Link, pull-stick-and-keep-playing, and PRIME 4 G2 / SYSTEM ONE partition resizing (#244) are not in this release.
- **Open deck checks**: pending real-deck run; no real deck was touched during Phase F.

### How to update

- **Already running EngineX OS 5.3.0.4 or earlier (all 16 models)**: connect to Wi-Fi or Ethernet and tap `Settings → About / Update → Update` (music, library, and settings are kept; 2nd-gen decks on `5.3.0.2` / `5.3.0.3` bridge through `*-Data-Bundle.tar.gz` and unlock `.img` updates; update to 5.3.0.5 first so future updates verify signatures, #239).
- **First-time install on a first-generation unit** (`SC5000`, `SC5000M`, `SC6000`, `SC6000M`, `PRIME 4`, `PRIME 2`, `PRIME GO`, `Mixstream Pro`): copy your model's `*-Update.img` unrenamed to the root of a FAT32 or exFAT USB drive and select it in `Settings → About / Update → Firmware Update Mode → Options`.
- **First-time install on a stock second-generation unit** (`SC LIVE 4`, `SC LIVE 2`, `PRIME 4+`, `PRIME GO+`, `Mixstream Pro+`, `Mixstream Pro Go`, `PRIME 4 G2`, `SYSTEM ONE`): update to official Engine OS 5.3.0 first, back up your deck while it is running (USB `/data` flash erases internal music, library, and settings), and run `EngineX-Installer-5.3.0.5-*`.
- **Going back to stock**: boot a 5.3.0.5 build once, shut down normally, then run `Restore-Official-Firmware.*`.

---

### SHA-256 Checksums (28 Release Artifacts)

`SHA256SUMS.txt`, `SHA256SUMS.txt.sig` and `RELEASE-TAG.txt` are uploaded with the files below (31 files in total).

#### One-Click Installer (`EngineX-Installer`)

| File | SHA-256 |
| --- | --- |
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `0cd69e84839d7d51ca3014bb7de5d7b158e9474b6c7d9356b9eae4b5e4dcda69` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `ab5f4ab8676f5f507a1dc627d26c132ad0782215f955fa8df9dabc4e817b44be` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `d358790922449645aeda525ca59f218fd476c233e16d7b595136b70e422368c4` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `fc593b63af32ebba67e8c3370616a0dc1e663f6a68efc86a8b4f2eb8933fc451` |

#### First-Generation Firmware Images (`*-Update.img`)

| File | SHA-256 |
| --- | --- |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `1287257d14df8995b6661cc4b945ff35fc998196160b155872e1eb744708452d` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `1e6864b2eab413398144dfa803e0548caf874aa3de463fd3b9d8485768eb9d6d` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `1e6864b2eab413398144dfa803e0548caf874aa3de463fd3b9d8485768eb9d6d` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761` |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `fb3dde6d2430fa9b0caca690e15d04b4c1e73611149084a4b9cbe41137e92761` |

#### Second-Generation Unified Update Images (`*-Update.img`, Primary)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `207dd572677c22dc3b96ce3bd1f34bfa9c14d97ec04d254f4a9dba256a073fcd` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `bced53c96bba18ac4ddf06a56bfd09e119670cc4346ad117da13e44d3eb38ddd` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `ec91e1428b9f310893b212ab53dccb935659f959f30080e1e7ba120387f98183` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `364c7082bc243bc0d12d75c8b79b3bbe6a7bb8b6eda0f6bb3071e330480766db` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `27992b1ff6aadcffbbc6e8098be2ada291ad43bcf492af52a41d45f1bf1c6e6d` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `180399f955e732a0d65ed009db27b5da9b100b401647977cbfdc8650b9ea9342` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `704c8f4a0fbf8e4815018c12380cdc4204c82d488863579aa3fca40d32f87e82` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `78b0cd3b706d25f9b4463c307f6c748edd5aa0b2c512dfd228016b07c76c6280` |

#### Second-Generation Legacy OTA Data Bundles (`*-Data-Bundle.tar.gz`, last time)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `5f799aa012a8140ed95403c66dc15bb913a9f79a5dbba996abc117fc67f2e7bb` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `6b1685a35b64b5c478ee0d87189b71adf544dd7e07388ee6bed2005d0d517095` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `7bd01f22e4f32bbeabbf2789c8ca3ad9274cda0aca584861f9d49c518df4af32` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `1fe29859be694981df9eccca3e88599f37eac1d2c411d5d265e1cb4262325eca` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `95dabce4a99d4db34548eeba5a6e044bbf8b84608d196b099bd5c167f0cd1d5d` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `918bf4913fab122fa38d760d2d7fd20525e28420b74c723f092c4fc7048e8404` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `7bd0af92d008998044c5fe43fa615d7a675619bdf476a1d89a229fef18b0aaf4` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `01a37d9700c77cae3a89f40bb2a7dd1efcecc3b0c043f4cfbb8997866c119768` |
