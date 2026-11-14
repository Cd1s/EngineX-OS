# Native systemd boot sandbox

Run as root on the ARM build host, with native armhf execution and
`systemd-container` installed (`apt-get install --no-install-recommends systemd-container`).
An enabled `qemu-arm` binfmt handler is rejected. No QEMU or compiler is used.

```sh
sim/nspawn-boot/boot.sh /path/to/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img
bash sim/nspawn-boot/test_boot.sh /path/to/SC5000-5.3.0.4-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img
```

The packer must report `"verified": true`. Each run copies the unpacked rootfs,
starts with empty `/data`, and boots the image's own systemd as PID 1 using
`systemd-nspawn --boot`. Mounts stay inside `unshare --mount`; the container has
its own network, devices, `/run`, `/tmp`, `/proc` and read-only hardware sysfs.
Host time, modules, raw hardware and reboot capabilities are dropped. No host
block devices are passed through. Runs are sequential.

The test checks etc-reset execution, prepare and the core services, the current
UI-ready token without fallback, Engine process liveness, the release gate's
default applied-hook count (20 on players, 28 on consoles), pack replacement,
the gate's QML load-error patterns, an empty `systemctl --failed`, and a JSON
response from `/v1/sc5000/ui`. `WALL TIME` measures nspawn start to UI readiness;
the remaining boot jobs are checked afterward. Readiness must take under 60 s.

Validated with the 5.3.0.4 release fixtures, guest systemd 255 and nspawn 257:
JP07 12.701 s (20/20 hooks), JP13 12.497 s (20/20), JC11 13.504 s (28/28).
All readiness/service/API/QML checks passed. The JP07 self-test also verified
that the release image SHA-256 did not change. Bash syntax, ShellCheck and
byte equivalence of the extracted release-gate recorder passed.

Test-only changes to the copy:

- Reuse the release gate recorder for kexec, reboot, encrypt-fs and az01-update.
  Updates are recorded only; this test does not flash anything. Keep real
  `systemctl` so the guest manages its own services.
- Reuse the gate's ttyS0 interrupt record and writable IRQ cover, plus a null
  `/dev/ttyS0` and product-code/compatible device-tree files.
- Describe the pre-mounted test `/data` without the vendor eMMC mkfs dependency;
  connect etc-reset before prepare (the old etc.mount trigger is absent).
- Select Qt `offscreen`, as in the release gate. QEngine's DRM/ALSA interposers
  are unnecessary for this headless UI test; it provides no rendered GPU/audio
  acceptance evidence.
- Mask `az0x-data-mkfs.service`, `touch-fw-update.service`,
  `xmos-update.service`, `bluetooth.service`, `az0x-loadpin-trustpoints.service`,
  `az01-usbsata-fixer.service`, `az0x-info.service`, and
  `systemd-networkd-wait-online.service`. The last one cannot obtain a physical
  network link in the isolated namespace. The list is also printed per run.

Default cleanup removes the fresh `/var/tmp/nspawn-boot.*` directory after the
container stops. `--keep` retains the copied rootfs, `/data`, console and guest
journals, unpack verification, failed-unit list, API JSON, and results; it still
stops the container and releases mounts. Retained `/data` includes generated
test SSH keys: do not publish it. Delete retained work directories when finished.

This covers userspace service ordering and headless Engine/UI startup. It does
not cover the deck kernel, bootloader, eMMC flash, real device tree, GPU/DRM
rendering, audio, controllers, physical storage or networking. Use QEngine when
a guest kernel, virtual block-device update path or DRM/audio emulation is
needed, and use real decks for hardware acceptance. This is not a substitute
for the complete release gate or real-deck regression checklist.
