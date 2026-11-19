## EngineX OS 5.3.0.5

Custom system built on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units. Build commit: `eea05653c83917a6489f8336fb48982e28de4348`. Rollback point: tag `v5.3.0.4-r5`.

> **Phase B evidence complete.** The SHA-256 table below is from the signed Phase B `SHA256SUMS.txt`. Publication was intentionally not performed in this run. Items marked **[not yet verified on a real deck]** still require the real-deck run. The full change record is `docs/ISSUE-FIXES-5.3.0.5.md`.

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

### Changed & Known Limits

- **OTA packages**: `*-Data-Bundle.tar.gz` is included one last time for 2nd-gen decks on `5.3.0.2` / `5.3.0.3`; the 5.3.0.5 updater uses `*-Update.img` only and enforces signature and downgrade checks (use `--image` / `--bundle` in the installer to downgrade intentionally).
- **Spotify & Apple Music**: Spotify direct audio is disabled due to upstream key restrictions (`librespot#1649`); playback matches tracks on a signed-in service. Apple Music plays on 2nd-gen units with a subscription and `enginex-amd` (1st-gen units can browse playlists only).
- **Hardware & scope notes**: Bluetooth adds ~150–250 ms latency (use for monitoring, not beatmatching); on-deck Stems have some bleed compared to computer `htdemucs`; do not unplug HFS+ drives during writes; WAV/AIFF over computer rekordbox Link, pull-stick-and-keep-playing, and PRIME 4 G2 / SYSTEM ONE partition resizing (#244) are not in this release.
- **Open deck checks**: pending real-deck run; no real deck was touched during Phase B.

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
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `d95491f31e296126476d6f85f44d26d689956626f80535836bf261802dd28e15` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `25d0c934cb0939085af1c3c5dd9dd255a71936eda26630ecb695b58ff66cfb0a` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `1a173a20cc90ec9b032ad4970c21388946904dc30f04b1448f411170fdf5a1b7` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `b46f98f2d1c45477cae2dbdf65e17a597bf41a196d9587d792533cc2cf5aefd9` |

#### First-Generation Firmware Images (`*-Update.img`)

| File | SHA-256 |
| --- | --- |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `4478a8ebff29374443e4651e46c5a89e065c792e5f4592526b3b96d4ca36b6fa` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `4478a8ebff29374443e4651e46c5a89e065c792e5f4592526b3b96d4ca36b6fa` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `4478a8ebff29374443e4651e46c5a89e065c792e5f4592526b3b96d4ca36b6fa` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `fce676f18afc93740a142d962bde651ba683f0ee90f8560edc0b0addaeb9d9c6` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `fce676f18afc93740a142d962bde651ba683f0ee90f8560edc0b0addaeb9d9c6` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `b088526c08dd6a8946c1212fe9fad596954deb39887667c70df02aa451fbef17` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `b088526c08dd6a8946c1212fe9fad596954deb39887667c70df02aa451fbef17` |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `b088526c08dd6a8946c1212fe9fad596954deb39887667c70df02aa451fbef17` |

#### Second-Generation Unified Update Images (`*-Update.img`, Primary)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `3c7e6e138d5cf172c3d0201d5b889095f2e3648da2ad3b11b4ba2800b838af7d` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `0dad3add4033b8df9a522d35777753d049fc5d0dd479e335efdf16d217aed02a` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `85e35d6c4433f734e314ea45b28fc58a1c7dacc8466e1b730dcce0f30c602db3` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `f3c61bad9366c8b1b19ea2f3816feb9350b2b702179f0a7c343c02ae2f45574f` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `34e45b620edd528a89087329787ac0bbf2a5ab8abe2723a9ff904a713a1f1c27` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `1895072ee91b252a1deb3fe235a361a1591cbdf0baf139260295369d1b0143e6` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `5133115bb2dfbe4d6a1aee7459f78005915e56c060fe89503d0b9989650ed60e` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `2ea625f64ae04b2195a35dfba799b6f3ffe858a58cb9a2cce6024cf7736224af` |

#### Second-Generation Legacy OTA Data Bundles (`*-Data-Bundle.tar.gz`, last time)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `9c67f8b7431b9459a6fc9b3a100a4a20096d1ac873f23eee5a4008c44c1ddf92` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `96a211962eccf1a828b0f4fa814c0a739775b8c3d2d378fb40d875dbab384770` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `4c6214c08849f2e5be089b077f0d246569e16649d72802307a31fad58fa5fa88` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `c6085c4ddc691255192147f42dbeb2c68c47b089febe729a83df0dae8df43e75` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `e335c34b131829d004d5faf1b2193740dca48442fec9225faad95ea4eef0b3fb` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `c1139a012f44903b4c561c3e3f476bd6b7b6350219defedf963d8ae97024622f` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `0d31d6a10d1b1f279c8342a9f25d3f1fec8fa84c77adef5bb2ecd17205bd4b55` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `64de98844e4a929e465d4dc1030bc9f11d5349324a8fe44550e18778bd3ba61e` |
