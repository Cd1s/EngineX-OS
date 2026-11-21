## EngineX OS 5.3.0.5

Custom system built on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units. Build commit: `1c41b7e9bd8b8d5064f0c3f7e1e623949fb51f01`. Release tag: `v5.3.0.5-r1`. Rollback point: tag `v5.3.0.4-r5`.

> **Installers updated on 2026-11-21 (#311)**: the four `EngineX-Installer-5.3.0.5-*` archives, `SHA256SUMS.txt` and `SHA256SUMS.txt.sig` were replaced with installers built at `89597c9f`. The 24 deck files and `RELEASE-TAG.txt` are unchanged, so decks are not offered another update.

> **5.3.0.5 r1 release evidence complete.** The signed table below is from the final output built at `1c41b7e9bd8b8d5064f0c3f7e1e623949fb51f01`. The evidence commit is made on top of `84708831`. The two shadow fixes were confirmed on two SC5000 decks with deck-push builds, including a one-step-per-beat countdown. Other physical-deck checks remain pending. The full change record is `docs/ISSUE-FIXES-5.3.0.5.md`.

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

- **Shadow phase meter follows the source**: the shadow lane shows the source deck's alignment and phase, turns green with the source, and its phase lines follow the source.
- **Shadow cue countdown follows the beat**: a paused shadow copy advances its cue countdown once per beat, matching the source deck.
- **PRIME 4 load stutter**: prevents audio stutter on the playing deck when loading a track on another deck (#298) *[needs a deck check]*.
- **HFS+ safety**: prevents writable mounts on the stock 5.3.0.4 kernel HFS+ driver and allows clean eject and firmware updates from HFS+ drives.
- **Shadow deck & rekordbox**: follows jog nudges and loops accurately on the shadow lane *[needs two decks]*, renders full-height overviews on first load, follows USB hotplug in Pro DJ Link export, and keeps both computers visible when one turns `LINK` off.
- **Streaming & Apple Music**: keeps streaming signed in when using an Engine DJ profile (#304), shows the active streaming service icon and name in the deck header and `Track Info → Source` (#305), preserves sign-out state on Apple Music, and prevents TIDAL/Spotify row interference.
- **Hardware, network & settings**: disables Wi-Fi power saving on all decks at boot (#309), persists 2nd-gen root SSH passwords across reboots (#299), uses standard high-quality SBC bitpool on Bluetooth speakers, restores `Shift + View` on controllers, and asks before USB profile import (#234).
- **Start-up single-drive browse**: automatically browses a deck's only local drive at start-up on all 16 models—including late-mounting drives—instead of stopping on the Sources list. Thanks to @sklempert (#315).
- **PRIME 4 G2 firmware-update USB mode**: supports the `15e4:d014` USB identity in Firmware Update Mode (#311).
- **2nd-gen first install checks the stock version**: the installer asks for the version shown in `Settings → About / Update` and stops without writing anything if the deck still runs stock firmware older than 5.3.0, which does not boot after the install (#311).
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
- **Open deck checks**: the phase-meter and cue-countdown fixes were confirmed on two SC5000 decks using deck-push builds, including a measured one-step-per-beat countdown. Other real-deck checks remain pending.

### How to update

- **Already running EngineX OS 5.3.0.4 or earlier (all 16 models)**: connect to Wi-Fi or Ethernet and tap `Settings → About / Update → Update` (music, library, and settings are kept; 2nd-gen decks on `5.3.0.2` / `5.3.0.3` bridge through `*-Data-Bundle.tar.gz` and unlock `.img` updates; update to 5.3.0.5 first so future updates verify signatures, #239).
- **First-time install on a first-generation unit** (`SC5000`, `SC5000M`, `SC6000`, `SC6000M`, `PRIME 4`, `PRIME 2`, `PRIME GO`, `Mixstream Pro`): copy your model's `*-Update.img` unrenamed to the root of a FAT32 or exFAT USB drive and select it in `Settings → About / Update → Firmware Update Mode → Options`.
- **First-time install on a stock second-generation unit** (`SC LIVE 4`, `SC LIVE 2`, `PRIME 4+`, `PRIME GO+`, `Mixstream Pro+`, `Mixstream Pro Go`, `PRIME 4 G2`, `SYSTEM ONE`): update to official Engine OS 5.3.0 or newer first with Engine DJ's official updater (check `Settings → About / Update`: older stock firmware such as 5.0.x does not boot after the install, and the installer stops below 5.3.0), back up your deck while it is running (USB `/data` flash erases internal music, library, and settings), and run `EngineX-Installer-5.3.0.5-*`.
- **Going back to stock**: boot a 5.3.0.5 build once, shut down normally, then run `Restore-Official-Firmware.*`.

---

### SHA-256 Checksums (28 Release Artifacts)

`SHA256SUMS.txt`, `SHA256SUMS.txt.sig` and `RELEASE-TAG.txt` are uploaded with the 28 artifacts below, for 31 files in total. This release is published as `v5.3.0.5-r1` so beta1 decks carrying `v5.3.0.5` receive the update.

| File | SHA-256 |
| --- | --- |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `a76b3eaefdb24dab01ec70f2316c0809a6b25d281fa6c03d751437463a1f25e6` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `57b352d82f1acd36245c5e4ef77e3f5734c8e246972bf92bd51eea238d4e8c4b` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `1cba7fe16ca95803ba9caa57c45343dccbfca47696e925511a142d4a4ca94cf2` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `a76b3eaefdb24dab01ec70f2316c0809a6b25d281fa6c03d751437463a1f25e6` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `a76b3eaefdb24dab01ec70f2316c0809a6b25d281fa6c03d751437463a1f25e6` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `ac5cbb4bb66589ed3b2bcdc887f75013e87a0c124548cfea5dfd7790430d0ea4` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `6991b1ff199f3586d92a446e1691ff4c5e1f842a871296eec093e846d70695d5` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `05be05975ef00cb0185e3ff262367934c48ee90b6e5161c4ade7e459fb6a1e04` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `3c4a7112e49c3af1eb551d4bd668881299e0be6ffd99e88b23c4b0d287181ecf` |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `05be05975ef00cb0185e3ff262367934c48ee90b6e5161c4ade7e459fb6a1e04` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `05be05975ef00cb0185e3ff262367934c48ee90b6e5161c4ade7e459fb6a1e04` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `27df9fa5fc46ce3ccc1b0c71ae754255b51a6b249d2b029360afdc6a08a844f8` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `27df9fa5fc46ce3ccc1b0c71ae754255b51a6b249d2b029360afdc6a08a844f8` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `1c7d270bd8ca136bce8be84a69c15b3d3d0de05cac6c0fcfb5409c5d36bfa708` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `3fcec1b89be14e10ad3a02bf2eeeb38f01f14e3a9aec64035360980d430ea33b` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `a4af1432ccec6e512e32c0765469f84c422626df4f18b5a75a3741eeb2c747b9` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `3bbe6cd4236e94dd74dbad0ac272906b5d1bfc9df68e6286be00f675f1308286` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `40ed000700185212d5e5a4b3bb8d51a159bcd1ef7296030d5383dd6ab90ddd2b` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `d207dbc79dd12336bd30c82c5d1fdd2eac5886b7524ea705e97573e55b57d547` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `55bbe42ebdd11baeb823c18cd100bdcd789e7430c8d366aeaef32b168b23216c` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `d675438a5a8c46b827c53314ce85e54a103832896b4797a6b715f2767b01873a` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `acd8d90cf5051a17ecb40a954ac578b54ef598ae8f56f4f5fdf497db65e03e2d` |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `def34d8578ce191ad0c8b2f6915c99491e70688aa932bbb17345326715cc03fe` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `0cde71d401e33f7abe5c82c72e730102d67fa73f006bf3c83dfe7f732f62c623` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `7708639ebd47fbfc7590e77a0e8779582587145eed5191cb69e19bba546e71f3` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `36d2e5b8e70a725c1b09c150b9a6edb13141eba380e01ee5cb1f4bf6fc50b60e` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `a9c43b139b6c15b9fe56f2990c73c3e3384194be62e9b511e69594f5ab62c87f` |
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `a47b64d48d525d465aeb181ddf579b6eabe21f1a819dd7a6a490832443c99dd7` |
