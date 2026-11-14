# sc5000 → enginex rename (#234, 5.3.0.5)

From 5.3.0.5 everything EngineX OS installs or runs carries the EngineX name. Old names keep
working where something outside this release still uses them: an older deck, an installed
Mod, a hand-written script, a guide. This page lists both.

## Old → new

| What | Before 5.3.0.5 | From 5.3.0.5 | Old name kept as |
| --- | --- | --- | --- |
| Data directory | `/data/sc5000-custom` (`/data/enginex` since #103) | `/data/enginex` | link `/data/sc5000-custom -> enginex` (permanent) |
| Config directory | `/etc/sc5000/` | `/etc/enginex/` | image link `/etc/sc5000 -> enginex` (permanent) |
| Libraries | `/usr/local/lib/sc5000/` | `/usr/local/lib/enginex/` | link |
| Shared files | `/usr/share/sc5000-custom/` | `/usr/share/enginex/` | link |
| Boot scripts | `/usr/libexec/sc5000-custom-{prepare,etc-reset}`, `sc5000-timesync-seed` | `/usr/libexec/enginex-{prepare,etc-reset}`, `enginex-timesync-seed` | — |
| Hand-run tools | `/usr/local/bin/sc5000-{debloat,perf-tune,prolink-diag,storage-mount}`, `/usr/sbin/sc5000-usb-audio` | `enginex-*` | links under the old names |
| Other tools | `sc5000-{deemixd,suite-exec}`, `/usr/sbin/sc5000-{perf-tune,storage-discover}`, `/usr/local/libexec/sc5000-storage-notify` | `enginex-*` | — |
| Image units | `sc5000-custom.target`, `sc5000-custom-{prepare,sshd,etc-reset}.service`, `sc5000-{debloat,deemixd,perf-tune,storage-discover}.service`, `sc5000-storage@.service`, `sc5000-{background,engine}.slice` | `enginex.target`, `enginex-*.service`, `enginex-storage@.service`, `enginex-*.slice` | — (etc-reset removes upper-layer copies under both names) |
| Image config | `/etc/default/sc5000-*`, `sysctl.d/90-sc5000-perf.conf`, `modules-load.d/sc5000-filesystems.conf`, `journald.conf.d/sc5000.conf`, `udev 60-sc5000-storage.rules`, `dnsmasq.d/sc5000-telemetry.conf`, `/var/cache/sc5000` | `enginex` in the same places | — |
| 2nd-gen `/run` units | `enginex-sc5000-deemixd.service`, `engine/edisksd.service.d/99-sc5000.conf` | `enginex-deemixd.service`, `99-enginex.conf` | removed by `boot-activate.sh` at an in-place update |
| Suite binary | `bin/sc5000-suite`, applet `sc5000-deemixd` | `bin/enginex-suite`, applet `enginex-deemixd` | the suite accepts both applet names; updaters end both |
| Runtime files | `/run/sc5000*`, `/tmp/sc5000-*` | `/run/enginex*`, `/tmp/enginex-*` | `/tmp/sc5000-debug` still read; `/run/sc5000-hfs-views` still hidden |
| Tailscale | unit `sc5000-tailscaled`, `/run/sc5000-tailscaled.sock` | `enginex-tailscaled`, `/run/enginex-tailscaled.sock` | old unit stopped; old socket linked for installed Mods |
| Environment | `SC5000_*` | `ENGINEX_*` | `SC5000_CUSTOM`, `SC5000_ALSA_OVERLAY`, `SC5000_UI_SCALE`, `SC5000_STEMS_*` still read |
| Local API | `/v1/sc5000/*` | `/v1/enginex/*` | `/v1/sc5000/*` permanent alias |
| Console host name | `sc5000.lan` | `enginex.lan` | both count as placeholders |

## Frozen in 5.3.0.5

These keep their sc5000 names, because every installed deck loads Engine through them:

- the ALSA file `99-sc5000-custom.conf`;
- the ALSA hook symbol `sc5000_alsa_boot_hook`;
- the library path `/data/sc5000-custom/bundle/lib/libengine-ui-hook.so` the ALSA file names.

## Upgrades and downgrades

- **Upgrade** from 5.3.0.2 … 5.3.0.4-r5 (1st gen by image, 2nd gen by bundle, screen OTA, the USB
  hook, EngineX-Installer) needs nothing by hand. The updater ends the suite under both names;
  `boot-activate.sh` takes the old `/run` units, drop-ins, the old engine-script bind and
  `/run/sc5000-*` out before it writes the new ones, so each daemon runs once.
- **Downgrade** of a second-generation deck to an older bundle (screen OTA, the USB hook,
  EngineX-Installer): the older release's own `install.sh` never runs over `/data`. It fills a
  stage in `/data/.enginex-leave`, and this release moves the result in with atomic swaps
  (`renameat2` `RENAME_EXCHANGE`): UI packs and icons, version files, the bundle last
  (`internal/leave`). A power cut before the bundle swap is put back by this release's
  `boot-activate.sh` at the next boot; after it the older release is whole. A release before
  5.3.0.3 also gets its layout back: the real `/data/sc5000-custom`, `/data/enginex` the link
  (its installer and prepare refuse the link). A deck that cannot swap atomically, or a damaged
  bundle, is refused with nothing changed. Then the leave step (`/run/enginex/leave.sh`) removes
  this release's `enginex-*` units, `99-enginex.conf` drop-ins and runtime files, never following
  a symlinked directory, and reboots. Older releases are not changed.
- **Downgrade** of a first-generation deck flashes an older image in Firmware Update Mode, where
  nothing of this release runs. 5.3.0.3 and later boot on the current layout. Before flashing
  5.3.0.2 or older, run `enginex-suite-exec leave layout` over SSH: it hands
  `/data/sc5000-custom` back the same way, without installing anything. Booting 5.3.0.5 again
  instead moves it forward again (prepare's migration).
- The next upgrade from a handed-back layout moves it forward again: the migration takes
  `/data/enginex` back from a link to `sc5000-custom` (or, cut half-way, to itself).

## Where sc5000 is left

- Shipped files: only what `tools/az01-packer/test_rename234_names.py` allows, each rule with its
  reason (frozen ALSA names, the data/config links and their readers, the API alias, the model
  name, older environment names, old names an in-place update ends or cleans up, the stems
  server side, a few comments about the old layout). A new sc5000 in a shipped file fails that test.
- Server side, unchanged: `cmd/sc5000-relay`, the `sc5000-stemsd` alias, `/etc/sc5000-stems`,
  `deploy/`; the default worker is `/opt/enginex-stems/worker.py`, with the old `/opt/sc5000-stems/worker.py` read on upgrade.
- Persisted ids: stems list ids start with `sc5000-stems-`.
- Dead legacy: `/media/sc5000-sd-cache` (unused since `8856176`, still read).
- Lab paths on the build hosts (`/root/build/sc5000-lab`), the Denon model name `SC5000` and
  official image names (`SC5000-<version>-Update.img`).
- Tests, which use old names on purpose to prove the compat paths, and the published 5.3.0.2 and
  5.3.0.3 installer scripts they run unchanged (`internal/leave/testdata`).
- History documents (`docs/ISSUE-FIXES-*`, `docs/REGRESSION-5.3.0.3-to-5.3.0.4.md`,
  `docs/re-report/`, dated reports and release notes): they describe the system as it was.
