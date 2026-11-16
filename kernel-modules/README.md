# Engine OS 5.3.0+ filesystem module & HFS+ tool build

Target: Gen-1 AZ01 units (SC5000 / PRIME 4 / RK3288, ARMv7), Linux
**6.18.57-az01** (`CONFIG_HFSPLUS_FS=y` built into the stock kernel). This
directory builds out-of-tree `hfsplusx.ko` (fixing 32-bit `HIGHMEM`
`kunmap_local` / same-page `hfs_bnode_move` aliasing bugs and registering
filesystem type `hfsplusx` alongside built-in `hfsplus`), optional `apfs.ko`
when `APFS_SRC` is present, and static ARMv7 `fsck.hfsplus`. No kernel
binaries are committed and nothing is installed on the build host.

## Required target inputs

Obtain the **actual firmware kernel sources with the vendor and PREEMPT_RT
patches**, the target `.config`, and the corresponding full-build
`Module.symvers`. Matching only `uname -r` is insufficient; `modules_prepare`
does not create the symbol CRC table. The build refuses missing ABI inputs, a
non-RT config, a non-`6.18.57-az01` release, a non-ARM compiler, and non-ARM
artifacts.

`fetch` downloads upstream `v6.18.57` (plus optional `KERNEL_PATCH_DIR` vendor/RT
patches in lexical order on first fetch), pinned `linux-apfs-rw`, and Debian
`hfsprogs` (`540.1.linux3-6`). Supply an already-patched `KERNEL_SRC` when
building against vendor/RT trees; `build_modules.sh` preserves the target
`.config` (including `CONFIG_HFSPLUS_FS=y`) without altering built-in options.
If the target config extracted from `/proc/config.gz` leaves
`CONFIG_LOCALVERSION=""` while `uname -r` carries the `-az01-...` suffix, set
matching `CONFIG_LOCALVERSION` or pass `LOCALVERSION` in the environment.

The target config must provide modules, NLS and OverlayFS. If module signatures
are enforced, sign the resulting modules with the firmware's trusted key before
packaging. Never force-load ABI mismatches.

## Patches (`patches/`)

- `hfsplusx-01-highmem-kmap-fix.patch`: fixes 32-bit `HIGHMEM` `kunmap_local`
  offset mismatches in `bnode.c`, same-page double-`kmap_local_page()` `memmove`
  aliasing in `hfs_bnode_move()`, and the early-exit `kmap_local` leak in
  `bitmap.c`.
- `hfsplusx-02-standalone-module.patch`: builds standalone out-of-tree
  `hfsplusx.ko` with filesystem name `hfsplusx` and independent slab caches
  (`hfsplusx_icache`, `hfsplusx_attr_cache`) so it coexists with built-in
  `hfsplus`.
- `hfsprogs-cross-ar.patch`: passes `$(AR)` when cross-compiling `libdfa.a`.

## Build

Host dependencies: Bash, Git, curl, GNU make, patch, tar/xz, coreutils, kmod,
ARM Linux GCC/binutils, and standard kernel build tools (`flex`, `bison`, `bc`,
`libelf`, OpenSSL host headers). Compiler prefix defaults to
`arm-linux-gnueabihf-`.

Run from this worktree on a build host:

```bash
set -o pipefail
TARGET_CONFIG=/absolute/path/to/engine.config \
TARGET_SYMVERS=/absolute/path/to/Module.symvers \
KERNEL_SRC=/absolute/path/to/vendor-linux-6.18.57-rt \
./kernel-modules/build_modules.sh modules > kernel-modules/build.log 2>&1
./kernel-modules/build_modules.sh tools >> kernel-modules/build.log 2>&1
tail -40 kernel-modules/build.log | cut -c1-200 | head -40
```

`fetch`, `modules` and `tools` can run independently (`all` runs `fetch`,
`modules`, and `tools`). `tools` builds only `fsck_hfs.tproj` and defaults to
`-static -Wl,--build-id`, staging static ARMv7 `fsck.hfsplus` without requiring
`HFS_SYSROOT` (`-idirafter` provides an empty `<uuid/uuid.h>` stub only if no
sysroot header exists). `mount.hfsplus` delegates to util-linux
`mount -i -t hfsplus`. HFS tools retain their upstream Apple license; consult
the downloaded source's license before redistribution.

Outputs under `out/`:

- `lib/modules/<release>/extra/hfsplusx.ko` (plus `apfs.ko` if `APFS_SRC` is present)
- `sbin/{fsck.hfsplus,mount.hfsplus}`
- `modules-build-manifest.txt` and `tools-build-manifest.txt` with input/patch/artifact hashes

## Release builds

`tools/az01-packer/build-release.sh` builds the module itself through
`build-release-module.sh <stock Gen-1 rootfs.ext2> <out.ko>`: pinned
`linux-6.18.57.tar.xz` and `patch-6.18.57-rt8.patch.xz` (sha256-checked), the
stock kernel's own config (`extract-ikconfig` on its `/boot/zImage-<release>`,
`CONFIG_LOCALVERSION` set to the release suffix), GCC 13, `vmlinux.symvers` from
a full `vmlinux` build, then `build_modules.sh modules`. The result is cached by
the sha256 of every input in `$HFSPLUSX_CACHE` (default
`~/.cache/enginex-hfsplusx`); set `HFSPLUSX_KO` to use a module built elsewhere.

## QEMU `HIGHMEM` stress harness

`test_hfsplusx_qemu.sh` boots an ARMv7 `virt` guest with 2 GiB RAM (`HighMem`
active) and `PANIC_ON_OOPS=y` against 8 KiB Catalog B-tree images to verify both
that stock built-in `hfsplus` reproduces the `HIGHMEM` `kunmap_local_indexed` /
`BUG` and same-page `hfs_bnode_move` corruption, and that `hfsplusx.ko` survives
6 rounds (4338 `mkdir`/create/`rename`/`unlink`/`rmdir` ops) with exit-0
`fsck.hfsplus -n` inside the guest and on the host:

```bash
QEMU_ZIMAGE=/path/to/out-qemu/arch/arm/boot/zImage \
HFSPLUSX_KO=/path/to/hfsplusx.ko \
FSCK_HFSPLUS=/path/to/static/fsck.hfsplus \
./kernel-modules/test_hfsplusx_qemu.sh
```

It also accepts `KERNEL_SRC` + `QEMU_KERNEL_BUILD` (to rebuild `hfsplusx.ko`
from `patches/`) or `KERNEL_SRC` + `TARGET_CONFIG` (to build the QEMU kernel and
`hfsplusx.ko` from scratch).

## Verification status

`tests/test_rootfs_overlay.sh` exercises script syntax and missing-ABI-input
refusal. `test_hfsplusx_qemu.sh` verifies `hfsplusx.ko` under real 32-bit ARM
`HIGHMEM` in QEMU. Physical-device validation on target hardware is still
required to confirm module loading against the signed/stock vendor kernel.
