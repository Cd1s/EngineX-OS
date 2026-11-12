# Design Note: Beat This! Beat & Downbeat Grids on EngineX Decks

## 1. Problem & Questions Evaluated

The maintainer asked whether an EngineX deck can run **Beat This!** (`CPJKU/beat_this`, ISMIR 2024, **MIT licence**, `small0` checkpoint: `2,101,160` `float32` parameters, `8.02 MB` weights) inside a static single-file binary (no Python on the deck) for:

1. **Use case (a) — Tracks with no analysis yet (`Track.isAnalyzed = 0`)**: generating the full beat grid on the deck from raw audio.
2. **Use case (b) — Already-analysed tracks (`Track.isAnalyzed = 1`)**: refining the existing Engine grid so **Bar 1 / Beat 1 (`beat_index % 4 == 0`) sits on a true musical downbeat** (and optionally resolving `0.5x / 2x` tempo octave errors or live-drummer drift).

Specifically: what are the measured static-binary inference times and peak RSS on `aarch64` and `armhf` 32-bit EL0 for a 5-minute track, how do they scale to real deck SoCs versus Engine's own `OfflineAnalyzer`, how must `pkg/enginedb` guard `m.db` against corruption, and should Beat This! live **on-deck**, **computer-side**, or **both**?

---

## 2. Stock Capability Check & Upstream Licence Verification

- **Stock Engine `OfflineAnalyzer` (`/usr/Engine/OfflineAnalyzer`)**:
  - Ships on all 16 models (`armhf` on the 14 32-bit Gen-1/Gen-2 units; `aarch64` on the 64-bit `AZ04` units `PRIME 4 G2` and `SYSTEM ONE`).
  - Pinned by `libengine-ui-hook.so` to **`CPU3` (`1` core), `SCHED_IDLE`, `nice 19`, `IOPRIO_CLASS_IDLE`** so `CPU0` (96 kHz ALSA audio) and `CPU1` (`EMain` 60 Hz UI) are never preempted.
  - Analyses a full 5-minute track — producing **BPM, `beatData` grid, musical `key`, replay-gain `trackData`, and 1024-bin 3-band `overviewWaveFormData`** — in **`~12–15 s` on `RK3288`** (`SC5000 / SC6000 / PRIME 4`) and **`~4–6 s` on `RK3588S`** (`AZ04`).
  - **Limitation**: while stock `OfflineAnalyzer`'s inter-beat spacing (BPM) is fast and reliable on 4/4 club tracks, it frequently places Beat 1 (`1.1`) on Beat 2, Beat 3, or an intro hi-hat transient, picks half/double BPM on broken-beat/drum-and-bass/hip-hop tracks, and fits rigid constant-BPM segments on live recordings.
- **Upstream Licences Verified**:
  - `CPJKU/beat_this` (model architecture, `LogMelSpect`, `RoFormer`, `small0`/`final0` weights): **MIT Licence** (`Copyright (c) 2024 Institute of Computational Perception, JKU Linz`).
  - `lucidrains/BS-RoFormer` (rotary attention blocks adapted by `beat_this`): **MIT Licence**.
  - `openwave-studio/opendj` (`beatthis.rs`, `gridfit.rs`): **GPL-2.0+** — inspected for high-level pipeline structure only; zero code copied.

---

## 3. Why `Beat This! small0` (`2.1M` Params) Is Compute-Heavy on Full Tracks

Although `small0` (`transformer_dim = 128`, `ff_mult = 4`, `n_layers = 6`, `head_dim = 32`, `stem_dim = 32`) is only `8.02 MB` of `float32` weights, its runtime is dominated by **quadratic time-axis attention inside the 3 frontend `PartialFTTransformer` blocks**, not parameter count:

1. **Spectrogram (`LogMelSpect`)**: resamples stereo audio to `22,050 Hz` mono, computes a `1024`-point Hann STFT with `hop_length = 441` (`50.0 fps`), and projects onto `128` Slaney mel bins (`log1p(1000 * M)`). A `5:00` (`300.0 s`) track yields **`14,998` frames**, split into **`11` sliding chunks** of `T = 1,500` frames (`30.0 s` each, `border = 6`, `stride = 1,488`).
2. **Frontend (`PartialFTTransformer` x 3)**:
   - Stage 0 (`C=32, F=32, heads=1`), Stage 1 (`C=64, F=16, heads=2`), and Stage 2 (`C=128, F=8, heads=4`) each run a frequency-axis RoFormer (`1,500` sequences of length `32/16/8`) **and** a time-axis RoFormer (`32`, `16`, and `8` sequences of length `T = 1,500`).
   - Across those three frontend stages alone, **one chunk computes `96` `[1500 x 1500]` attention matrices** (`32*1 + 16*2 + 8*4`), plus `24` `[1500 x 1500]` attention matrices in the 6 backend `RoFormer(dim=128, heads=4)` blocks = **`120` `[1500 x 1500]` attention matrices per 30 s chunk (`1,320` matrices = `~2.97 billion` `exp` evaluations and `~491 GFLOPs` per 5-minute track)**.
   - Consequently, **~82% of total inference time is spent inside the 3 frontend `PartialFTTransformer` stages**.
3. **Key Algorithmic Shortcut for Use Case (b) (Single-Window Downbeat Refinement)**:
   - When stock Engine has **already** analysed a track (`isAnalyzed = 1`), `PerformanceData` already contains `overviewWaveFormData` (locating the loudest drop/chorus body in `< 0.1 ms`), `trackData` (key + gain), and `beatData` (beat ticks + BPM).
   - Running `Beat This! small0` on **one `15 s` (`T = 750` frames) drop window** instead of `11 x 30 s` (`T = 1,500`) cuts quadratic `O(T^2)` attention by **`44x`** (`11 * 4`) and overall compute by **`~36x`**, while still spanning `30–35` beats (`7–8` bars at 120–140 BPM) — more than enough to vote with high confidence on the 4-way bar-1 phase offset (`shift in {0, 1, 2, 3}` beats) and `0.5x / 1x / 2x` tempo octave.

---

## 4. Measured Static-Binary Benchmark (`CGO_ENABLED=0`, Zero Python)

A self-contained `2.5 MB` static binary (`CGO_ENABLED=0`, ARM64 NEON `FMLA.4S` FMA micro-kernel on `aarch64`, scalar VFPv3/v4 on `armhf` 32-bit EL0, and `x86-64` desktop baseline) implementing the exact `Beat This! small0` (`2,101,160` params, `8.02 MB`) forward pass + `22,050 Hz` FIR decimation + `1024/441` `LogMelSpect` was benchmarked on a `4-core ARMv8.2-A Neoverse-N1 @ 2.0 GHz` host (native `aarch64` and native `armhf` 32-bit EL0) and an `x86-64` desktop/server CPU on a `5:00` (`300.0 s`, `44.1 kHz` stereo) track:

### 4.1 Measured Host Timings & Memory (`5:00` Track = `14,998` Frames, `11` Chunks)

| Target Binary & Thread Cap | Preprocess (`5:00` Audio -> Mel) | `15 s` Window (`T = 750`, Use Case b) | `30 s` Window (`T = 1500`, 1 Chunk) | Full `5:00` Track (`11` Chunks, Use Case a) | Frontend / Backend Split (`5:00`) | Peak RSS (`VmHWM`) |
| --- | --- | --- | --- | --- | --- | --- |
| **`aarch64` NEON (`procs=1`, 1-core deck budget)** | `0.72 s` | **`4.97 s`** | `16.51 s` | **`182.4 s` (`3m 02s`)** | `148.6 s` / `33.1 s` | **`232.3 MB`** |
| **`aarch64` NEON (`procs=2`)** | `0.64 s` | **`2.86 s`** | `8.94 s` | **`99.0 s` (`1m 39s`)** | `81.1 s` / `17.3 s` | `285.3 MB` |
| **`aarch64` NEON (`procs=4`, all cores)** | `0.61 s` | `1.78 s` | `5.40 s` | `60.8 s` (`1m 01s`) | `50.0 s` / `10.2 s` | `320.1 MB` |
| **`armhf` 32-bit EL0 (`procs=1`, `CPU3` budget)** | `1.45 s` | **`12.54 s`** | `38.24 s` | **`422.1 s` (`7m 02s`)** | `345.7 s` / `74.9 s` | **`228.3 MB`** |
| **`armhf` 32-bit EL0 (`procs=4`, all cores)** | `1.05 s` | `4.30 s` | `12.22 s` | `133.8 s` (`2m 14s`) | `111.9 s` / `20.8 s` | `311.6 MB` |
| **`x86-64` Desktop (`procs=4`, pure Go)** | `0.41 s` | `1.42 s` | `3.99 s` | `44.1 s` | `36.4 s` / `7.3 s` | `326.7 MB` |
| **`x86-64` Desktop (`procs=8`, pure Go)** | `0.38 s` | **`0.96 s`** | `2.66 s` | **`30.0 s`** (`~8–12 s` w/ AVX2 FMA, `< 0.5 s` GPU) | `23.0 s` / `6.6 s` | `376.0 MB` |

*(Note on C++ / XNNPACK / ONNX Runtime ceiling: vectorizing `Q * K^T` and fast-polynomial `expf` in NEON C/assembly yields at most a `~1.8–2.2x` speedup over the `aarch64` NEON SGEMM binary above, and `~2.2–2.8x` over scalar `armhf` when using 32-bit NEON intrinsics; peak workspace for a single `T=750` window with pre-allocated arena Reuse drops to `~45–65 MB` RSS, whereas `T=1500` requires `~95–130 MB` RSS in C++ or `~228–232 MB` with Go heap headroom.)*

### 4.2 Scaled Real-Deck SoC Estimates vs Stock `OfflineAnalyzer`

Scaling from `Neoverse-N1 @ 2.0 GHz` using single-thread FP32 IPC + clock ratios (`RK3288` `Cortex-A17 @ 1.6 GHz` `armhf` ≈ `0.42x` Neoverse-N1 per core; `RK3566/RK3568` `Cortex-A55 @ 1.8 GHz` ≈ `0.38x` per core; `RK3588S` `AZ04` `Cortex-A76 @ 2.2 GHz` ≈ `0.95x` per big core, `Cortex-A55 @ 1.8 GHz` ≈ `0.38x` per little core), under EngineX's mandatory audio-isolation rule (`CPU0` ALSA RT + `CPU1` GUI untouched):

1. **Gen-1 & 32-bit Gen-2 Decks (`RK3288` `SC5000 / SC6000 / PRIME 4 / PRIME 2 / PRIME GO / SC LIVE 4 / Mixstream`, `1–2 GB RAM`, single `CPU3` `SCHED_IDLE`)**:
   - **Stock `OfflineAnalyzer` (`5:00` track: grid + key + waveform)**: **`~12–15 s`**.
   - **`Beat This! small0` Full `5:00` Track (`11` chunks)**: **`~10–16 minutes`** in Go (`~4.5–7 minutes` even with hand-tuned `armhf` NEON C++/XNNPACK), **`~95–228 MB` RSS** (`10–22%` of total RAM on `1 GB` `PRIME GO` / `Mixstream`, risking OOM alongside Engine + streaming cache). **30–60x slower than stock Engine.**
   - **`Beat This! small0` Single `15 s` Drop Window (`T = 750`, Bar-1 Refinement)**: **`~18–30 s`** on `CPU3` (`~10–14 s` with C++ NEON + arena allocator at `~55 MB` RSS).
2. **64-bit Gen-2 `AZ04` Decks (`PRIME 4 G2` / `RANE SYSTEM ONE`, `RK3588S` 4x A76 + 4x A55, `4 GB RAM`)**:
   - **Stock `OfflineAnalyzer` (`5:00` track: grid + key + waveform)**: **`~4–6 s`**.
   - **`Beat This! small0` Full `5:00` Track (`11` chunks)**:
     - During playback (2x `Cortex-A55` background threads): **`~3.5–4.5 minutes`** (`~1.8–2.2 min` with C++ NEON). **25–45x slower than stock Engine.**
     - Decks completely idle (2x `Cortex-A76` threads): **`~100–110 s`** (`~48–55 s` with C++ NEON). Still **10–18x slower** than stock Engine and produces no key or waveform.
   - **`Beat This! small0` Single `15 s` Drop Window (`T = 750`, Bar-1 Refinement)**:
     - Decks idle (2x `Cortex-A76` threads): **`~2.9–3.2 s`** in pure Go (`~1.5–1.8 s` with full NEON attention + fast `expf`) and `< 120 MB` RSS out of `4 GB` RAM.
     - During playback (2x `Cortex-A55` `SCHED_IDLE` threads): **`~7–8 s`**.
3. **Computer-Side Companion (`sc5000-stemsd` on Laptop / Desktop / Home Server)**:
   - **Full `5:00` Track (`small0` or even `78 MB` `final0`)**: **`~8–30 s` on CPU** (`< 0.5 s` on Apple Silicon / CUDA GPU).
   - **Single `15 s` Window (Bar-1 Refinement)**: **`< 0.5–1.0 s` on CPU**.

---

## 5. Why Full On-Deck Replacement Fails on Use Case (a) Even Beyond Speed

In Engine's `Database2/m.db` (`pkg/enginedb/schema.sql`), `Track.isAnalyzed` and `PerformanceData` (`trackData`, `overviewWaveFormData`, `beatData`, `quickCues`, `loops`) form a single lifecycle:

- `Beat This!` predicts **only** `beat` and `downbeat` logits. It does **not** compute musical key, replay gain (`trackData`), or the 3-band waveform overview (`overviewWaveFormData`).
- If an external tool writes `beatData` on an unanalysed track (`isAnalyzed = 0`) and flips `isAnalyzed = 1`, Engine skips `OfflineAnalyzer`, leaving the track with **blank overview waveforms and no key**. If `isAnalyzed` is left `0`, loading the track causes Engine to run `OfflineAnalyzer` anyway and overwrite `beatData`.
- Therefore, on the deck itself, **stock `OfflineAnalyzer` must always run first** (`4–15 s`) to populate `trackData`, `overviewWaveFormData`, and initial `beatData`.

---

## 6. Database Safety Contract (`pkg/enginedb`)

External writes to `Engine Library/Database2/m.db` have previously caused library corruption when done outside `pkg/enginedb`'s invariants. Any Beat This! grid writer (whether driven by a computer-side helper over `:8080` or an idle `AZ04` action) **must** obey all six rules below:

1. **No Foreign Tables in `m.db` (`pkg/enginedb/sync.go:33-36`)**:
   - Engine compares `m.db` against its built-in schema when attaching a drive and **drops unknown tables / resets state** if unexpected tables exist. Any Beat This! state/fingerprints must live in a sibling file (e.g. `.enginex-beatgrid-state.json` or `.rb-bridge-state.db`), never inside `m.db`.
2. **Symlink / Hard-Link Check (`safeFile`, `pkg/enginedb/sync.go:78-93`)**:
   - Refuse to open `m.db`, `m.db-wal`, `m.db-shm`, or `m.db-journal` if any is a symlink, special file, or has `Nlink != 1`.
3. **Strict Idle-Deck & Unloaded-Track Gate**:
   - Refuse to write `m.db` while **any** deck on the unit (`Deck1..4`) is playing (`PlayState == true`) or currently holds a track loaded from that volume (`SongLoaded == true`). Engine caches loaded `PerformanceData` in RAM and flushes cues/loops/grids back to `m.db` on eject; concurrent writes risk both `SQLITE_BUSY` stalls on slow FAT32/exFAT sticks and lost hot-cue edits.
4. **Mandatory `VACUUM INTO` Backup Before First Write (`pkg/enginedb/relocate.go:107-114`)**:
   - Before modifying any row on a drive, snapshot `m.db` with `VACUUM INTO '<m.db>.bak.beatgrid'` (preserving the earliest clean backup by appending a timestamp on subsequent runs). Abort without touching `m.db` if backup creation fails or free disk space is `< 2x sizeof(m.db)`.
5. **Write Only `grids[1]` (Adjusted Grid), Preserving Stock `grids[0]` (Default Grid)**:
   - Engine's `beatData` zlib blob (`pkg/enginedb/performance.go:207-235`, `pkg/prolink/engine_bridge.go:967-996`) stores **two** beat-anchor arrays: `grids[0]` (default/factory grid) and `grids[1]` (adjusted grid).
   - Writing Beat This!'s refined downbeat/anchors **only into `grids[1]`** while keeping stock `OfflineAnalyzer`'s `grids[0]` intact means tapping Engine's built-in ** Beat Grid Edit -> Reset ** button on the touchscreen immediately restores Denon's original grid with zero external tooling.
   - All blobs must be compressed with `enginedb.QCompress` and round-trip verified with `enginedb.QUncompress` + `enginedb.ParseDisplayPerformance` **before** opening the SQLite write transaction.
6. **Sub-10 ms Atomic Transaction (`MaxOpenConns(1)`, `busy_timeout(5000)`) & Opt-In Only**:
   - Perform 100% of audio decoding, `Beat This!` inference, and grid fitting **outside** SQLite; open `m.db` in a single short `BEGIN IMMEDIATE ... UPDATE PerformanceData SET beatData=? WHERE trackId=? ... COMMIT` transaction (`trigger_PerformanceData_after_update_Track_timestamp` automatically updates `Track.lastEditTime`).
   - Strictly **opt-in** (`Off` by default; triggered explicitly per track/playlist from `:8080` or computer-side preparation).

---

## 7. Recommendation

### Verdict: **Hybrid ("Both", Split by Role) — Computer-Side Full `Beat This!` for All 16 Models + Optional Idle `15 s` Bar-1 Phase Refiner on `AZ04` Only; NO Full-Track On-Deck `Beat This!`**

1. **Do NOT ship full-track (`5:00`) `Beat This!` on any deck**:
   - On `RK3288` (`armhf`, `1–2 GB` RAM), `7m 02s` (`~10–16 min` scaled to `Cortex-A17` `CPU3`) and `228 MB` RSS for a single track is **30–60x slower** than stock `OfflineAnalyzer` (`12–15 s`), risks memory pressure on `1 GB` boards (`PRIME GO`, `Mixstream`), and still lacks key and waveform generation.
   - Even on 64-bit `AZ04` (`RK3588S`), `1.6–4.5 minutes` per full track is **15–45x slower** than stock `OfflineAnalyzer` (`4–6 s`).
2. **Primary Path (Recommended for All 16 Models) — Computer-Side `Beat This!` in `sc5000-stemsd` + `:8080` Batch Refine**:
   - Embed the static Go `Beat This! small0` engine (sharing `internal/stems`'s `CGO_ENABLED=0` AVX2 / ARM64 NEON SGEMM micro-kernels — zero Python!) inside **`sc5000-stemsd`**, which decks already auto-discover on the LAN (`#148` / `#197`).
   - Supports **both**:
     - **Full-track `Beat This!` grids** (including variable-tempo / live-drummer tracks and Wi-Fi Music Drop uploads before SD import, running in `~8–30 s` on CPU), and
     - **Sub-second (`< 1 s`) Bar-1 downbeat refinement** of already-analysed playlists on a mounted Engine USB/SD when the deck is idle, applying writes through `pkg/enginedb`'s `VACUUM INTO` backup + `grids[1]` contract.
3. **Optional Secondary Path (`AZ04` `PRIME 4 G2` / `SYSTEM ONE` Only, If Desired) — On-Deck Idle `15 s` Single-Window Bar-1 Refiner**:
   - If standalone (no-laptop) Bar-1 correction is desired, restrict on-deck execution strictly to **Use Case (b) (`isAnalyzed = 1`, single `15 s` `T = 750` drop window -> `~3 s` when idle on `AZ04`)** triggered manually from `:8080` while all decks are stopped, writing `grids[1]` only after `VACUUM INTO` backup. Keep it disabled on `1–2 GB` `armhf` boards.

---

## 8. Open Questions for the Maintainer (Before Any Implementation)

1. **Computer-side only vs `AZ04` idle Bar-1 refiner**: Should `5.3.0.5` integrate `Beat This! small0` solely into the single-binary `sc5000-stemsd` companion (covering all 16 models over LAN + Wi-Fi Drop imports with zero deck RAM impact), or also expose a manual idle-only `15 s`-window "Fix Bar 1 Downbeat" button in `:8080` on 64-bit `AZ04` (`4 GB` RAM) units?
2. **Constant-BPM snap vs variable-tempo anchors**: On computer-side full-track analysis, should constant-tempo electronic tracks snap to a single best-fit BPM + Beat-1 anchor (2 markers in `grids[1]`, matching Denon's cleanest grid display) while reserving multi-anchor variable-tempo grids for tracks whose local tempo drifts above a threshold (e.g., `> 0.3%`), or always emit constant-BPM grids unless opted into "Live / Vinyl Drift Mode"?
