# `tools/shadow-twin` — Dual-Deck SC5000 (JP07) Shadow-Track Sandbox

`shadow-twin` runs two complete SC5000 (`JP07`) instances (`A` and `B`) side by side on `Oracle-Batam-arm` without touching physical decks. Both instances execute the stock ARM32 (`armhf`) Engine 5.3.0 binary, `libengine-ui-hook.so`, `OfflineAnalyzer`, and `stream-sync` inside isolated Linux namespaces, render full 1280×800 OpenGL scenes via `eglfs_kms` + Mesa `llvmpipe` (LLVM 19 NEON JIT), and communicate over a virtual Ethernet (`veth`) link shaped by `tc netem` to match real hardware latency.

---

## 1. Architecture

| Component | Instance `A` (Left / Player 1) | Instance `B` (Right / Player 2) |
|---|---|---|
| Network namespace | `shadow-twin-a` | `shadow-twin-b` |
| Link IP (`eth0` veth) | `192.168.100.1/24` | `192.168.100.2/24` |
| `four_deck_role` | `13` (owns Deck 1 & 3; shadows Deck 2 & 4) | `24` (owns Deck 2 & 4; shadows Deck 1 & 3) |
| Default test track | `/data/music/track-a.wav` (128.00 BPM, 90s) | `/data/music/track-b.wav` (132.00 BPM, 90s) |
| Rootfs stack | Unpacked 5.3.0 `rootfs.ext2` (ro) + `rootfs-overlay` + instance `upper` | Same |
| OpenGL QPA | `eglfs` (`eglfs_kms`, `"headless": "1280x800"`, `/dev/dri/card0`, `llvmpipe`) | Same |

### Network Link (`veth` + `tc netem`)
- Physical SC5000 inter-deck RTT measured read-only (`deck L 'ping -c 50 192.168.1.238'`):
  `min/avg/max/mdev = 1.815 / 4.201 / 23.819 / 3.412 ms` (0% loss).
- Each side of the `veth` pair applies `tc qdisc replace dev eth0 root netem delay 2.0ms 1.1ms distribution paretonormal`, producing a ~4 ms RTT with a realistic pareto-normal tail across Engine Link (TCP/UDP) and `stream-sync` (`/peer/decks`, `/peer/file`, `/peer/display`).

### Dual Headless `eglfs_kms` OpenGL Rendering & Screenshot Capture
- Setting `"headless": "1280x800"` in `/usr/Engine/ScreenConfiguration/Default/ScreenConfiguration.json` activates `QEglFSKmsGbmDevice::createHeadlessScreen()` in Qt 6.7.2 (`libQt6EglFsKmsGbmSupport.so.6.7.2`), which creates a 1280×800 GBM surface on `/dev/dri/card0` without acquiring DRM master (`drmModeSetCrtc` / `drmModePageFlip`).
- Both instances `A` and `B` render simultaneously with Mesa `llvmpipe` (LLVM 19 JIT) and pace `_ZN19QEGLPlatformContext11swapBuffersEP16QPlatformSurface` at 60 Hz (`SBX_HEADLESS_VSYNC=1`).
- Screenshots grab `root.parent` (`QQuickRootItem`), capturing both `EmbeddedMainWindow` and `QQuickOverlay` (Control Center drawer, `Layout → View`, `Preferences → Display`, and modal dialogs).
- Every screenshot is saved with the active development line and commit hash in its filename (`<name>-<line_slug>-<commit>-<side>.png`) and a bottom-right corner overlay (`<line> @ <commit> | <side> (role <role>) | mode=<mode> | <name>`).

### Audio Playback Clock Evaluation (`alsa_null_fixture.c`)
We evaluated three ALSA clock options on `Oracle-Batam-arm` (`Linux 7.2.9+deb13-arm64`):
1. **`snd-dummy`**: Not built in the host kernel (`/lib/modules/7.2.9+deb13-arm64` has no `snd-dummy.ko`).
2. **`snd-aloop`**: Present in the kernel, but its card name (`Loopback`) does not match `Engine`'s `ALSADevice` USB card filter (`"JP07"` / `"USB15e4:0001"`), so `Engine` refuses to attach it without an ALSA control shim.
3. **ALSA `null` PCM + `CLOCK_MONOTONIC` `TIMER_ABSTIME` fixture (`alsa_null_fixture.c`) — Selected**:
   - Presents card 0 as `"JP07"` (`USB15e4:0001`) with 4 playback channels and 2 capture channels (`S32_LE`).
   - Paces both `AudioOut` (`snd_pcm_writei`, stream 0) and `AudioIn` (`snd_pcm_readi`, stream 1) against per-stream absolute `CLOCK_MONOTONIC` deadlines (`clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, ...)`) at 48,000 frames/s (`450` frames = `9.375 ms` per DSP block).
   - **Clock accuracy**: Writes telemetry to `/tmp/alsa-clock.txt` (exposed in `shadow-twin.sh status`). Measured long-window drift is **`< 2 ppm`** (`-0.04` to `-1.6 ppm`), and `TrackPosition` advances at **`1.000x`** real-time speed (`59.128` track seconds over `59.118` wall seconds).

---

## 2. Two Development Lines & Version Tracking

- **Default sandbox line**: `origin/agent/sandbox-main-20261110`
- **Hardware comparison line**: `origin/agent/shadow-grid-halfwave-20261110`

`shadow-twin.sh update [ref]` maintains a dedicated detached git worktree at `/var/tmp/shadow-twin/src` so switching between branches or historical commits never clobbers `tools/shadow-twin/` in the main checkout.

```bash
# Fetch origin, build and deploy latest default sandbox line (origin/agent/sandbox-main-20261110)
tools/shadow-twin/shadow-twin.sh update

# Switch to the hardware line or any specific commit to reproduce/compare issues
tools/shadow-twin/shadow-twin.sh update agent/shadow-grid-halfwave-20261110
tools/shadow-twin/shadow-twin.sh update 8f5bbdb --mode engine
tools/shadow-twin/shadow-twin.sh update 44f5ede --mode display

# Check running line/commit on A & B vs upstream sandbox-main and shadow-grid-halfwave
tools/shadow-twin/shadow-twin.sh status --fetch
```

---

## 3. Single-Command Operations

```bash
# 1. Start both instances (display mode: shadow_legacy_engine=0, or engine mode: =1)
tools/shadow-twin/shadow-twin.sh up --mode display
tools/shadow-twin/shadow-twin.sh up --mode engine

# 2. Hot-redeploy after editing Go or QML/C code (~26s for both instances)
tools/shadow-twin/shadow-twin.sh deploy --local
tools/shadow-twin/shadow-twin.sh deploy --mode engine

# 3. Synthesize (if needed), load, and analyze non-copyright tracks on A (Deck 1) and B (Deck 2)
tools/shadow-twin/shadow-twin.sh load

# 4. Start/stop playback, Master/Sync, and pitch-bend nudge
tools/shadow-twin/shadow-twin.sh play start
tools/shadow-twin/shadow-twin.sh sync A
tools/shadow-twin/shadow-twin.sh nudge A:Forward:120 B:Backward:80
tools/shadow-twin/shadow-twin.sh play stop

# 5. Switch views or open Control Center / Layout / Preferences pages
tools/shadow-twin/shadow-twin.sh view 4deck AB
tools/shadow-twin/shadow-twin.sh view control-center A
tools/shadow-twin/shadow-twin.sh view layout A
tools/shadow-twin/shadow-twin.sh view layout-4deck A
tools/shadow-twin/shadow-twin.sh view preferences-display A

# 6. Capture eglfs OpenGL screenshots (individual A/B + side-by-side AB + JSON metadata)
tools/shadow-twin/shadow-twin.sh shot display-only AB --ui 4deck
tools/shadow-twin/shadow-twin.sh shot control-center A --ui control-center
tools/shadow-twin/shadow-twin.sh shot layout-view A --ui layout
tools/shadow-twin/shadow-twin.sh shot preferences-display A --ui preferences-display

# 7. Run a 60-second performance window (frame swap intervals >33ms, EMain CPU %, GC count)
tools/shadow-twin/shadow-twin.sh perf window-60s --seconds 60

# 8. Measure veth netem ping RTT between A and B
tools/shadow-twin/shadow-twin.sh ping -c 50

# 9. Stop sandbox and clean up all mounts, namespaces, worktrees, and scratch files
tools/shadow-twin/shadow-twin.sh down --clean
```
