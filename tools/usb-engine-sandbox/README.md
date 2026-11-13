# usb-engine-sandbox

Runs the **stock Engine and the image's own edisksd** against loop-image USB sticks, so a
USB or library bug can be checked against what Engine really lists, without a deck.

Build host only, as root, on aarch64 with 32-bit EL0 and the host's armhf Mesa
(`/usr/lib/arm-linux-gnueabihf`). Never on a deck.

## What it does

- Everything runs in a private `unshare -m -n -p` namespace, chrooted into a tmpfs overlay
  of the read-only `rootfs.ext2`. `/tmp`, `/run`, `/dev`, `/data`, `/media` and `/sys` are
  private, and `systemctl`, `reboot`, `kexec`, `poweroff`, `shutdown`, `az01-update` and
  `encrypt-fs.sh` are shims that refuse. The host's `/tmp`, system bus, udev and systemd are
  never touched.
- Each stick is a loop image (`mkfs.vfat -F 32` or `mkfs.exfat`), filled with a content
  directory. edisksd finds it through a private udev database: blkid's `ID_FS_*` (what
  udev's blkid builtin sets) plus `EDISKSD_REMOVABLE=1`, as a USB stick gets on the deck.
- The image's edisksd runs on a private D-Bus system bus and mounts the sticks with its own
  options. Engine attaches them over `io.github.inmusicdev.edisksd`.
- Engine runs with software GL, a null ALSA device (`tools/shadow-twin/alsa_null_fixture.c`)
  and the UI hook built as a loader only: no Engine edits. Its only job is to serve
  `probe.qml` in place of `QmlWarnings.qml`.
- `probe.qml` selects each stick, opens the browse view, and logs the visible texts, the
  connected-source rows and the librarian state.

## Run

```sh
python3 -I tools/az01-packer/az01_packer.py unpack --output /root/build/<you>/p4 PRIME4-5.3.0-Update.img
STICKS='FATSTICK vfat /path/to/export-a
EXSTICK exfat /path/to/export-b' \
  bash tools/usb-engine-sandbox/run.sh /root/build/<you>/usb-sbx /root/build/<you>/p4/rootfs.ext2 240
python3 -I tools/usb-engine-sandbox/read-probe.py /root/build/<you>/usb-sbx/out/engine.log
```

- `STICKS` takes one line per stick: `<label> <vfat|exfat> <content dir>`.
- Labels must be upper case for FAT; the probe matches them exactly.
- Each stick takes about 40 s after Engine's QML is up, and Engine boots in about 60 s
  under llvmpipe. Give at least 120 s plus 40 s per stick.

Environment:

| Variable | Meaning | Default |
|---|---|---|
| `PRODUCT` / `COMPAT` | device-tree product code / compatible string | `JC11` / `inmusic,jc11` |
| `SBX_CARD` | ALSA card name the product expects | `PRIME 4 Audio` |
| `STICK_SIZE` | size of each loop image | `512M` |
| `PROBE` | another probe file | `probe.qml` |

## Output (`<work dir>/out/`)

| File | Content |
|---|---|
| `engine.log` | Engine's log with the probe's `FSP` lines |
| `edisksd.log` | edisksd's mounts |
| `edisksd-objects.txt` | edisksd's D-Bus objects: Block (Removable, Media, Size…) and Filesystem (Type, Label, UUID, Mountpoints) |
| `mounts.txt` | the sticks' mount lines, with the options edisksd chose |
| `sticks.txt`, `udev-<label>` | the loop devices and the udev records given to edisksd |
| `img/` | the stick images, for a look afterwards |

## Known limits

- Tested with the PRIME 4 (JC11) 5.3.0 image. The PRIME 4+ (JC11S) Engine does not boot
  in this kind of sandbox.
- Engine logs device labels as hashes (for example in `EDisks filesystem added`). The
  source rows and edisksd's D-Bus `Label` carry the real label.
- The stock rekordbox import prompt stays open; the probe only logs it.
