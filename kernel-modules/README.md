# Engine OS 5.0.4 filesystem module build

Target: SC5000 / JP07 / RK3288, ARMv7, Linux **6.6.119-rt**. This directory
contains a cross-build and staging tool; no kernel binaries are committed.

## Required target inputs

Obtain the **actual firmware kernel sources with the vendor and PREEMPT_RT
patches**, the original `.config`, and the corresponding full-build
`Module.symvers`. Matching only `uname -r` is insufficient. `modules_prepare`
does not create the symbol CRC table. The build refuses missing ABI inputs, a
non-RT config, a different release, a non-ARM compiler, and non-ARM artifacts.

`fetch` can download the upstream `v6.6.119` base. This is **not** a substitute
for the Engine kernel: supply an already patched `KERNEL_SRC`, or supply a
`KERNEL_PATCH_DIR` containing the vendor/RT patches in lexical order on the
first fetch. Failed/incomplete source directories require inspection before
retry; the script does not remove user-provided source trees.

The target config must provide modules, NLS and OverlayFS. Existing target
NLS/CRC/other dependent modules must be staged with the firmware too. The
builder selects `CONFIG_HFSPLUS_FS=m`; it preserves other target selections.
If module signatures are enforced, sign the resulting modules with the
firmware's trusted key before packaging. Never force-load ABI mismatches.

## Build

Host dependencies: Bash, Git, curl, GNU make, patch, tar/xz, coreutils, kmod,
ARM Linux GCC/binutils, and the kernel's normal build dependencies (host C
compiler, flex, bison, bc, libelf and OpenSSL development headers). Use the
firmware's matching cross-toolchain and userspace sysroot; compiler prefix
defaults to `arm-linux-gnueabihf-`.

Run from this worktree on build machine IN:

```bash
set -o pipefail
TARGET_CONFIG=/absolute/path/to/engine.config \
TARGET_SYMVERS=/absolute/path/to/Module.symvers \
KERNEL_SRC=/absolute/path/to/vendor-linux-6.6.119-rt \
CROSS_COMPILE=/absolute/path/to/toolchain/bin/arm-linux-gnueabihf- \
HFS_SYSROOT=/absolute/path/to/engine-sysroot \
./kernel-modules/build_modules.sh all > kernel-modules/build.log 2>&1
tail -40 kernel-modules/build.log | cut -c1-200 | head -40
```

`fetch`, `modules` and `tools` can be run independently. `tools` fetches only
hfsprogs when necessary. `modules` never fetches. `BUILD_ROOT`, `OUTPUT_ROOT`,
`KERNEL_BUILD`, `JOBS`, `APFS_SRC`, `APFS_REF`, `HFS_TOOLS_SRC`, `HFS_CFLAGS` and
`HFS_LDFLAGS` are configurable. `EXPECTED_RELEASE` must equal the complete
target release if the firmware includes an additional RT suffix. Use absolute
paths. Defaults keep all build products in this worktree's `kernel-modules/`.

The APFS source is pinned to commit
`cb9c0a0681923eaa090026b4c9357e139dfb355b` of
<https://github.com/linux-apfs/linux-apfs-rw>. `APFS_CONFIG` is forced empty;
default-write mode is never enabled. The storage worker always requests `ro`.

HFS tools use Debian's `540.1.linux3` upstream archive and `-6` patch set,
both verified by fixed SHA-256 digests. These patches remove clang Blocks and
fix ARM stdarg/current glibc builds; a local patch selects the cross archiver.
The checker is installed as `fsck.hfsplus`. `mount.hfsplus` delegates to
util-linux `mount -i -t hfsplus`, without helper recursion. Automatic RW mounts
must go through the storage worker; invoking the helper directly does not run
fsck. HFS tools retain their upstream
Apple license; consult the downloaded source's license before redistribution.

Outputs under `out/`:

- `lib/modules/<release>/extra/{hfsplus,apfs}.ko`
- `sbin/{fsck.hfsplus,mount.hfsplus}`
- source/config/symbol/artifact hashes in build manifests

Merge `out/` into the **unpacked firmware staging root**, preserving any
merged-usr `/sbin` layout. Run `depmod -b <staging-root> <release>` during image
packaging, and include the checker's target libc/loader dependencies. Do not
copy artifacts into the build host's `/lib/modules` or `/sbin`. No installer,
host service mutation, push, or device flashing is performed by this script.

## Verification status

The repository tests exercise script syntax, storage policy and hardware
behavior through mocks. Real module compilation/loading requires the target
ABI inputs and toolchain above. QEngine and physical-device validation must
verify module vermagic/dependencies, SD filesystem xattrs, APFS volume support,
and reads/writes of a disposable test library before image release. The APFS
driver chooses its default volume; encrypted/multi-volume APFS support is not
implemented by the worker.
