# Issue #176: Stems on SC LIVE 4 and Read-Only 2nd-Gen Data Bundles

## Symptom

On 2nd-generation units running via `/data` bundle (`SC LIVE 4` `JP21`, `SC LIVE 2`, `PRIME 4+`, `PRIME GO+`, `Mixstream Pro+`, `Mixstream Pro Go`), Stems showed as not installed (`"Add"` instead of `"Remove"`) and `stems-processor` was never spawned even when `/data/enginex/bundle/bin/stems-processor` (linked via `/data/sc5000-custom/bundle/bin/stems-processor`) was present and executable.

## Root Cause

In Engine `5.3.0-sclive4`, `StemsProcessorManager::checkProcessorInstalled()` (`0x8c0374`) and `StemsProcessorManager::isProcessorInstalled()` (`0x8c0d5c`) call `vtable[+0x48]` (`0x8ba000`), which the in-memory `edits_sclive` table patches to return `QString("/usr/local/bin/stems-processor")`. Both methods then construct `QFile("/usr/local/bin/stems-processor")` (`0x8c04ac`, `0x8c0e1c`) and call `QFile::exists()` (`0x8c04b4`, `0x8c0e24`).

In `libQt6Core.so.6.7.2`, `QFile::exists()` calls `statx@plt` (`0x2bded8`, `dirfd = AT_FDCWD`, `flags = AT_NO_AUTOMOUNT`). Previously, `tools/ui-mod/engine_ui_hook.c` only applied `stems_processor_path(path)` in `access()`, `posix_spawn()`, `execve()`, and `execv()`, leaving `statx()` and `redirected_open()` (`open`/`open64`/`openat`/`openat64`) unredirected. Because the 2nd-gen root filesystem is mounted read-only without `/usr/local/bin/stems-processor`, `statx(AT_FDCWD, "/usr/local/bin/stems-processor", ...)` returned `-1` (`ENOENT`) and `QFile::exists()` returned `false`.

## Fix

- `tools/ui-mod/engine_ui_hook.c`:
  - Added `statx()` hook forwarding `path` through `stems_processor_path(path)` when `is_engine()` is true.
  - Wired `stems_processor_path(path)` into `redirected_open()` before `volatile_path()` when `is_engine()` is true.
- Rebuilt `rootfs-overlay/etc/sc5000/libengine-ui-hook.so` (`tools/ui-mod/build.sh`) and copied to `rootfs-overlay/usr/local/lib/sc5000/libengine-ui-hook.so`.
- `tools/ui-mod/test_stems_feature.c` and `tools/ui-mod/test_az04_hook.c`:
  - Added assertions verifying `statx(AT_FDCWD, "/usr/local/bin/stems-processor", 0, 0xfffu, &stx) == 0` and `open("/usr/local/bin/stems-processor", O_RDONLY) >= 0` when `/data/sc5000-custom/bundle/stems-processor` or `/data/sc5000-custom/bundle/bin/stems-processor` is executable and `/usr/local/bin/stems-processor` is absent.

## Verification

- Built `libengine-ui-hook.so` via `sh tools/ui-mod/build.sh` (GLIBC <= 2.38, 4 KiB segment alignment) and verified exported `statx`, `open`, `open64`, `openat`, `openat64`, and `access` symbols.
- `test_stems_feature` compiled with `-std=c11 -O2 -Wall -Wextra -Werror` and executed natively against stock `5.0.4`, `5.3.0`, and `5.3.0-sclive4` `Engine` binaries: all checks passed.
- `test_az04_hook` compiled with `-std=c11 -O2 -Wall -Wextra -Werror` and executed natively against stock `5.3.0-az04` `Engine`: all checks passed.
- Verified inside a read-only `sclive4` rootfs chroot (`unshare -m`) using stock `libQt6Core.so.6.7.2` (`QFile::QFile` + `QFile::exists`) that `QFile("/usr/local/bin/stems-processor").exists()` returns `0` without `LD_PRELOAD` and `1` with `LD_PRELOAD=libengine-ui-hook.so`.
- `bash tools/az01-packer/test_sclive4_bootstrap.sh` (both `SCLIVE4` and `AZ04=1` `PRIME4G2`): all 5 stages passed.
