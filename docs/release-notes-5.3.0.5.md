## EngineX OS 5.3.0.5

Custom system rebuilt on the official Engine OS 5.3.0+ base for all 16 Denon DJ, Numark, and Rane standalone units. Build commit: `TO-FILL-AFTER-BUILD`. Rollback point: tag `v5.3.0.4-r5`.

> **DRAFT.** Replace every `TO-FILL-AFTER-BUILD` (build commit, SHA-256 table) from the signed `SHA256SUMS.txt` of the real build before publishing. Items marked **[not yet verified on a real deck]** have only sandbox, unit-test and release-gate results; keep or drop the mark after the deck checks. The full change record is `docs/ISSUE-FIXES-5.3.0.5.md`.

- **Online User Guide (English / 简体中文 / 繁體中文)**: [https://enginexdj.com](https://enginexdj.com)

---

### New

- **Native installer and signed updates (#255, #239).** `EngineX-Installer` no longer needs Python, fastboot, e2fsprogs, libusb or WSL. On Windows it configures Microsoft's own signed WinUSB driver for the selected update device when needed (an administrator prompt appears only then) and warns before a second-generation install erases the internal storage. Every Release's `SHA256SUMS.txt` is signed (`SHA256SUMS.txt.sig`); the on-screen update and the installer refuse a release that is unsigned, wrongly signed, altered, or older than the deck's.
- **Restore official firmware.** `Restore-Official-Firmware.bat` / `.command` / `.sh` (or `enginex-installer --restore-stock`) returns a deck to stock Engine OS 5.3.0. Boot a 5.3.0.5 build once and shut down normally before you go back to stock.
- **Mac-formatted (HFS+) sticks and cards can be written to on every model.** Engine can save its library, cues and loops on the drive. Only a volume that passes a read-only check (`fsck`, volume header, empty journal) is mounted writable; anything else stays read-only. A drive that was not ejected cleanly but checks out is marked clean first. If the deck built a library for the drive while it was read-only, that library is copied onto the drive in full the first time (nothing is half copied; an existing library on the drive is never overwritten). First-generation decks use a fixed kernel module shipped in the image; PRIME 4 G2 / SYSTEM ONE use the stock driver, which has no kernel bug there; the second-generation 32-bit units (SC LIVE 4, SC LIVE 2, PRIME 4+, PRIME GO+, Mixstream Pro+, Mixstream Pro Go) use a user-space HFS+ driver (`lklfuse`), and ejecting waits up to about 15 seconds for it to finish writing. **[a first-generation deck was tested with a scratch stick; PRIME 4 G2 / SYSTEM ONE and all second-generation 32-bit units are not yet verified on a real deck]**
- **rekordbox on your computer as a source.** Turn on `Settings → Services → Pro DJ Link`, run rekordbox 7 in `Export` mode with `LINK` on, and `rekordbox (<computer name>)` appears in the deck's Source list with all tracks and playlists (title, artist, BPM and key shown before loading). Audio comes from a network share of your music folder (set it in the web console, `Library, network & system → rekordbox music folder`; SMB works on decks without CIFS, #300) or, if the computer allows it, over NFS, with faster pipelined reads. The source disappears when `LINK` goes off and returns with it. Waveforms, beat grids and cues are not carried over: Engine analyses the track when it loads. **[not yet verified on a real deck]**
- **One Pro DJ Link switch.** The two old switches are now one: `Settings → Services → Pro DJ Link` (also in the web console). It announces each deck as its own player (rekordbox shows one player per deck and can hand MASTER to it), browses CDJs and rekordbox, and shares the deck's USB / SD.
- **CDJ-3000 link (#198).** The status packet and keep-alive of our players now look like a CDJ-3000's, which ends the "linked to a player with older firmware" message. **[needs a real CDJ-3000]**
- **Big rekordbox sticks (#301).** The first insert of a rekordbox stick publishes all playlists, tracks and the export's hot and memory cues within seconds when the library fits the mount window (about 6–7 thousand tracks on an SC5000); larger libraries show the stock OneLibrary view first, convert in the background at low priority, show `Importing rekordbox library… (done / total)` in the USB pill, and are published by themselves by an invisible remount once no deck is using the stick and everything has been quiet for 30 seconds (no re-insert). **[not yet verified on a real deck]**
- **Write cue and loop edits back to a rekordbox stick (#271).** Web console, `Extras`, `Write cues back to rekordbox USB`: hot cues, memory cues and loops set on the deck are written into the stick's analysis files and `exportLibrary.db`, with a one-time backup in `/.enginex-backup/`. **[not yet verified with a CDJ-3000 / rekordbox 7]**
- **Color FX on each channel's `FILTER` knob.** On PRIME 4, PRIME 2, PRIME GO, Mixstream Pro and the second-generation SC LIVE 2 / 4, PRIME 4+, PRIME GO+, Mixstream Pro+ / Pro Go, choose a style in `Profile → Playback → Color FX Style`: at the centre the sound is stock, turning the knob blends the effect in, and `Off` gives the stock Sweep FX back. It never touches the main output (an earlier build left a constant echo on a PRIME GO). PRIME 4 G2 / SYSTEM ONE hide the setting. **[not yet verified on a real deck]**
- **Apple Music (maintainer's feature) and Spotify audio source.** Apple Music uses the `enginex-amd` download helper (macOS, Windows, Linux, or locally on second-generation decks) to convert lossless ALAC to FLAC and cache it on the deck; set the helper in the `:8080` console and sign in on the deck's phone page `/m/applemusic` or in the console. Personal songs, albums and catalog playlist search work again, and signing out clears the old account's browse cache. An Apple Music subscription is required; PRIME 4 G2 / SYSTEM ONE sign in natively without an external backend. Spotify's direct audio is switched off (see Known issues); the deck matches each Spotify track on a service you are signed in to, and the web console's Music section lets you choose `auto`, Deezer, NetEase, Beatport, Apple Music or SoundCloud.
- **Web console, Remote access and About QR.** The web console has been redesigned. `Settings → Device → Remote access` opens a temporary public address (Cloudflare Quick Tunnel); `Settings → About / Update` shows a `Scan to open the console` QR code (the address only, never the access code).
- **One-click backend.** `sudo bash deploy/one-click-backend.sh` on Debian 12+ / Ubuntu 24.04+ sets up `enginex-relay` and the Stems service (no domain needed); `enginex-installer --setup-backend` connects a flashed deck to it. Existing installs migrate and keep their passwords.
- **Mixstream (#286).** `Settings → Device` and the web console on Mixstream Pro / Pro+ / Pro Go: `Pitch Bend → Beat Jump` turns `-` / `+` into Beat Jump back / forward (`Shift` keeps Speed Range), and `FX Slot 1–4` assigns Echo, Flanger, Delay or Phaser to each FX button. Sweep FX is a single `△` button on the Pad bar, and the Stems EQ switch is a per-channel row in `Profile → Playback`. All Control Center tiles now fit on screen.
- **Shadow deck.** A jog nudge on the source shows on the other deck's shadow lane within a few frames, the shadow no longer runs slightly ahead, and it follows a loop without overshooting. **[needs two decks]**
- **Streaming Stems.** The imported file is named "Artist - Title" and tagged; the row you tapped shows Engine's render progress per track; a song that cannot be downloaded says "Download failed"; the "No source drive available" pop-up is gone (the deck keeps a private save drive); tapping Stems while every deck plays says so on the same swipe. Stems also work on a computer with an RTX 50-series GPU.
- **Pitch fader plateaus.** With `Profile → Display → BPM Decimals` set to `2` the fader has a short plateau at every whole BPM; `1` keeps stock behaviour.
- **Hot cue countdown** follows the beat grid and grid edits (#151). **Remote Library** sorting offers `Date Added` again (#292). New official Mod `cpu-ram-monitor` shows CPU and RAM next to the clock (#296).
- **Second-generation SSH password (#299).** The root password set in the web console (`Remote & live → SSH access`) now works on second-generation decks and survives a reboot; public-key login is unchanged.
- **Also in this release:** Bluetooth speaker output, copy settings between decks (#205), on-deck Stems that really separate (#238), optional HTTPS console (#242), Remote Load LAN API (#281), PRIME 4 FX selection (#285), Spanish UI (thanks to Javier Saenz), experimental high-speed SD card, `Reverse Jog Wheel` per side (#213), `Fader Echo` on every console (#258), motorized platters in 4-deck layouts (#263), Mixstream pad bar (#261), set tracklist export (#268), `Jet Flanger (PX)` and a longer `Reverb` tail on PRIME 4 (#195, #207), Wi-Fi + cable on one subnet, Live Stream as WAV only and on demand.

### Fixed

- **Stutter on the playing deck when another deck loads a track (#298, PRIME 4).** The background tuning script no longer pins IRQs and Engine's per-deck decoder and loader threads; they stay where stock puts them. This is the likely cause found in a sandbox, not confirmed on a PRIME 4. **[needs a deck check; if it still stutters please tell us your model and drive]**
- **HFS+ safety.** 5.3.0.4 could mount a clean, non-journaled HFS+ stick writable on a driver with a kernel bug that can crash the deck and damage the stick. 5.3.0.5 fixes this on every model.
- `Shift + View` cycles the five stock views again on the real controller; selecting a USB drive no longer opens a "Settings copy" error; a read-only HFS+ stick ejects cleanly and a firmware update from a Mac-formatted stick installs the chosen file; a layer switch costs the UI pack little more than stock (#222).
- Large rekordbox libraries import more reliably and the shadow deck's overview is as tall as Engine's own from the first load; Pro DJ Link export follows a stick that is plugged in or removed; cue write-back sees edits Engine has not checkpointed yet.
- Bluetooth: the speaker's SBC bitpool is the standard high-quality value, not the speaker's maximum (a possible cause of dropouts), and a hand-edited output setting can no longer crash an all-in-one.
- Streaming: failed-download rows expire one by one; the "No source" pop-up no longer lingers after a USB eject; Spotify loads, the TIDAL login, collection rows and Ogg names no longer interfere with each other.
- Settings: an invalid request, or one with trailing garbage, is refused before anything changes or Engine restarts.
- Spanish and other translated screens: whole-line strings that stayed English are translated and long Spanish labels fit.
- Official Mods follow the final names, and a Mod pause saved on 5.3.0.4 still restores after the upgrade.
- Internal names now all say EngineX (#234): the old compatibility API alias, data link, sockets and downgrade restore are retired; upgrades from 5.3.0.4 and earlier migrate once and keep your data.
- **Wi-Fi power saving is off on every deck (#309)**: decks without the stock `iw` tool, such as the SC LIVE 4, silently kept Wi-Fi power saving on; the deck now switches it off itself at start-up (stock `iw` is still used where it exists).
- **Review fixes for Apple Music, the relay and rekordbox**: signing out of Apple Music on the deck stays signed out while the download helper is still signed in; a relay that points a deck to another helper address no longer hands the old token to the new host; the relay console keeps the auto-detected token when you press Save; holding a phone-login QR can no longer set the helper and push it to the relay; with two computers running rekordbox, one with `LINK` off no longer hides the library of the other; the Mixstream-only `Stems EQ` row is built only on Mixstream units.
<!-- PENDING-REVIEW #304: drop this line if agent/5305-issue-304 is not merged -->
- **Engine DJ profile no longer logs streaming out (#304)**: being signed in to your Engine DJ profile no longer ends in "Tidal Disconnected" and a logged-out streaming source when you load a streaming track.
<!-- /PENDING-REVIEW #304 -->
<!-- PENDING-REVIEW #305: drop this line if agent/5305-issue-305 is not merged -->
- **The deck header shows the streaming service you chose (#305)**: tracks played from Deezer, Spotify, Apple Music, NetEase, SoundCloud, Beatport or rekordbox show that service's icon, and `Track Info → Source` its name, instead of TIDAL's. It follows the service selected now, not a fact stored per track.
<!-- /PENDING-REVIEW #305 -->

### Changed

- `*-Data-Bundle.tar.gz` is in this Release for the last time (see How to update); the on-screen updater of 5.3.0.5 offers only `*-Update.img`.
- Decks on 5.3.0.5 verify signatures on every on-screen update and refuse older releases; use the installer's `--image` / `--bundle` to go back on purpose.

### Not in this release

- **Pull the stick and keep playing** (feasibility study only), **WAV / AIFF from the rekordbox source**, rekordbox Link instant waveforms, cues and grids, and the PRIME 4 G2 / SYSTEM ONE data partition size (#244).
- Two items of #286 (items 3 and 4).

### Known issues and limits

- **Spotify:** direct audio is disabled because Spotify blocks audio-key decryption for many accounts (librespot-org/librespot#1649, no known fix). Login, playlists, liked songs and search work; playback matches tracks on a service you are signed in to.
- **Apple Music** plays only on the second-generation units (SC LIVE 4, SC LIVE 2, PRIME 4+, PRIME GO+, Mixstream Pro+, Mixstream Pro Go, PRIME 4 G2, SYSTEM ONE) and needs an Apple Music subscription and the helper (PRIME 4 G2 / SYSTEM ONE can sign in natively). On first-generation units you can browse playlists but not load tracks: their Engine 5.3.0 has no Apple Music provider.
- **HFS+ read-write on second-generation 32-bit decks** (user-space driver) and on PRIME 4 G2 / SYSTEM ONE has not been tried on a real deck; an unplug while writing can still lose cached data, and a drive that fails the check stays read-only.
- **Very large rekordbox libraries:** the background conversion publishes only when no deck is using the stick and it has been quiet for 30 seconds; the list may blink once at the remount; timings on an SC5000 are estimates.
- **Bluetooth** adds about 150–250 ms: use it for monitoring, not beatmatching. On-deck Stems are not a neural network and keep some bleed between stems. High-speed SD card is experimental. Live Stream offers WAV only.
- Open follow-ups: the SoundCloud save in #247, Remote Library logging and Prepare for #268, large-stick recognition speed on a real stick (#253).
- TO-FILL-BEFORE-PUBLISH: list the real-deck checks that are still open at release time.

### How to update

- **Already running EngineX OS 5.3.0.4 or earlier — all 16 models:** connect the deck to Wi-Fi or Ethernet, open **`Settings → About / Update`** and tap **`Update`**. Music, library and settings are kept. Second-generation decks still on `5.3.0.2` or a locked `5.3.0.3` update through the `*-Data-Bundle.tar.gz` bridge in this Release, which also unlocks `*-Update.img` updates for later. **Decks on 5.3.0.4 and earlier do not verify signatures: update to 5.3.0.5 first; from then on every update is verified (#239).**
- **First-time install on a first-generation unit** (`SC5000`, `SC5000M`, `SC6000`, `SC6000M`, `PRIME 4`, `PRIME 2`, `PRIME GO`, `Mixstream Pro`): download your model's `*-Update.img` below, put it unrenamed in the root of a FAT32 or exFAT USB drive, plug it into the deck and choose it in **`Settings → About / Update → Firmware Update Mode → Options`**.
- **First-time install on a stock second-generation unit** (`SC LIVE 4`, `SC LIVE 2`, `PRIME 4+`, `PRIME GO+`, `Mixstream Pro+`, `Mixstream Pro Go`, `PRIME 4 G2`, `SYSTEM ONE`): update to official Engine OS 5.3.0 first, then run `EngineX-Installer-5.3.0.5-*` for your platform. **Back up first: writing the `/data` image over USB erases the internal music, library and settings.**
- **Going back to stock:** boot a 5.3.0.5 build once, shut down normally, then run `Restore-Official-Firmware.*`.

---

### SHA-256 Checksums (28 Release Artifacts)

`SHA256SUMS.txt`, `SHA256SUMS.txt.sig` and `RELEASE-TAG.txt` are uploaded with the files below (31 files in total).

#### One-Click Installer (`EngineX-Installer`)

| File | SHA-256 |
| --- | --- |
| `EngineX-Installer-5.3.0.5-windows-amd64.zip` | `TO-FILL-AFTER-BUILD` |
| `EngineX-Installer-5.3.0.5-macos-arm64.zip` | `TO-FILL-AFTER-BUILD` |
| `EngineX-Installer-5.3.0.5-macos-amd64.zip` | `TO-FILL-AFTER-BUILD` |
| `EngineX-Installer-5.3.0.5-linux-amd64.tar.gz` | `TO-FILL-AFTER-BUILD` |

#### First-Generation Firmware Images (`*-Update.img`)

| File | SHA-256 |
| --- | --- |
| `SC5000-5.3.0.5-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img` | `TO-FILL-AFTER-BUILD` |
| `SC5000M-5.3.0.5-EngineX-OS-Denon-SC5000M-Prime-JP08-Update.img` | `TO-FILL-AFTER-BUILD` |
| `PRIMEGO-5.3.0.5-EngineX-OS-Denon-PRIME-GO-JP11-Update.img` | `TO-FILL-AFTER-BUILD` |
| `SC6000-5.3.0.5-EngineX-OS-Denon-SC6000-Prime-JP13-Update.img` | `TO-FILL-AFTER-BUILD` |
| `SC6000M-5.3.0.5-EngineX-OS-Denon-SC6000M-Prime-JP14-Update.img` | `TO-FILL-AFTER-BUILD` |
| `PRIME4-5.3.0.5-EngineX-OS-Denon-PRIME-4-JC11-Update.img` | `TO-FILL-AFTER-BUILD` |
| `PRIME2-5.3.0.5-EngineX-OS-Denon-PRIME-2-JC16-Update.img` | `TO-FILL-AFTER-BUILD` |
| `MIXSTREAMPRO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-NH08-Update.img` | `TO-FILL-AFTER-BUILD` |

#### Second-Generation Unified Update Images (`*-Update.img`, Primary)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Denon-SC-LIVE-4-JP21-Update.img` | `TO-FILL-AFTER-BUILD` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Denon-SC-LIVE-2-JP20-Update.img` | `TO-FILL-AFTER-BUILD` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Denon-PRIME-4-Plus-JC11S-Update.img` | `TO-FILL-AFTER-BUILD` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Denon-PRIME-GO-Plus-JP11S-Update.img` | `TO-FILL-AFTER-BUILD` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Plus-NH08S-Update.img` | `TO-FILL-AFTER-BUILD` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Numark-Mixstream-Pro-Go-NH10-Update.img` | `TO-FILL-AFTER-BUILD` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Denon-PRIME-4-G2-JP24-Update.img` | `TO-FILL-AFTER-BUILD` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Rane-SYSTEM-ONE-RMZ2-Update.img` | `TO-FILL-AFTER-BUILD` |

#### Second-Generation Legacy OTA Data Bundles (`*-Data-Bundle.tar.gz`, last time)

| File | SHA-256 |
| --- | --- |
| `SCLIVE4-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `SCLIVE2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `PRIME4PLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `PRIMEGOPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `MIXSTREAMPROPLUS-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `MIXSTREAMPROGO-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `PRIME4G2-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
| `SYSTEMONE-5.3.0.5-EngineX-OS-Data-Bundle.tar.gz` | `TO-FILL-AFTER-BUILD` |
