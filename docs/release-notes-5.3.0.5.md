## EngineX OS 5.3.0.5

Custom system built on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units. Build commit: `1b05b8d159eff23c9698053bef780810fcb26a57`. Rollback point: tag `v5.3.0.4-r5`.

> **Phase E dry-run evidence complete.** The SHA-256 table below is from the signed rc5 `SHA256SUMS.txt` built at `1b05b8d159eff23c9698053bef780810fcb26a57`. Publication was intentionally not performed. Rows 359–363 still require the real-deck run. The full change record is `docs/ISSUE-FIXES-5.3.0.5.md`.

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
- **RGB mini-overviews for shadow copies and rebuilt sticks**: removes stale per-track RGB overview files before Engine starts, serves the source deck's Engine-built RGB overview, and lets shadow copies render the same colours.
- **Paused shadow-copy BPM**: a paused copy keeps the source's BPM while following the source.
- **Streaming source selection**: the selected source changes only when the DJ picks it and never moves to a service that is off or not signed in.

### Changed & Known Limits

- **OTA packages**: `*-Data-Bundle.tar.gz` is included one last time for 2nd-gen decks on `5.3.0.2` / `5.3.0.3`; the 5.3.0.5 updater uses `*-Update.img` only and enforces signature and downgrade checks (use `--image` / `--bundle` in the installer to downgrade intentionally).
- **Spotify & Apple Music**: Spotify direct audio is disabled due to upstream key restrictions (`librespot#1649`); playback matches tracks on a signed-in service. Apple Music plays on 2nd-gen units with a subscription and `enginex-amd` (1st-gen units can browse playlists only).
- **Hardware & scope notes**: Bluetooth adds ~150–250 ms latency (use for monitoring, not beatmatching); on-deck Stems have some bleed compared to computer `htdemucs`; do not unplug HFS+ drives during writes; WAV/AIFF over computer rekordbox Link, pull-stick-and-keep-playing, and PRIME 4 G2 / SYSTEM ONE partition resizing (#244) are not in this release.
- **Open deck checks**: pending real-deck run; no real deck was touched during Phase E.

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
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `04d53367fb592cfe46afd04fb65ed13850ba104513b2b9f8afbd63e225db045a` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `065c2e97a21efd85f4fc6611899e068ec2eb71b2070f43d5edade525930304b6` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `99c225f93f8fe26a3c4b54cfd1f5d3c4aee4c3904a79eca8227677350a2cb114` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `bf126c4cf1d4609fa673e7bc53ead274d54610c76b6857b4da1052781d498fb9` |

#### First-Generation Firmware Images (`*-Update.img`)

| File | SHA-256 |
| --- | --- |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `0ac5f0ab702d4fe2fa8d0361939871e60a87bee2641303065f2c44324011aea7` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `ce4a193654e1508dff51c4a6b4c8b9bff7aefe515ce068c3063043949df982f3` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `ce4a193654e1508dff51c4a6b4c8b9bff7aefe515ce068c3063043949df982f3` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9` |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `5aa7cbe7d297439b686c6aa82aded63cff2ad674699b924f6f35ca61519dd6f9` |

#### Second-Generation Unified Update Images (`*-Update.img`, Primary)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `8fdeba39f6697c3901dfa4108cddbd7412e32864bda9e1bfca2443b1b1a3928e` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `57d4364a75780fed65288f849bddf39b6b396021d5026cc04279a935b5155ef8` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `f31f00d1f1562a8bb59810849ee0b58f8f1bd5479b6f44e19fbd1428e2937829` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `5625b58d57d75ef87113926b51a0455635fd4da70952cbfcb6dc9676b3fe4338` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `3195803fbd99438f66ee71662df808d3e7e88022f71f699db240c2089d0a5486` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `93851bd63bae8634597dd6b6326b2041545014e4de6819597c4eae2f21db5f27` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `16bc57b63653bca44f1d49e898147d02363f72157e33ac7d6fe0f43c16722c19` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `7c242e0a2f103b16c1c025252eab0dca0a235a785c6a9f7a751f3534b97505ba` |

#### Second-Generation Legacy OTA Data Bundles (`*-Data-Bundle.tar.gz`, last time)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `f16e752878fa8228da69faf22fa66cbb7ca173847373e83a56a44f90965b2189` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `a61a08d6c9fc45124b375e9de192ac6df0fe5d72028511ea23b28328676abfa6` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `dc0e444d285b060ec42adb49296b8fddcd60ee1372a2d76f2ebe3c647b33d7e2` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `55ad54cd82c27605cf4d41c24ebffd8ac097f952e0381894dd76f0ad0970c8ac` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `6d56dade959380f86d0337aa8f26db5d6f04c651dde6cc1c8756b6c8f5ee8497` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `956dd64fd6162dcf2d2b1bcf2147f2cfa2a79324b8e32a7f056de83007a79b4f` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `b9425d07d4a9388309a98d5ed2eb9c15c33635eeed616cb1b09165e34d3dd559` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `d8a555cdbf33d6517d3849ddfdb081f964b7006f115b3ba3edea2cf7dc0c8f68` |
