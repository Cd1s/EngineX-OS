# EngineX OS — Universal Custom Firmware & High-Performance Backend Suite for Denon DJ Engine OS 5.0.4

<p align="center">
  <b>English</b> · <a href="./README.zh-CN.md">简体中文</a> · <a href="./README.zh-TW.md">繁體中文</a> · <a href="./docs/OPEN-SOURCE-FULL-MANIFEST.md">Full Technical Manifest & 642-Bug Audit</a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Base_Firmware-Engine_OS_5.0.4-0078ff?style=flat-square" alt="Base Firmware" />
  <img src="https://img.shields.io/badge/Kernel-Linux_6.6.119--rt_(PREEMPT__RT)-00e632?style=flat-square" alt="Kernel" />
  <img src="https://img.shields.io/badge/Models-SC5000_%7C_SC6000_%7C_PRIME_4_%7C_PRIME_GO_%7C_SC_LIVE_2-ff8800?style=flat-square" alt="Models" />
  <img src="https://img.shields.io/badge/Editions-Standalone_(Serverless)_%26_Server--Assisted-5ce4d5?style=flat-square" alt="Editions" />
</p>

**EngineX OS** (`sc5000-custom-os`) is a universal, multi-model custom OS & backend replacement suite for Denon DJ standalone players running **Engine OS 5.0.4** (`SC5000`, `SC5000M`, `PRIME GO`, `SC6000`, `SC6000M`, `PRIME 4`, `SC LIVE 2`).

It follows one strict engineering constitution:
> **100% Authentic Denon QML UI & Tactile Feel + Surgical Linux & C++ Backend Replacement.**
> Every DJ who walks up to the decks gets the exact stock Denon interface with zero learning curve and zero intrusive popups—while under the hood, slow C++ importers, synchronous SQLite disk stalls, flash telemetry bloat, CPU underclocking dropouts, and model-ID feature locks are surgically replaced by high-performance C `LD_PRELOAD` hooks and isolated Go daemons.

---

## 1. Live Hardware Screenshots (Captured via KMS `drmgrab` on SC5000 `JP07`)

| Native 4-Stem Playback & Official Pads Bar | Native Sidebar `Stems` Playlist (`0x6d3e00`) |
| :---: | :---: |
| ![Deck Stems Pads Bar](./docs/assets/screenshots/01-deck-stems-official-pads.png) | ![Library Native Stems List](./docs/assets/screenshots/02-library-native-stems-list.png) |
| *100% stock UI with Pioneer 3-Band waveform, stretched Rekordbox overview & official bottom `[ Vocal ] [ Melody ] [ Bass ] [ Drums ]` bar* | *Unlocked native `Stems` row (`typeRole = 5`) at row 0 of the library sidebar with track stem badges* |

| 5 Streaming Services Integrated Natively | On-Deck QR Code Login Modal |
| :---: | :---: |
| ![5 Streaming Platforms](./docs/assets/screenshots/03-sources-5-streaming-platforms.png) | ![QR Code Login Modal](./docs/assets/screenshots/04-streaming-qr-login.png) |
| *USB 3, SD Card, NetEase Cloud Music, Spotify, Deezer, SoundCloud & TIDAL in the native Source selector* | *Scan with your phone on the local LAN (Standalone Mode) or over 4G/5G via your self-hosted relay* |

| Streaming Playlists with Per-Service `Stems`, `[BPM / Key]` & Swipe-Left `Stems` | `EngineX OS` Custom Device Preferences (Language, Waveforms, Quality, Jog, Bluetooth) |
| :---: | :---: |
| ![Streaming Playlists & Swipe Stems](./docs/assets/screenshots/05-streaming-playlists-bpm-swipe.png) | ![EngineX OS Device Preferences](./docs/assets/screenshots/06b-preferences-device.png) |
| *Per-platform isolated `Stems` playlist at row 0, catalog BPM/Camelot Key preview columns & swipe-left `Stems` trigger* | *Trilingual UI (EN/简体/繁體), Denon/RGB/3-Band waveforms, Lossless/320k quality, Jog Light modes & unlocked Bluetooth* |

| `EngineX OS 5.0.4` GitHub OTA Update Checker (`About / Update`) |
| :---: |
| ![EngineX OS GitHub Update](./docs/assets/screenshots/06-preferences-enginex-github-update.png) |
| *Replaces Denon's official update check (`autoupdate.airmusictech.com`) with your GitHub repository (`Cd1s/sc5000-custom-os`)* |

---

## 2. Two Editions in One Codebase: **Standalone Edition (Serverless)** vs. **Server Edition**

Many DJs do not own a cloud VPS. **EngineX OS** is architected so that **one unified firmware works out-of-the-box as a 100% Standalone (Serverless) Edition**, and automatically upgrades to **Server-Assisted Edition** if you configure a self-hosted backend (`sc5000-relay` / `sc5000-stemsd`) on a cloud VPS or a home computer on your LAN.

### Feature Parity Matrix: Standalone (Serverless) vs. Server Edition

| Feature / Capability | **Standalone Edition (Serverless / Zero Server)** | **Server Edition (Self-Hosted VPS or Home PC)** | What Differs & Why? |
| :--- | :--- | :--- | :--- |
| **Native USB/SD `.stems` Playback & Bottom 4-Stem Bar** | ✅ **100% Supported** | ✅ **100% Supported** | Identical (`0x842644` + `0x6d3e00` C Hook). |
| **On-Demand Stems Rendering (USB & Streaming Tracks)** | ✅ **Supported On-Deck (`internal/stems/local.go`)**<br>Runs on `CPU3` (`SCHED_IDLE` + `nice 19`) using a zero-latency causal IIR + Mid-Side 4-stem separator (**2.48s per 60s audio = 24.2× real-time** on SC5000 ARMv7!). | ✅ **Supported via Remote Worker (`worker.py`)**<br>Streams via `X-Stems-Framing: 2` (`int16` + 3-stem residual) to `sc5000-stemsd` (**6.8× real-time over Wi-Fi / 33.8× compute** with full 2048-pt STFT + 2D median HPSS). | **Quality vs. Zero-Hardware**: Standalone splits 4 stems locally via causal IIR frequency/mid-side filters (exact sum = input, zero server needed); Server/Home-PC runs full 2D STFT harmonic-percussive median separation. |
| **Per-Platform Isolated Streaming `Stems` Lists** | ✅ **100% Supported** | ✅ **100% Supported** | NetEase, Spotify, Deezer & SoundCloud each get their own row-0 `Stems` playlist on the deck, never mixing with USB Stems. |
| **NetEase Cloud Music (网易云音乐)** | ✅ **100% Supported (Zero Server)**<br>Official app scans QR directly; downloads Lossless FLAC / 320k MP3 straight to the deck. | ✅ **100% Supported** | Identical. |
| **Deezer (Lossless FLAC / 320k MP3)** | ✅ **100% Supported (LAN QR + Local `sc5000-deemixd`)**<br>Public charts need no login. To link your account/ARL, scan the QR on the same Wi-Fi (`http://<deck-ip>:8080/m/deezer`). Audio downloads & decrypts 100% locally on the deck (`127.0.0.1:6595`). | ✅ **100% Supported (4G/5G QR + Headless Browser Login)**<br>Adds 4G/5G cross-network QR scanning and an interactive remote headless browser to sign into Deezer with email/password. | **Audio is always downloaded locally on the deck in both editions.** Server only adds 4G/5G QR reachability & headless browser login. |
| **Spotify & SoundCloud** | ✅ **100% Supported (LAN QR Login)**<br>Phone on the same Wi-Fi opens `http://<deck-ip>:8080/m/{spotify,soundcloud}` to link account; audio downloads locally on the deck. | ✅ **100% Supported (4G/5G Automatic OAuth Redirect)**<br>Adds automatic HTTPS OAuth callback over 4G/5G without copying loopback redirect URLs. | Standalone uses local Wi-Fi phone page; Server adds 4G/5G automatic HTTPS OAuth callback. |
| **Rekordbox Zero-Scan Instant Bridge (`rb-bridge`)** | ✅ **100% Supported** | ✅ **100% Supported** | Identical (runs in `edisksd` mount hook). |
| **Database Healer (`dbhealer`) & SQLite RAM Acceleration** | ✅ **100% Supported** | ✅ **100% Supported** | Identical. |
| **642 Bug Defenses, 1.2GHz Floor, 3 Languages, RGB/3-Band & GitHub OTA Check** | ✅ **100% Supported** | ✅ **100% Supported** | Identical. |

> **💡 Can I use my Home Computer (Mac / Windows / Linux) instead of buying a Cloud Server?**
> **Yes!** Both `sc5000-stemsd` (`tools/stems/worker.py`) and `sc5000-relay` accept private LAN HTTP connections (`http://192.168.x.x:6595` or `SC5000_STEMS_URL=http://192.168.x.x:6596`). You can run the worker on your laptop/desktop on the same Wi-Fi for server-grade STFT Stems rendering with zero cloud cost. See [**Issue #1**](https://github.com/Cd1s/sc5000-custom-os/issues/1) for upcoming zero-config mDNS auto-discovery and optional GPU Demucs support.

---

## 3. Empirical Performance Benchmarks: Stock Engine OS 5.0.4 vs. EngineX OS 5.0.4

Every optimization in **EngineX OS** is backed by hardware measurements on a real **Denon SC5000 Prime (`JP07`)**:

| # | Benchmark Metric | Stock Engine OS 5.0.4 / Initial Baseline | EngineX OS 5.0.4 (Measured on Hardware) | Speedup / Improvement |
| :-: | :--- | :--- | :--- | :--- |
| **1** | **Stems Separation Speed**<br>*(4-minute / 240s track)* | **Stock**: Locked/Disabled on SC5000 (`JP07`); no on-deck or streaming Stems.<br>**Unoptimized Remote**: `394.1s` (**0.61× real-time**) + `120.0s` EOF hang at 100%. | • **Server Mode (`X-Stems-Framing: 2` + Batch 1D Median)**: **`35.1s` (6.8× real-time)** over Wi-Fi (`33.8×` worker compute, `0.04s` exit).<br>• **Standalone On-Deck Mode (`CPU3` `SCHED_IDLE`)**: **`2.48s` per 60s audio (24.2× real-time)**. | **11.1× – 39× faster**<br>+ **120s tail hang eliminated (`0.04s`)** |
| **2** | **Rekordbox USB Mount & Delta Sync**<br>*(~6,000-track USB stick)* | • **First Mount**: `~8 minutes` (re-decodes every audio file on deck, often corrupting `m.db`).<br>• **Second Mount (Unchanged)**: `~15–24s` scan.<br>• **Delta Update (`GL-011`)**: `0/21` new tracks imported (exceeds 20s mount budget & rolls back). | • **First Mount (`>500` tracks)**: **`< 2 seconds`** (builds `m.db` on `tmpfs` from exported PDB/OneLibrary without touching audio files).<br>• **Second Mount (Unchanged)**: **`30 ms (0.03s)`** via `.rb-bridge-stamp`.<br>• **Second Mount (Delta Update)**: **`< 1 second`** on `tmpfs` (**100% new tracks & playlists synced**). | **~800× faster re-mount (`24s` $\rightarrow$ `0.03s`)**<br>+ **100% delta sync reliability** |
| **3** | **SoundCloud Long DJ Set Download**<br>*(1h50m, 106 MB set)* | • **Stock**: Stream-only, no local caching; frequent dropouts on slow venue Wi-Fi.<br>• **8-way Batch Baseline**: `60.3s`. | **16-Way Sliding-Window Range Downloader** (2 MB chunks, bounded on `CPU3` `SCHED_IDLE`): **`43.7s – 45.8s`** (saturates the SC5000 hardware NIC limit of `~2.3 MB/s`). | **27.5% faster (`60.3s` $\rightarrow$ `43.7s`)**<br>Hardware line-rate saturated |
| **4** | **SQLite Transaction Latency & eMMC Flash Telemetry Writes** | • **SQLite**: `synchronous=FULL` + disk temp tables (`-journal` creation/fsync on FAT32/exFAT stalls UI thread on every cue/history write).<br>• **Telemetry**: Continuous writes to `/data/.../posthog.db`, `events.db` & `CrashReportDB` + 4 `J:I:Takingfreez` threads every 20s. | • **SQLite Hook (`0x1cba858`/`0x1cba874`)**: Injects `PRAGMA synchronous=NORMAL; temp_store=MEMORY; cache_size=-8000;` (8 MB RAM cache + RAM temp tables).<br>• **Telemetry Hook**: Redirects `posthog.db`/`events.db` to `":memory:"` and blocks `crashpad_handler` (`ENOENT`).<br>• **Measured eMMC Writes**: **`0 Bytes`** written during 30s rapid track-switching stress test. | **100% flash telemetry writes eliminated (`0 B`)**<br>+ **>95% reduction in FAT32/exFAT `fsync` stalls** |
| **5** | **ALSA Real-Time Audio XRUN Rate**<br>*(450-frame / 4.68ms period @ 96kHz)* | • CPU drops to `126 MHz – 408 MHz` during idle, causing ALSA buffer underruns (XRUNs) when loading lossless tracks or high-res art.<br>• GPU, USB & Ethernet IRQs all contend on `CPU1`; `Analysis Queue` & `QQuickPixmapReader` preempt `CPU0`. | • **CPU Floor Locked at `1.2 GHz`** (`ondemand`, `up_threshold=50`, `sampling_down_factor=10`, steady `~72°C`); GPU idles on `simple_ondemand`.<br>• **Strict 4-Core Isolation**: `CPU0` = ALSA `ffb20000` IRQ (`FIFO 99`, `workqueue/cpumask=6`); `CPU1` = GPU/UI; `CPU2` = USB/SATA/ETH IRQs; `CPU3` = `Analysis Queue` / `Freeze Checker` / `J:I:Takingfreez` / `Dropbox*` (`SCHED_IDLE` + `IOPRIO_CLASS_IDLE`). | **0% ALSA XRUNs**<br>Zero audio dropouts during rapid track loads & background analysis |
| **6** | **Album Art Decode RAM & Long-Session Stability (`CF003` / `N006`)** | • Unbounded 3000×3000 PNG/JPEG covers decode to **`~36 MB` raw RGBA each**, spiking memory bandwidth and freezing jog displays after 45–120 minutes. | • `capCover` (`pkg/deemixclient/cover.go`) & `dbhealer` reject `>16MP` bombs and bilinearly downscale covers to **`<= 512×512` JPEG (`<= 256 KB`)**.<br>• Decoded bitmap footprint drops to **`<= 1 MB`**; gateway thumbnail cache capped at `128` entries (`<= 512 KB` each); `QQuickPixmapReader` pinned to `CPU1-3` (`nice 10`). | **>97% artwork RAM reduction (`36 MB` $\rightarrow$ `<= 1 MB`)**<br>Zero memory growth over multi-hour sets |

---

## 4. Complete Architectural Changes vs. Stock Engine OS 5.0.4

### 4.1 In-Memory Binary Hooks (`tools/ui-mod/engine_ui_hook.c` $\rightarrow$ `libengine-ui-hook.so`)
All patches are applied in RAM at startup after verifying the exact ELF `NT_GNU_BUILD_ID` (`af073865...`) and expected instruction bytes. The read-only rootfs binary `/usr/Engine/Engine` is never modified on disk, and creating `/data/sc5000-custom/ui/safe-mode` (or 3 rapid crashes within 90s via `engine-ui-guard`) automatically restores the stock UI:

| ELF Virtual Address / Symbol | Stock 5.0.4 Behavior | EngineX OS Replacement | Multi-Model Scope |
| :--- | :--- | :--- | :--- |
| `0xf6303c` (`prepare_config`) | Common return path of `getConfig()` | 8-byte jump to RX stub setting `[r0,#0xdb]=1` (`hasStems`), `[r0,#0x24c]=1` (`hasBluetooth`), `[r0,#0x24e]=1` (`hasBluetoothSendToAssignment`), `[r0,#0x276]=1` (remote Stems child support), `[r0,#0x289]=0` (`disableHfsPlus=false`), then replays literal loads & jumps to `0xf63044` | **Universal across all 7 models**: runs after `JP07` (`0xf6586c`), `JP08` (`0xf663f8`), `JP11` (`0xf665e0`), `JP13` (`0xf6709c`), `JP14` (`0xf67dcc`), `PRIME 4` (`0xf64124`) & `SC LIVE 2` (`0xf67fc8`) constructors |
| `0x842644` (`prepare_stems_feature`) | Early-exits when model config string is `"False"`, skipping `0x8429b0` | Intercepts before the `"False"` branch: binds `StemsPreRenderedPlayback` (`base+0x29bc83c`) `[+4]` to the native `true` byte (`base+0x258e185`) and jumps to native enabled-feature registration at `0x8429b0` | **Universal across all models** without offline string patching |
| `0x6d3e00` | `beq` skips inserting native `Stems` row in library sidebar | `b` (`07 00 00 ea`) unconditionally preserves `r8` eligibility | **Universal**: unlocks row-0 native `Stems` playlist (`typeRole = 5`) |
| `0x85ef9c` / `0x85e5f8` | Looks up stock stems binary | Redirects `RenderStemsJob` child execution to `/usr/local/bin/stems-processor` | Bridges native Stems queue to Standalone `LocalRender` or Remote `sc5000-stemsd` |
| `0x336740` / `0x336758` / `0x33652c` / `0x1db8e90` | Built-in `TidalAPI` talks to `api.tidal.com` and only recognizes `flac`/`aac` extensions | Redirects `TidalAPI` to `http://127.0.0.1:8080/v1/` & `/au//`; maps `"aac\0"` $\rightarrow$ `"mp3\0"` (`mp4a.40.x` AAC unaffected) | Powers NetEase, Spotify, Deezer, SoundCloud & TIDAL (with native `.mp3` and `.flac` loading) |
| `0x1897e70` / `0x1897dcc` / `0x1678644` / `0x58af34` / `0x167295c` / `0x16728c0` | Rejects unknown filesystems; rewrites `hfsplus` to missing `ufsd`; auto-ejects read-only drives | Marks all filesystems supported; preserves native `hfsplus` driver; disables auto-eject & read-only rejection | Full HFS+ (with `overlayfs`), APFS, NTFS (`ntfs3`), exFAT & FAT32 support |
| `0x1cba858` (`sqlite3_open`) / `0x1cba874` (`sqlite3_open_v2`) / `0x1c91f74` (`sqlite3_exec`) | Statically linked SQLite 3.45.3 opens DBs with default `FULL` sync & file temp store | Verified via `sqlite3_api_routines` (`0x29980a4` slots 70/135/55): redirects `posthog.db`/`events.db` to `":memory:"`; runs `PRAGMA synchronous=NORMAL; temp_store=MEMORY; cache_size=-8000;` on all library DBs | Eliminates FAT32/exFAT journal `fsync` stalls and flash telemetry writes |
| `pthread_setname_np` / `prctl` / `__prctl_time64` / `pthread_setschedparam` / `pthread_setaffinity_np` | Default scheduling; `RenderStemsJob` aborts with `EINVAL` on 4-core CPUs | 1) Clamps out-of-range affinity masks to online cores (`0..3`);<br>2) Forces `Analysis Queue`, `OfflineAnalyzer`, `Dropbox*`, `Beatport*`, `J:I:Takingfreez`, `Freeze Checker`, `sentry*`, `CrashHandler*` to `CPU3` + `SCHED_IDLE` + `nice 19` + `IOPRIO_CLASS_IDLE`;<br>3) Pins `QQuickPixmapRea` & `FileInfoThread` to `CPU1-3` (`nice 10`) | Protects `CPU0` (ALSA IRQ) and `CPU1` (`EMain` UI) from background preemption |
| `snd_pcm_sw_params` / `snd_pcm_sw_params_set_tstamp_type` | Secondary ALSA path (`0x167cdf8`) leaves `tstamp_type = 0` (`GETTIMEOFDAY` / `CLOCK_REALTIME`) | Upgrades `tstamp_type == 0` to `SND_PCM_TSTAMP_TYPE_MONOTONIC_RAW (2)` (matching `0x165bcd0` and `clockid=4` consumer) while preserving existing `1`/`2` and `silence_threshold=0` | Defends against NTP clock-step audio glitches (`RA002`) with zero pop risk |
| `getaddrinfo` | Queries `autoupdate.airmusictech.com/PrimeUpdates.xml` (`0x386f6c`) for official firmware updates | Blocks `autoupdate.airmusictech.com` with `EAI_NONAME` while preserving `device.inmusicbrands.com` (auth/unlock), `connman`, and `api.github.com` | Replaces official Denon update check with GitHub OTA update checker |

---

## 5. 642 Historical Bug Audit Summary (Engine OS 1.x–5.0.4)

A complete audit of all **642 historical defects** across Engine OS 1.x–5.0.4, Engine DJ Desktop (229 items), and third-party compatibility suites (`libdjinterop`, `Seabass`, `StageLinQ`, 26 items) is documented in [**`docs/OPEN-SOURCE-FULL-MANIFEST.md`**](./docs/OPEN-SOURCE-FULL-MANIFEST.md):

- **✅ ~88 Active 5.0.4 Bugs Directly Cured / Defended by EngineX OS**:
  - **Audio Dropouts & Stutters (`CF001`, `CF002`, `N001`, `N002`, `N003`, `N006`, `LC001`, `LC003`, `RA002`)**: Cured via 1.2 GHz CPU floor, `CPU0` ALSA FIFO 99 isolation, `workqueue/cpumask=6`, `QQuickPixmapReader`/`Analysis Queue` thread demotion, `<=512×512` cover art cap, 1 MiB block `read_ahead_kb`, and ALSA `MONOTONIC_RAW` timestamping + playback-time `Sync Time` lock.
  - **Database Corruption, Slow Mounts & 45-Minute Degradation (`CF003`, `CF004`, `CF005`, `CF006`, `GL-002`, `GL-005`, `GL-010`, `GL-011`, `GL-012`, `GL-013`, `CF049`, `H019`, `H055`, `N005`, `RA011`, `EOS-01`–`EOS-09`)**: Cured via `libedisksd-hook.so` + `rb-bridge` `tmpfs` delta sync + `dbhealer` chain/schema/opaque-color healing + SQLite RAM PRAGMAs + bounded single-eviction caches + AIFF 80-bit float & 500 BPM clamp recovery.
  - **Multi-Model Hardware & Power Management (`CF017`, `CF018`, `H134`, `LA-040`)**: Cured via `usbcore.autosuspend=-1` and JMicron JMS578 SATA link power management (`max_performance`).
- **🟢 ~525 Historical 1.x–5.0.3 & Desktop Bugs Resolved in Base or Bypassed**:
  - Includes 229 desktop software bugs completely bypassed by on-device `rb-bridge` and `stream-sync`.
- **⚠️ Honest Disclosure of Remaining Closed-Source C++ DSP / Hardware MCU Limits (~29 items)**:
  - Internal C++ DSP effect algorithms (`H145`/`H163` FX channel-switch pop, `RA012` Reverse Reverb Key Lock path, `H130` Elastique Key Lock toggle artifact), multi-deck Sync state machine (`RA004`/`LA-033`), and M-series motorized platter MCU firmware (`CF014`/`LC030`) reside inside closed-source floating-point DSP / MCU loops and are intentionally left untouched to guarantee **Zero Audio Pop / Zero XRUN** safety.

---

## 6. Build, Verification & Custom GitHub OTA Setup

```sh
# 1. Run full Go race test suite & rootfs-overlay regression tests
go test -race ./...
bash tests/test_rootfs_overlay.sh
python3 -m unittest discover -s tools/az01-packer -v
python3 -m unittest discover -s tools/stems -v

# 2. Cross-compile static ARMv7 deck binaries
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/sc5000-suite ./cmd/sc5000-suite
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o out/stems-processor ./cmd/stems-processor

# 3. Cross-compile LD_PRELOAD C hooks & run QEMU ARMv7 instruction-level tests
arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -shared -fPIC -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=0 -o out/libengine-ui-hook.so tools/ui-mod/engine_ui_hook.c -ldl
sh tools/edisksd-hook/build.sh

# 4. Build trilingual QML UI packs (en / zh-Hans / zh-Hant)
python3 tools/ui-mod/build_packs.py --qrc /path/to/stock-5.0.4-qrc --out out/ui-packs
```

### Configuring Your Own GitHub Repository for OTA Update Checks
By default, `Settings -> About / Update` checks `Cd1s/sc5000-custom-os` via `GET /v1/sc5000/update`. To point the deck at your own fork:
- Add `github_repo=your-org/your-repo` to `/data/sc5000-custom/ui/settings.conf`, or set `SC5000_GITHUB_REPO=your-org/your-repo`.
