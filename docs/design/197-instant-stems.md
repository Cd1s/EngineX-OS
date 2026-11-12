# Design Note: Instant Stems & Python-Free Computer Service (#197)

## 1. Problem

Today EngineX OS has two Stems separation paths:

1. **On-deck fallback (`internal/stems/local.go`)**: a causal second-order IIR frequency split (`<250 Hz` bass, `250–4000 Hz` mid vocals, `>4 kHz` drums, remainder melody). It uses negligible RAM and runs at ~1.4x real time on 32-bit `armhf` decks, but provides only band filtering rather than semantic instrument separation.
2. **Computer-side LAN / relay renderer (`internal/stemsd`, `tools/stems/worker.py`)**: spawns a Python subprocess per render session (`htdemucs` via PyTorch/Demucs when installed, else NumPy/SciPy `hpss`). Users must install multi-gigabyte Python/PyTorch dependencies, cold process imports add startup latency on every track, and CPU-only `htdemucs` takes 90–300 s per 4-minute track (~35 s with a mid-range GPU).

Goal (#197): make neural stems usable within seconds on the computer-assisted path without requiring Python, evaluate whether a fast Spleeter-class neural separator can run directly on 64-bit (`PRIME 4 G2` / `SYSTEM ONE`) or 32-bit `armhf` decks within a usable time, and preserve high-quality `htdemucs` separation via background caching while keeping the existing Python `htdemucs` path working.

## 2. Stock Capability Check

- **Whole-track encrypted `.stems` container**: On all 14 `armhf` models (and as fallback on 64-bit AZ04 when no factory processor is installed), stock Engine's `RenderStemsJob` / `StemsAudioProcessor` spawns `stems-processor -s -hv 1.0.0 -pv <ver>`, streams decoded float32 PCM sequentially from frame `0` to `N-1` (`action=block`), sends `action=done`, reads back `4 * numChannels` planar float32 stems (`returnFrames`), encrypts `Engine Library/Stems/<trackId> <uuid>.stems` itself, and transitions `GET_STEMS_STATUS` from `2` (processing) to `3` (completed) only after the child exits `0`.
- **No partial `.stems` playback in stock Engine**: Stock Engine neither reads a partially written `.stems` container nor hot-reloads an open `.stems` container mid-playback without re-rendering or reloading the track, and EngineX never forges encrypted `.stems` containers externally.
- **Factory AZ04 Stems priority**: On 64-bit AZ04 units (`PRIME 4 G2` `JP24`, `Rane SYSTEM ONE` `RMZ2`), stock Engine supports `DownloadStemsProcessorJob` (`stems-processor.tar.zst` in `/data`). EngineX's `statx`/`open`/`access` fallback already preserves priority for any installed executable stock processor (`#176`, `#197`).
- **Zero-deck-change LAN protocol**: `stems-processor` on the deck already speaks `POST /v1/engine` (`X-Stems-Protocol: engine-5.0.4`, `X-Stems-Framing: 2` packed int16 upload + 3-stem int16 download with exact `melody = input - vocals - bass - drums` reconstruction on the deck) and discovers `enginex-stemsd` via UDP `17891` and mDNS `_enginex-stems._tcp.local.`. Any speedup or caching inside `enginex-stemsd` works immediately with unmodified decks.

## 3. On-Deck ARM Feasibility Benchmark & Scaling (`(b)`)

### 3.1 Workload of Spleeter 4-Stem U-Net (MIT, `deezer/spleeter`)

- Architecture: 4 independent U-Nets (`vocals`, `drums`, `bass`, `other`) operating on `44.1 kHz` stereo STFT magnitude windows (`frame_length=4096`, `frame_step=1024`, `T=512` frames = `11.89 s`, `F=1024` bins). Each U-Net has 6 stride-2 `5x5` encoder convolutions (`2->16->32->64->128->256->512`), 6 stride-2 `5x5` transposed decoder convolutions with skip concatenations, and a dilated `4x4` output convolution, followed by `L2` ratio masking (`e^2 / sum(e^2)`).
- Arithmetic cost: `6.10B MACs` (`12.2 GFLOPs`) per stem per `11.89 s` window -> **`24.4B MACs` (`48.8 GFLOPs`) across 4 stems per `11.89 s` window** (`4.10 GFLOPs` per second of stereo audio; **`984 GFLOPs`** for a 4-minute / `240 s` track, ~20.2 windows).
- Weights & working memory: `4 x 9.82M` float32 parameters (`157 MB` fp32, `78.5 MB` fp16) plus `~150–210 MB` for STFT buffers, skip activations (`conv1..conv5`), and `im2col` strips -> **`~310–375 MB` RSS**.

### 3.2 Measured ARM & x86-64 Benchmarks

Measured on an idle 4-core ARMv8.2-A build host (`Neoverse-N1` @ ~3.0 GHz, native 64-bit `aarch64` and native 32-bit `armhf` EL0) and an 8-core / 16-thread `x86-64` build host (`AVX2+FMA`):

- **64-bit `aarch64` (`CGO_ENABLED=0` Go with AArch64 NEON `FMLA.4S` micro-kernel)**:
  - 1 thread: `3.26 s` per `11.89 s` window (`65.7 s` per 4-min track, `3.7x` real time, `14.9 GFLOP/s`)
  - 2 threads: `1.63 s` per `11.89 s` window (`32.8 s` per 4-min track, `7.3x` real time, `29.9 GFLOP/s`)
  - 4 threads: `0.81 s` per `11.89 s` window (`16.4 s` per 4-min track, `14.7x` real time, `60.1 GFLOP/s`)
  - Reference C (`gcc -O3 -ffast-math -mcpu=native -static`): `2.65 s` (1T), `1.35 s` (2T), `0.86 s` (4T, `56.7 GFLOP/s`).
  - Scalar Go (`CGO_ENABLED=0`, no SIMD): `11.38 s` (1T), `5.99 s` (2T), `3.02 s` (4T, `61.1 s` per 4-min track, `372–573 MB` Sys).
- **32-bit `armhf` EL0 (`armv7-a`, `neon-vfpv4`)**:
  - NEON C (`arm-linux-gnueabihf-gcc -O3 -ffast-math -mfpu=neon-vfpv4 -static`): `4.22 s` (1T, `11.5 GFLOP/s`, `85.1 s` per 4-min track), `2.28 s` (2T, `46.0 s`), `1.15 s` (4T, `23.2 s`, `42.4 GFLOP/s`).
  - Scalar Go (`GOARCH=arm GOARM=7`): `12.52 s` (1T, `252.7 s` per 4-min track), `3.48 s` (4T, `70.2 s` per 4-min track).
- **Computer `x86-64` (`CGO_ENABLED=0` Go with `AVX2+FMA` micro-kernel)**:
  - 1 thread: `1.03 s` per `11.89 s` window (`20.8 s` per 4-min track, `11.5x` real time, `47.1 GFLOP/s`)
  - 4 threads: `0.26 s` per `11.89 s` window (`5.2 s` per 4-min track, `46.1x` real time, `188.7 GFLOP/s`)
  - 8 threads: `0.23 s` per `11.89 s` window (`4.7 s` per 4-min track, `50.8x` real time, `207.8 GFLOP/s`)
  - 16 threads: `0.18 s` per `11.89 s` window (`3.6 s` per 4-min track, `65.9x` real time, `269.6 GFLOP/s`)

### 3.3 Scaling Assumptions to Real Deck Hardware

1. **14 32-bit `armhf` models (Rockchip RK3288: 4x Cortex-A17 @ 1.6–1.8 GHz, 2 GB DDR3)**:
   - Per-core scaling vs 3.0 GHz Neoverse-N1: clock ratio `1.6 / 3.0 = 0.53x` and single-pipe ARMv7 NEON + DDR3 bandwidth (`~0.55x` IPC) -> `~0.29x` throughput (`~3.4x` slower per core).
   - Real-time audio protection (`pkg/resourceguard`): background separation on RK3288 must stay pinned to a single isolated core (`CPU3`, `SCHED_IDLE`, `80%` duty cycle = `1.25x` wall-time factor) so `CPU0` (96 kHz ALSA `SCHED_RR 49`) and `CPU1–2` (Qt/QML 60 fps UI) never drop frames or glitch.
   - Resulting estimate: `85.1 s * 3.4 * 1.25 ≈ 362 s` (~6 minutes, `0.66x` real time) for a 4-minute track, plus `~310 MB` RSS against `<700 MB` free RAM on a 2 GB system. **Decision: not feasible on `armhf` decks.**
2. **64-bit AZ04 models (`PRIME 4 G2` `JP24` / `SYSTEM ONE` `RMZ2`, Rockchip RK3588 / RK3588S: 4x Cortex-A76 @ 2.2–2.4 GHz + 4x Cortex-A55 @ 1.8 GHz)**:
   - Cortex-A76 shares the dual-128-bit ASIMD FMA execution width of Neoverse-N1 (`3.0 / 2.3 = 1.30x` clock ratio; adding `~15%` for STFT, `im2col`, `ELU`, and BatchNorm overhead).
   - With 2 spare Cortex-A76 cores (`SCHED_IDLE`, reserving 2 A76 + 4 A55 cores for Engine audio, UI, and decoding), a 4-minute track takes `32.8 s * 1.30 * 1.15 ≈ 49 s` (~`41 s` with tight C packing).
   - Because stock Engine blocks Stems pad activation until the entire `.stems` file is written (`~41–49 s` wait, not seconds after load) and AZ04 models keep stock factory Stems (`#197`), CPU-only Spleeter on AZ04 does not achieve instant stems. **Decision: deliver `(a)` (the Python-free computer-side service) first.**

## 4. Options for `(a)` Computer-Side Service Without Python

1. **ONNX Runtime (`ort` C++ shared library)**:
   - Pros: mature CPU/CoreML/DirectML execution providers.
   - Cons: requires shipping 25–50 MB OS-specific C++ shared libraries (`.so`/`.dylib`/`.dll`) and Cgo cross-compilers for Linux, macOS, and Windows, breaking the single-file `CGO_ENABLED=0` `build-installer.sh` pipeline.
2. **Separate Rust binary (`burn` + `wgpu`/`ndarray`)**:
   - Pros: GPU acceleration via Vulkan/Metal; `demucs-rs` (Apache-2.0) exists.
   - Cons: requires adding a Rust + cross-SDK toolchain (`macos` Metal frameworks, `windows` MSVC/MinGW) to the build host and maintaining a second daemon alongside `enginex-stemsd`.
3. **Chosen — Single `CGO_ENABLED=0` Go Static Binary (`enginex-stemsd`) with `amd64` AVX2/FMA & `arm64` NEON SIMD (`internal/stems`, `internal/stemsd`)**:
   - Compiles via the existing `CGO_ENABLED=0 GOOS=... GOARCH=... go build ./cmd/enginex-stemsd` in `tools/az01-packer/build-installer.sh` into one standalone binary per OS (`linux-amd64`, `macos-amd64`, `macos-arm64`, `windows-amd64`).
   - Runs Spleeter 4-stem U-Net in `~4–6 s` per 4-minute song on 4–8 desktop CPU threads (`46–66x` real time) and HTDemucs v4 in the background on CPU, with zero Python/PyTorch installation.

## 5. Chosen Design

### 5.1 Upstream Licences (Verified)

- **Spleeter 4-stem architecture & weights**: `github.com/deezer/spleeter` (`v1.4.0`), **MIT License**. Official `4stems.tar.gz` SHA-256: `3adb4a50ad4eb18c7c4d65fcf4cf2367a07d48408a5eb7d03cd20067429dfaa8`.
- **HTDemucs v4 architecture & weights**: `github.com/facebookresearch/demucs`, **MIT License** (`htdemucs` signature `955717e8`; safetensors conversion SHA-256 `8193504cdfb3943adaf039b8acb524a46e87ebf232c383ac7a32c80a6578423e`).
- **openDJ (`GPL-2.0+`)**: studied for high-level two-tier architecture only; zero code copied. All EngineX implementation is clean-room Go under EngineX's MIT licence.

### 5.2 In-Process Separators & Wire Compatibility (`internal/stems`)

`stems.Backend` gains an optional in-process `Render func(ctx context.Context, in io.Reader, out io.Writer) error` alongside the existing `Worker []string` subprocess path. Both paths share identical `/healthz`, `POST /v1/engine`, `X-Stems-Protocol: engine-5.0.4`, `X-Stems-Framing: 1|2` (`int16` packed upload + 3-stem `int16` download), UDP `17891` beacon, and mDNS `_enginex-stems._tcp.local.` handling, so **decks require zero changes**.

Four in-process Go backends are provided:

1. **`native-hpss`**: pure-Go causal streaming WOLA separator (`2048`-pt Hann, `512` hop, 17-frame temporal median harmonic filter, 31-bin frequency median percussive filter, mid/side vocal mask, exact `melody = input - vocals - bass - drums` remainder). Requires zero external model files and works offline immediately.
2. **`spleeter` (Instant Neural Pass)**: clean-room Go implementation of Deezer's 4-stem U-Net (`4096`/`1024` periodic Hann STFT/iSTFT with `2/3` window compensation, polyphase windowed-sinc resampling when Engine sends non-`44.1 kHz` audio, 6 encoder `Conv2D(5x5, s=2)` + `BatchNorm` + `ELU`, 6 decoder `Conv2DTranspose(5x5, s=2)` + `ELU` + `BatchNorm` + pre-BN skip concatenation, dilated `Conv2D(4x4, d=2)` + `Sigmoid`, `L2` ratio mask, and exact `melody = input - vocals - bass - drums` reconstruction). Loads either `spleeter-4stems.safetensors` or Deezer's official `4stems.tar.gz` (`model.index` SSTable + `model.data-00000-of-00001`) directly in Go without Python or TensorFlow, keeping weights warm in memory across sessions.
3. **`native-htdemucs` (High-Quality Neural Pass)**: clean-room Go implementation of Meta's 4-stem HTDemucs v4 (`4096`/`1024` reflect-padded normalized STFT/iSTFT, complex-as-channels `CaC`, 4-stage `HEnc`/`TEnc` with `DConv` + `GroupNorm` + `GLU` + `LayerScale`, frequency embedding, 5-layer `CrossDomainTransformer` with sinusoidal 1D/2D positional encodings, and 4-stage `HDec`/`TDec`, applied over overlapping `7.8 s` windows at `44.1 kHz`). Loads `htdemucs.safetensors` (`F16`/`F32`) directly in Go.
4. **`tiered` (`spleeter+htdemucs`, Two-Pass Instant + Background HQ Cache)**:
   - Maintains a bounded SHA-256 content-addressed disk cache (`-cache-dir`, `-cache-max-mb`, default `2048 MB`) storing 3-stem `int16` planes (`vocals`, `bass`, `drums`); `melody` is reconstructed as `input - vocals - bass - drums` so output frames always sum to Engine's exact input.
   - **Cache Hit**: when a track's canonical audio digest (`sampleRate`, `channels`, `frames`, `int16`-quantized mix) is present in the HQ cache, `POST /v1/engine` serves the cached HTDemucs stems immediately (`<1 s`).
   - **Cache Miss**: `POST /v1/engine` runs the in-process **Spleeter** instant pass and returns stems within seconds, while enqueueing the captured mix onto a single-worker background **HTDemucs** queue (using `native-htdemucs` when weights are present, or optional Python `worker.py --backend htdemucs` when configured) that atomically writes the HQ cache entry upon completion.
   - Note on stock Engine `.stems` replacement: because stock Engine writes and encrypts `<trackId> <uuid>.stems` once per `RenderStemsJob`, a second render of that track (or any second deck / library re-stem of the same audio) receives the cached HTDemucs stems immediately over the unchanged wire protocol.

### 5.3 Preserving Existing Python `htdemucs` Path

- Existing flags (`-worker`, `-python`, `-backend htdemucs|demucs|hpss|auto`), `defaultBackend() == "htdemucs"`, `resolveWorker`, and `tools/stems/worker.py` remain intact and pass all existing Go and Python unit tests.
- Native Python-free execution activates when `-backend` is `tiered`, `spleeter`, `native-htdemucs`, or `native-hpss`, when `-native` is set, or automatically in zero-config standalone mode when `worker.py` / `python3` is not present on the host.
- Model weight downloads (`-download-models` one-shot command or `-auto-download-models`) verify pinned SHA-256 digests before atomic rename and are never triggered implicitly during unit tests.

## 6. Risks & Mitigations

- **CPU instruction portability (`amd64`)**: `AVX2+FMA` assembly (`VFMADD231PS`) would fault (`SIGILL`) on older CPUs lacking AVX2. Guard dispatch at init via `golang.org/x/sys/cpu` (`cpu.X86.HasAVX2 && cpu.X86.HasFMA`), falling back automatically to pure Go. On `arm64`, baseline ASIMD (`FMLA.4S`) is mandatory in ARMv8-A.
- **Memory pressure during background HQ pass**: bound background HTDemucs queue depth (`1` active worker, bounded backlog of `4` tracks) and reuse `im2col` strip buffers so RSS stays bounded regardless of session count.
- **Corrupted downloads or partial cache files**: verify full SHA-256 before promoting model archives, write HQ cache files via `.tmp` + `os.Rename`, validate frame count and header magic on cache read, and evict oldest entries by mtime when exceeding `-cache-max-mb`.
- **Sample-rate mismatch (44.1 kHz vs 48/96 kHz)**: resample input to `44.1 kHz` for neural inference, resample the 3 predicted stems (`vocals`, `bass`, `drums`) back to the exact input frame count `N`, and compute `melody = input - vocals - bass - drums` in the native session sample rate so `FramesOut == FramesIn` and sum-to-input hold identically at every sample rate.

## 7. Test Plan

1. **Existing test suites (must stay 100% green)**:
   - `go test -race ./internal/stems/... ./internal/stemsd/... ./internal/stemsproc/... ./cmd/sc5000-relay/...`
   - `python3 -m unittest discover -s tools/stems`
2. **Protocol & DSP parity tests (`internal/stems`)**:
   - Port all `tools/stems/test_worker.py` invariants (mono/stereo, odd lengths `0..12347`, arbitrary `maxBlockSize` `1, 511, 513, 1000, 1024`, batching invariance, stray blank lines, unbounded header rejection, exact `sum(stems) == input` within `2e-5`) to test `native-hpss`, `spleeter`, `native-htdemucs`, and `tiered`.
   - Verify SIMD vs scalar `sgemm` numerical equivalence across misaligned/tail matrix dimensions (`N` not divisible by 4 or 8).
3. **Model & loader unit tests**:
   - Synthetic TF `TensorBundle` (`.index` SSTable + `.data-00000-of-00001`) and `safetensors` (`F16` + `F32`) roundtrip tests, SHA-256 verification failure rejection, Spleeter U-Net forward pass at `44.1 kHz` and `48 kHz`, and HTDemucs v4 forward pass + chunk overlap-add alignment.
4. **Tiered Instant -> Background HQ Cache integration test**:
   - Full `stems.Proxy` (`X-Stems-Framing: 2` packed int16) against `stems.Backend`: verify 1st session returns instant stems immediately and schedules background HQ; wait for background HQ completion; verify 2nd session hits the HQ cache and returns the HQ stems over the identical wire protocol; verify LRU cache eviction.
5. **Real-weight smoke & cross-OS static build verification on remote build hosts**:
   - Run `spleeter` and `native-htdemucs` against real upstream MIT weights (`4stems.tar.gz` and `htdemucs.safetensors`) on a multi-component musical test signal and record separation energy, exact frame accounting, and wall time.
   - Cross-compile `CGO_ENABLED=0` `enginex-stemsd` for `linux/amd64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, `linux/arm64`, and `linux/arm` (`GOARM=7`).
