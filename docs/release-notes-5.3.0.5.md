## EngineX OS 5.3.0.5

Custom system built on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units. Build commit: `ed3b5fa70ef5f9c7a78a7dcc4a96d26c715a28d7`. Rollback point: tag `v5.3.0.4-r5`.

> **Phase C dry-run evidence complete.** The SHA-256 table below is from the signed rc3 `SHA256SUMS.txt` built at `ed3b5fa70ef5f9c7a78a7dcc4a96d26c715a28d7`. Publication was intentionally not performed. Rows 359 and 360 still require the real-deck run, and the shadow-lane M4A/FLAC overview issue is deferred to 5.3.0.6. The full change record is `docs/ISSUE-FIXES-5.3.0.5.md`.

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
- **Hot Cue and rekordbox shadow edit retention**: preserves deck-set Hot Cues when a capped BridgeAnalysis database prunes the analysis row, and refreshes the shadow copy when PerfGen changes after a rekordbox edit.
- **Deezer OmniSource isolation**: keeps drives and other local sources visible in Deezer's OmniSource view while excluding playlists owned by other streaming services.

### Changed & Known Limits

- **OTA packages**: `*-Data-Bundle.tar.gz` is included one last time for 2nd-gen decks on `5.3.0.2` / `5.3.0.3`; the 5.3.0.5 updater uses `*-Update.img` only and enforces signature and downgrade checks (use `--image` / `--bundle` in the installer to downgrade intentionally).
- **Spotify & Apple Music**: Spotify direct audio is disabled due to upstream key restrictions (`librespot#1649`); playback matches tracks on a signed-in service. Apple Music plays on 2nd-gen units with a subscription and `enginex-amd` (1st-gen units can browse playlists only).
- **Hardware & scope notes**: Bluetooth adds ~150–250 ms latency (use for monitoring, not beatmatching); on-deck Stems have some bleed compared to computer `htdemucs`; do not unplug HFS+ drives during writes; WAV/AIFF over computer rekordbox Link, pull-stick-and-keep-playing, and PRIME 4 G2 / SYSTEM ONE partition resizing (#244) are not in this release.
- **Known issue**: shadow-lane overviews for M4A/FLAC are not drawn; deferred to 5.3.0.6.
- **Open deck checks**: pending real-deck run; no real deck was touched during Phase C.

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
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `34353e8d711c0110cd412c0dee67320d682a5725e428a410dbe880ed58bd4673` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `a7f879a9c8c634852724bdfe1e51803a723ee90128c3975bb376c9eed618861a` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `99d7d3631581432ddc08941b0e8f3815436975cea52546796b9e8a407fbc4bcd` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `f1feb8362c90841888ca657a9e7f675242e46ba5f3f686850cd5325aeb60b8b6` |

#### First-Generation Firmware Images (`*-Update.img`)

| File | SHA-256 |
| --- | --- |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `03079438dcbd347333b4984276e98e3ed8613c76a7412accaaa887f7d18ffc30` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `03079438dcbd347333b4984276e98e3ed8613c76a7412accaaa887f7d18ffc30` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `03079438dcbd347333b4984276e98e3ed8613c76a7412accaaa887f7d18ffc30` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `259120f2475acb6eab3cc984ccbe64b0983af3e4dd7923ae813bcc7e6226e647` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `259120f2475acb6eab3cc984ccbe64b0983af3e4dd7923ae813bcc7e6226e647` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `d52a9bb1873a3402577a3152c0fe7f631abbad17e89d678a07b7ed2cff744fb6` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `d52a9bb1873a3402577a3152c0fe7f631abbad17e89d678a07b7ed2cff744fb6` |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `d52a9bb1873a3402577a3152c0fe7f631abbad17e89d678a07b7ed2cff744fb6` |

#### Second-Generation Unified Update Images (`*-Update.img`, Primary)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `32f9e920afbf52c92961fd759f8a5a497ddc4f5c337a5db9c9a0d08f00a93978` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `da21eab755ec7dc4e9f0b940166e2b2d49302951ab0e91074e99ba9e43caacaa` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `11b522f26cdb0b8c51db4d253ba4884dabd9d062d31e8f9abca8d4bdd437bb2d` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `3fab5a5540532961b466ae1ca9d267a54642f3bb2042f9b2516af36ff8fa3010` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `9347c3abda6f8289afb7e373e848b92bbd7f1b44c600298be7df466c830feb7f` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `d9362b521ebb717a6563d9f6014e0824001759e419a97645f396c390e5a38252` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `d1af491bd694cbfcd3efdfac68377ab5ad661f8c0b5e28d9f495be1cdd6fb4ba` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `245368f9e67a98ea51985d6ce335ecc5e50434cdf142724f3d86ee27c6f86397` |

#### Second-Generation Legacy OTA Data Bundles (`*-Data-Bundle.tar.gz`, last time)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `edd6cd5d293b6f91a9c36f46e06854079599d79fe987a24fda21978644b92534` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `636b537c7118883852a7749589ff5e200ea175b09835a640fa5fa5cecfe3afd5` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `52ab1ebfbcd5eb688b6253a1ddda47f8a59018d8fe74e4483fc8623cb85bdf68` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `70d901d475b92c0437e76c0fad99c423b525cc513687653e19820172794c9406` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `552b71f1f659cf3d11d5e9bb48c681648f6a5c0767bf19af0fe33a65a8847414` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `cc1fb9df0e40653429f6a06686be6c56ed2a236f8ad50b958a4e9bfb64f058bd` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `063d93c9b9c3b56f5091772f4ca6191d92f98677ef33c7c53c187b0999b33b1d` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `edf4e0181f80afcaca717de186269004826754683e20c57defa6bd18a1cd57e1` |
