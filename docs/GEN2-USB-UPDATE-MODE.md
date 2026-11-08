# AZ04 and SCLIVE4 boot-mode investigation

Status: static analysis completed for the az01 userspace (`libaz01-prog` 32/64-bit,
`az01-update`), the recovery ramdisk `/init` and `/shutdown`, on 2026-11-07. Neither
issue #124 nor #125 is complete: the U-Boot USB-scan and filename path is still open,
and no hardware behavior has been verified. Offsets below are byte offsets in the
extracted U-Boot binary, not physical addresses in the update image. Analysis was
performed on the build host.

Official hardware key combination for Firmware Update Mode: **not recorded here**.
The repo has no power-on key-combo source (`docs/manual/02-installation-and-ota.md`
says the same), and the analysis batch contains no hardware manual. Use the on-screen
path `Settings → About / Update → Firmware Update Mode (Reboot)` until the manual's
combo is checked against a real deck.

## Inputs

* SYSTEM ONE 5.3.0 AZ04: `/root/build/sc5000-lab/uboot-re-125/s1/u-boot0.bin`,
  SHA-256 `393f3c8e45bf5f626eec628c827b49fc9b18e835b34fc13b94edbddc973d287a`;
  disassembly `s1/u0.dis`. DTB `s1/fdt0b.dtb`, SHA-256
  `7d4e6e85f1bfd29329e1cfaab56d241d0905f73e7e1df42786defaf8a793472e`.
* SCLIVE4 5.3.0: `fw-5.3.0/sclive-analysis/00-uboot-u-boot.bin`, SHA-256
  `b865de3fe76c990732949f50fe7eb446a647b2faac952ed2496250b1c2b109d4`;
  existing `jp21-stage2-fdt-1.bin`. `uboot-re-125/sl4/` was empty. The binary
  strings and existing JP21 DTB were inspected; the SCLIVE4 binary's complete
  control flow and the relationship of that DTB to each binary remain to be traced.

## AZ04: startup mode and ADC

`0x33a4` calls the ADC helper `0x2a7b0` with device name
`adc@fec10000` (`0x88c0f`), channel **1**, and an output pointer. On error it
logs the ADC read failure (`0x88cdf`) and returns false. On success it returns
`sample <= 0x545` (1349, inclusive; unsigned comparison). No conversion to
millivolts is performed in this function. `0x33f8` calls it at `0x3458`;
if true and the bootcount test at `0x3460` returns zero, it sets bootcount to
one and sets `bootpart=recovery` at `0x3474..0x3484`. Thus the observed branch
is a **recovery** path; the physical control, voltage, and any debounce or
electrical meaning are not established.

| Condition | Statically observed result | Physical control |
| --- | --- | --- |
| SAR ADC channel 1, raw 0..1349 | Recovery branch, conditional on bootcount | Unknown |
| SAR ADC channel 1, raw >1349 | This ADC recovery branch not taken | Unknown |
| ADC read fails | Error logged; this ADC recovery branch not taken | Unknown |
| ADC sample leading to update-mode or fastboot | No such mapping established in this analysis | Unknown |

`0x33f8` also reads the PMU register at `0xfd588080`. Value `0x5242c355`
sets a global pointer to the string `update-mode` (`0x3448..0x3454`), while
`0x5242c303` sets `bootpart=recovery` (`0x3488..0x3498`). When `0x33f8` sees
`0x5242c301` (`mode-loader`), it rewrites `0xfd588080` to `0x5242c309`
(`0x3430..0x3444`). Its caller at `0x2524..0x25a4` then re-reads `0xfd588080`,
clears it to `0x5242c300` (`mode-normal`), and dispatches `preboot` commands:

| Address (`u0.dis`) | PMU `0xfd588080` value | Action |
| --- | --- | --- |
| `0x3430..0x3444` -> `0x255c..0x2564`, `0x2584..0x2598` | `0x5242c301` -> `0x5242c309` | `env_set("preboot", "setenv preboot; fastboot usb 0")` (`0x88a53`, `0x88a34`) |
| `0x2568..0x2570`, `0x259c..0x25a4` | `0x5242c30c` | `env_set("preboot", "setenv preboot; ums mmc 0")` (`0x88a53`, `0x88a5b`) |
| `0x3448..0x3454` | `0x5242c355` (`mode-update`) | Sets global `inmusic,startup-mode` pointer (`[0xa6aa8]`) to `"update-mode"` (`0x88d00`) |
| `0x3488..0x3498` | `0x5242c303` (`mode-recovery`) | `env_set("bootpart", "recovery")` (`0x8ae5e`, `0x985d6`) |

A separate `bootcount` escalation ladder at `0x356c..0x36d8` reads `bootcount`
via `0x2bc8c` and selects the startup or recovery path:

| `bootcount` | Branch (`u0.dis`) | Console diagnostic | Action |
| --- | --- | --- | --- |
| `0..1` | `0x36b4..0x36b8` (`cmp x0, #0x1; b.ls 0x35a0`) | None | Normal boot (`bootpart=boot`) |
| `2..3` | `0x3688..0x368c`, `0x36bc..0x36d8` | `"too many reboots... trying update mode\n"` (`0x88e8a`) | Sets `[0xa6aa8] = "update-mode"` (`0x88d00`) |
| `4..5` | `0x3570..0x3574`, `0x3690..0x36b0` | `"too many reboots... switching to recovery\n"` (`0x88e5f`) | `env_set("bootpart", "recovery")` |
| `> 5` | `0x3570..0x359f` | `"too many reboots... falling back to fastboot\n"` (`0x88e31`) | `env_set("preboot", "setenv preboot; fastboot usb 0")`; resets `bootcount` to `0` (`0x2bc18(0)`) |

The DTB's `reboot-mode` node separately labels `0x5242c303` recovery and
`0x5242c355` update. `0x331c..0x3390` writes `inmusic,startup-mode` under
`/chosen` when the global pointer is non-null. It is therefore unsafe to infer
an ADC fallback from the absence of that property in the input DTB.

In userspace, `/usr/Engine/Scripts/engine` lines 84–93 (`UpdateFromLoader`
branch, around line 90) branches on `uname -m`:

```sh
if [ "$(uname -m)" = "aarch64" ]; then
    reboot update
else
    reboot loader
fi
```

On 64-bit AZ04 (`aarch64`, PRIME 4+ G2 / RANE SYSTEM ONE), `UpdateFromLoader`
invokes `reboot update` (writing `0x5242c355` to PMU `0xfd588080` so U-Boot
passes `inmusic,startup-mode = "update-mode"` under `/chosen`), whereas 32-bit
decks invoke `reboot loader` (`0x5242c301`, which on AZ04 would instead be
remapped to `0x5242c309` and enter `fastboot usb 0`).

Pseudo-code for the observed U-Boot portions (calls named by behavior, not symbols):

```c
int adc_recovery(void) {
    unsigned sample;
    if (adc_read("adc@fec10000", 1, &sample) != 0) return 0;
    return sample <= 0x545;
}
/* 0x2524..0x25a4 and 0x33f8..0x36d8, simplified */
if (pmu_reboot_mode == 0x5242c301) pmu_reboot_mode = 0x5242c309;
if (pmu_reboot_mode == 0x5242c355) startup_mode = "update-mode";
if (pmu_reboot_mode == 0x5242c303) env_set("bootpart", "recovery");
if (adc_recovery() && bootcount_get() == 0) {
    bootcount_set(1);
    env_set("bootpart", "recovery");
}
unsigned count = bootcount_get();
if (count > 5) {
    env_set("preboot", "setenv preboot; fastboot usb 0");
    bootcount_set(0);
} else if (count > 3) {
    env_set("bootpart", "recovery");
} else if (count > 1) {
    startup_mode = "update-mode";
}
/* Back in 0x2544..0x25a4 after 0x33f8 returns: */
unsigned mode = pmu_reboot_mode;
pmu_reboot_mode = 0x5242c300;
if (mode == 0x5242c309) env_set("preboot", "setenv preboot; fastboot usb 0");
else if (mode == 0x5242c30c) env_set("preboot", "setenv preboot; ums mmc 0");
```

The different ADC thresholds at `0x2fe0..0x307c` are **not established as
boot-mode keys**. Their caller `0x30e8` reads channels 5, 6 and 7 and writes
`inmusic,az04-pcb-rev` (`0x88c6e`); the resulting bins participate in PCB
revision identification. Raw bins are 0..0x154 => 0, 0x155..0x3ff => 1,
0x400..0x6a9 => 2, 0x6aa..0x954 => 3, 0x955..0xbfe => 4,
0xbff..0xea9 => 5, >=0xeaa => 6. Do not use them as a startup-mode table.

The decoded `fdt0b.dts` also has `gpio-keys/key-power` and a separate
`mains-power` ADC channel 4. Neither proves which physical button drives ADC
channel 1. Other DTBs and actual board wiring still need checking.

## Boot storage and update-image evidence

The compiled-in AZ04 environment at `0x9d953..0x9da75` has `bootpart=boot`,
`boot_targets=mmc0` and `bootcmd=run imb_bootcmd`. `imb_bootcmd` resolves
`${bootpart}` on MMC 0, loads `kernel.fit` from that partition, boots the
selected FIT configuration, then resets. The recovery selection above changes
that partition name. These are default environment strings; a saved or imported
environment and later boot stages may behave differently.

U-Boot includes `boot update invalid...`, SPL/U-Boot verification diagnostics,
and `boot update verified OK` (`0x8900a..0x891d1`). These strings establish
that update-related verification code exists, but do **not** by themselves
establish its caller, a USB scan, filename, accepted filesystem, flashing
sequence, or that the vendor `SYSTEMONE-5.3.0-Update.img` is its input. The
full `boot update` control flow and recovery kernel/initramfs USB discovery are
still outstanding. No claim is made that merely inserting USB triggers or
cannot trigger an update in later stages.

## SCLIVE4 comparison

The inspected AZ0x U-Boot's default environment at `0x52c4f` checks
`dorecovery=1` and runs `az01_recovercmd`, then otherwise resolves the MMC 0
`kernel` partition and loads `kernel.fit`. Its stdin includes `button-kbd`.
The examined JP21 stage-2 DTB labels both `mode-loader` and `mode-update`
`0x5242c301`, versus AZ04's `mode-update=0x5242c355` in the inspected DTB.
This is a DTB comparison, not proof of identical button wiring or runtime
handling. The definition/source of `az01_recovercmd`, boot-key handling, USB
scan and update filename have not yet been traced.

## Userspace update path (libaz01-prog, az01-update)

Analysed from the 2026-11-07 disassembly batch (9 tool-result files). Symbol
names come from the dynamic symbol table; addresses are in the stripped object.

**Exported image API (aarch64 `libaz01-prog64.so.1.8.1`):** `az01_open_image`
(`0x8f0c`), `az01_verify_image` (`0x90a0`), `az01_image_for_each_partition`
(`0x92cc`), `az01_image_partition_is_compatible_model` (`0x9340`),
`az01_image_is_compatible` (`0x93a0`), `az01_image_partition_is_compatible`
(`0x9440`), `az01_write_partition_image` (`0x9500`),
`az01_image_set_onboard_key_lookup` (`0xd184`), `az01_download_init` (`0xd400`).

**Image operation tables: not resolved.** `az01_image_fdt_ops`,
`az01_image_v2_ops` and `az01_image_default_ops` are data tables. They are
absent from the batch (no symbol refs, no dumped `.data.rel.ro`), so their
per-format handlers are unknown. Dump `.data.rel.ro` and resolve the `R_AARCH64_RELATIVE`
entries before naming any handler.

**Platform nodes and power paths (`.rodata`).**

| Offset | String | Platform |
| --- | --- | --- |
| `0x11e08` | `/sys/firmware/devicetree/base/inmusic,az01-pcb-rev` | AZ01 |
| `0x11e40` | `/sys/firmware/devicetree/base/inmusic,az05-pcb-rev` | AZ05 |
| `0x11fb8` (`A`) | `/sys/firmware/devicetree/base/inmusic,az04-pcb-rev` | AZ04 |
| `0x11ff0` (`B`) | `/sys/firmware/devicetree/base/inmusic,az07-pcb-rev` | AZ07 |
| `0x12e80` | `/sys/class/power_supply/az01-ac-power/online` | AZ01/AZ05 (32-bit) |
| `0x12ec0` | `/sys/class/power_supply/mains-power/online` | AZ04/AZ07 (64-bit) |

**`az01_apply_image` dispatch (aarch64, `0x5b28`).** Two existence checks run
first on `A` and `B`.

| Condition | Action |
| --- | --- |
| `access(A) == 0` or `access(B) == 0` (AZ04/AZ07) | tail-call `0x58ac` |
| neither exists, helper `0x54c0` returns bit 0 set (AZ01/AZ05) | tail-call `0x59c0` |
| neither exists, bit 0 clear | return `-11` |

* **`0x58ac` (64-bit AZ04/AZ07):** after verifying the image, resolve the block
  device `<dev>` and relative path `<relpath>`, then run
  `/usr/bin/systemctl --reboot-argument=update:<dev>:<relpath> reboot`
  (`0x11c68`, `0x11f00`).
* **`0x59c0` (32-bit AZ01/AZ05):** after verifying, run
  `/usr/sbin/kexec --load --ramdisk /opt/ramfs-update.squashfs --command-line
  "ramdisk_size=32768 root=/dev/ram0 ro -- upgrade-device=<dev>
  upgrade-file=<relpath>" --kexec-syscall-auto /boot/zImage`
  (`0x11cc0`..`0x12088`), then `systemctl kexec`.

`az01_usb_update_mode` (`0x5c48`) uses the same `A`/`B` and `0x54c0` checks:

* **`0x500c` (64-bit AZ04/AZ07):** `/usr/bin/systemctl --reboot-argument=update reboot`
  (`0x11c80`). The kernel `syscon-reboot-mode` writes `0x5242c355` (`mode-update`)
  to PMU `0xfd588080`.
* **`0x4fc0` (32-bit AZ01/AZ05):** `/usr/bin/systemctl --reboot-argument=loader reboot`
  (`0x11c30`), which writes `0x5242c301` (`mode-loader`) to PMU.
* Otherwise it returns `-11`. Helper `0x54c0` is not analysed.

`az04_apply_image` and `az01_kexec_apply_image` do **not** appear in the batch.
Their mapping to `0x58ac` / `0x59c0` is therefore unknown, and this document
does not assign them.

**`az01-update` (32-bit, `/usr/sbin/az01-update`)** strings and calls confirmed:

* usage: `az01-update [--file|--net] <file|url>`, `az01-update --erase-data`;
  `--usb` = "Reboot into USB update mode"; `conflicting modes specified`.
* phases: `Downloading %s`, `Verifying %s`, `Applying %s`, `failed to update`.
* `BUG: az01_apply_image returned 0` is printed if apply returns 0.
* calls `az01_apply_image@plt`, `az01_erase_data_partition@plt` and
  `az01_download_*`.

## Recovery ramdisk (`/init`, `/shutdown`)

Sources: `/var/tmp/az04-usb-sandbox-test/recovery-ramdisk/` (aarch64).
`init` sha256 `6b215096…88fd`. `/shutdown` and `usr/bin/az0x-splash` are the same
binary (`a56c9ceb…05dd4`), linked against `libdrm`, `libpng`, `libmount` and
`libaz01` (`az01_image_for_each_partition`,
`az01_image_is_compatible_current_machine`, `az01_image_partition_is_compatible_model`,
`az01_image_set_onboard_key_lookup`, `az01_close_image`). It is a splash program
that reads the image. Its imports in the dump show no write function.

`init` flow, read from the disassembly (`/init`, not from strings):

1. Read a text file with `fopen`/`getdelim`, split by `strsep`, match five
   `strcmp` tokens (`0x1ac0..0x1b50`). The tokens are not extracted.
2. `access(<path @ 0xac8>)` (`0x1700..0x170c`). If present, branch to `0x1c04`
   (not analysed).
3. Otherwise `mkdir` (`0x1f68`), then `mount(…, MS_BIND)` (`flags 0x1000`) at
   `0x1738` and `mount(…, MS_BIND|MS_REMOUNT|MS_RDONLY)` (`flags 0x1021`) at
   `0x1754`, on the `0xb00` path.
4. Loop over the parsed items (`0x1774..0x1794`), then `chdir` (`0x1a24`) and
   `execl` (`0x1a68`).
5. Daemon path: `fork`, `setsid`, `fork`, `execv` (`0x1fdc..0x2040`).
6. USB storage mount helper (`0x2100..0x216c`): libmount `mnt_new_context` →
   `set_source` / `set_target` / `set_fstype` / `set_options` → `mnt_context_mount`.
7. `waitpid` (`0x1bd0`), `sync` (`0x1bf0`), then
   `reboot(0x01234567)` = `LINUX_REBOOT_CMD_RESTART` (`0x1bfc`).

Not established: the source device, filesystem type, target path, and the exact
`execl`/`execv` target. Those are string references. Whether `init` calls
`az01-update --usb` is not established either. Dump `.rodata` for the ramdisk and
`libaz01-prog64`, then resolve the `0x1f50`/`0x1e40` helpers, the `0x1c04` branch,
and the strings at `0xac8`, `0xaf0` and `0xb00`.

## Boot ramdisk and `az0x-splash` (corrected)

Sources: `/tmp/boot-ramdisk/init`, `/tmp/boot-ramdisk/shutdown` and
`/tmp/libaz01-prog64.so` on the build host. This section supersedes the
"splash program, no write function" statement in the recovery section above.

**`09-boot.img` ramdisk (`6,989,982` bytes), `/init` (sha256 `48ebd306…`).** It reads
`/sys/firmware/devicetree/base/chosen/inmusic,startup-mode` (`0x2da8`):

* `"normal"` (`0x2e10`): mount `/dev/mmcblk0p10` through `dm-verity`
  (`--table=`, `0x2ce0`) and `switch_root` to the main system.
* `"update-mode"` (`0x2e18`): start `/usr/bin/az0x-splash usbupdate`
  (`0x28b8`, `0x2f68`).

**`/shutdown` and `/usr/bin/az0x-splash`** (sha256 `a56c9ceb…`) are one
multi-function program with two complete update engines:

* **Local file / USB flash (`update:<dev>:<relpath>`, `0x15e48`).** After systemd
  switches to `/run/initramfs/shutdown`: mount `<dev>`, open `<relpath>`, show
  `*-file-update.png` (`0x14860`/`0x148e8`), load `/usr/lib/az0x/pubkeys/%s.pub`
  (`0x14a88`) and verify the signature, then write the partition table, each
  partition and `BOOT` to `/dev/mmcblk0` (`0x14468`, `0x15bb8`, `0x15dd8`).
  On success it prints `file update complete: %s` (`0x14408`) and reboots.
* **USB gadget flash (`usbupdate`, `0x16008`).** Configures the ConfigFS gadget
  `/sys/kernel/config/usb_gadget/update` (`0x165b0`) and FunctionFS
  `functions/ffs.update` (`/dev/ffs-update/ep0..ep2`, `0x166a8`..`0x16868`).
  Host commands handled: `get-property`, `erase-part`, `write-part`,
  `write-boot`, `get-pubkey` (`0x12718`..`0x12758`).

## Open work and safety boundary

1. Trace the update-mode global into the handed-off kernel and recovery image;
   trace `boot update` from its command dispatcher, including source and writes.
2. Disassemble the specific SCLIVE4 U-Boot and correlate its matching FIT/DTB,
   key drivers, and recovery environment; locate actual USB scan and filename.
3. Identify ADC channel 1 wiring using schematics or non-destructive measurement,
   then perform explicitly supervised hardware verification.
4. Verify the full end-to-end software signature chain (`az01-update` / `az0x-splash`
   container RSA check -> U-Boot FIT verification -> `dm-verity` rootfs) in a
   sandbox before any on-device image test. No hardware eFuse lock is used by the
   updater; modified `.img` files on a stock system are rejected in software at the
   `Verifying` step before any partition write occurs.
