# Issue #177: SC LIVE 4 Waveform Lag & 2nd-Gen Data-Bundle Performance Parity

## Symptom

On 2nd-generation units running via `/data` bundle (`SC LIVE 4` `JP21`, etc.), scrolling waveforms felt less responsive than on 1st-generation `.img` builds.

## Root Cause

Comparing 1st-gen `rootfs-overlay` (`engine.service.d/override.conf`, `stream-sync.service`, `rb-bridge.service`, `sc5000-deemixd.service`, `sc5000-perf-tune`, `sc5000-debloat`) against 2nd-gen `BOOT_ACTIVATE` in `tools/az01-packer/build-data-bundle.py` revealed three gaps:

1. **Missing RAM-backed `HOME` & shader cache config**: `engine.service.d/99-sc5000.conf` did not set `HOME=/tmp/sc5000-engine-home`, `MESA_SHADER_CACHE_MAX_SIZE=32M`, `QML_DISABLE_DISK_CACHE=1`, or `ExecStartPre=-/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home`, causing Mesa GL shader cache writes to fall back to the read-only rootfs.
2. **Unpinned Go background services**: `enginex-rb-bridge.service` and `enginex-sc5000-deemixd.service` (as well as `enginex-stream-sync.service` unit config) lacked `CPUAffinity=3`, allowing background Go GC/goroutines to preempt CPU 0/1/2 (audio, `EMain` / Mali GPU, USB/Ethernet).
3. **Missing runtime IRQ isolation & cpufreq floor**: Stock `/usr/Engine/Scripts/setup-prerequisites.sh` runs inside `runengine` (`engine.service`) and pins `eth0`, `ehci_hcd`, and `ff540000` IRQs onto CPU 1 (sharing the Mali GPU `ffa30000` core), pins `ffb20000` audio IRQ onto CPU 3 (the background daemon core), and leaves `scaling_min_freq` at 408 MHz. Because 2nd-gen `/` is read-only, `setup-prerequisites.sh` cannot be edited on disk.

## Fix

In `tools/az01-packer/build-data-bundle.py` (`BOOT_ACTIVATE` and `UNINSTALL`):

- Updated `/run/systemd/system/engine.service.d/99-sc5000.conf` to set:
  - `Environment=HOME=/tmp/sc5000-engine-home MESA_SHADER_CACHE_MAX_SIZE=32M`
  - `Environment=QML_DISABLE_DISK_CACHE=1`
  - `ExecStartPre=-/bin/mkdir -p -m 0700 /tmp/sc5000-engine-home`
  - `ExecStartPost=-/bin/sh /run/sc5000/perf-tune.sh`
- Added `CPUAffinity=3` to `enginex-stream-sync.service`, `enginex-rb-bridge.service`, and `enginex-sc5000-deemixd.service`.
- Added `/run/sc5000/perf-tune.sh`, executed both in `boot-activate.sh` before service restarts and in `engine.service` `ExecStartPost` after stock `setup-prerequisites.sh` finishes:
  - Sets `cpu0..cpu3` `scaling_governor=ondemand`, `scaling_min_freq=1200000`, `ondemand/up_threshold=50`, `ondemand/sampling_down_factor=10`, `ondemand/ignore_nice_load=1` (best-effort `[ -w ... ]` + `|| :`).
  - Reads `/proc/interrupts` (if readable) and routes `ffb20000:0 ffa30000:1 ehci_hcd:2 ff540000:2 eth0:2` via `smp_affinity_list` (or `smp_affinity`), pins `irq/<irq>-*` kernel threads with `taskset -pc`, and sets `chrt -f -p 99` on `ffb20000` IRQ threads.
- Updated `uninstall.sh` to remove `/run/sc5000/perf-tune.sh`.

In `tools/az01-packer/test_data_bundle.py`:

- Added assertions checking `HOME`, `MESA_SHADER_CACHE_MAX_SIZE=32M`, `QML_DISABLE_DISK_CACHE=1`, `ExecStartPre`, `ExecStartPost`, `CPUAffinity=3` on all 3 generated units, and runtime tuning in `boot-activate.sh` + cleanup in `uninstall.sh`.
- Added `test_boot_activate_perf_tune_runtime` executing the extracted `perf-tune.sh` against mocked `SC5000_SYS_ROOT` and `SC5000_PROC_ROOT` trees and verifying cpufreq knobs, IRQ masks, `taskset`, and `chrt` invocations.

## Verification

- `python3 -m unittest discover -s tools/az01-packer` (113 tests, 1 skipped on host `/data` check; all 112 active tests including `test_boot_activate_perf_tune_runtime` passed).
- `bash tools/az01-packer/test_sclive4_bootstrap.sh` (both `SCLIVE4` and `AZ04=1` `PRIME4G2`): all 5 stages passed in read-only rootfs `unshare` chroots.
- Note: actual live hardware frame-pacing / CPU temperature on a physical `SC LIVE 4` was not measured in the sandbox.
