# SC5000 Stems runtime deployment — 2026-11-03

## Installed

- Engine file remains unchanged: SHA-256 `342b611bd71fd09d9fda8013f19dccfba47ee4f0f772a959058bdd19d74d5413`.
- Hook: `/data/sc5000-custom/lib/libengine_ui_hook.so`, 0755, SHA-256 `2d1b8f8c36069d031f45a8db625617d358309faa15502b313d9994a2157d2336`.
- Existing LD_PRELOAD path `/etc/sc5000/libengine-ui-hook.so` is now a symlink to that hook. No Engine service drop-in was changed.
- Client: `/data/sc5000-custom/bin/stems-processor`, 0755, SHA-256 `aac8691dfc9dba84c5087deb912edb28b30149fd4824686410a8663fc4b88017`.
- `/usr/local/bin/stems-processor` links to the client. The root filesystem is read-only, so `/etc/sc5000/stems-runtime-prepare` mounts an overlay preserving the existing `/usr/local/bin` lower directory. Upper/work directories are `/data/sc5000-custom/stems-bin-overlay/{upper,work}`.
- The existing `/etc/sc5000/engine-ui-guard` invokes this helper in its `pre` case. Repeated helper execution succeeded without stacking another overlay. Existing vendor commands remain visible.
- Credentials were provisioned separately by the coordinating agent; no credential was read, printed, or included in this report or backups.

## Validation

All builds ran on IN, in isolated `/root/build/sc5000-stems-hook-runtime`, through `remote-build`. ARMv7 hard-float compilation used `-O2 -Wall -Wextra -Werror -shared -fPIC`; highest required GLIBC version is 2.34, below the deck's 2.39.

The four Stems edits match the upstream manifest and original Engine bytes exactly: `0x85ef9c` (63), `0x85f000` (20), `0x85e5f8` (4), `0xf6303c` (4). The first executable PT_LOAD has file offset and virtual address zero and covers all seven existing/new edits.

An IN C harness executed the actual edit function against synthetic mapped memory. It passed matching edits, whole-Stems-group rejection on one mismatched expected byte, an 8197-byte edit crossing several pages, and readback of restored RX permissions. The temporary harness is at `out/hook-test.c` in the isolated IN build directory; it is not a native ARM execution test.

Engine restarted at 11:59:16 Asia/Bangkok, PID 5034. Its maps show the new hook from `/data`; logs confirm all four Stems edits plus the three existing edits, and 185 zh-Hant UI resources loaded. At 12:00:51 the same PID remained active/running, NRestarts was 0, the 90-second health guard had cleared its boot counter, and UI safe-mode was absent. No process-exit or segfault entries appeared in the Engine unit log for this window. Each of the four patched regions was read back from `/proc/5034/mem` and matched its complete replacement bytes exactly.

The client command `stems-processor -s -hv 1.0.0 -pv 1.6.0 </dev/null` returned exit 0, empty stderr, and 26 response bytes: `session=FFFFFFFFFFFFFFFF\n\n`. This was repeated from `/usr/local/bin/stems-processor` after deployment.

Independent Claude review by 严DE: PASS, no blocking findings. Agent `8436565f-b906-464b-af1f-7cfbf43f3738`; workspace archived after review. Review did not independently access the deck; live checks above were performed by the implementation agent.

## Backup and rollback

Restorable remote backup of the original hook, guard and service drop-ins:

`20261103T045348Z-maintainer-laptop-sc5000-before-stems-hook-guard-a5b7bc357c76-sc5000-stems-hook-guard-prestate.tar.gz`

SHA-256 `a5b7bc357c76ded88a9a54c741e6b24af83cd6780ca4c1075f26c439ee84078a`. Download round trip and archive member listing verified. The target `/data` hook and client paths did not exist before deployment. The original vendor `/usr/local/bin` was also backed up under label `sc5000-before-stems-runtime`.

To roll back: stop Engine; remove the `/etc/sc5000/libengine-ui-hook.so` symlink; restore the original hook and `engine-ui-guard` from the backup; remove the new `/etc/sc5000/stems-runtime-prepare`; unmount `/usr/local/bin` when no process is using its overlay; then start Engine. The original read-only vendor directory reappears. Preserve credentials, audio and databases. The install script staged an automatic rollback for synchronous deployment failure; it is not a continuing watchdog.

## Limits

Startup/handshake and live patching passed. No real track separation, encrypted `.stems` creation, per-stem playback, cancellation, or backend-outage test was performed. A full hardware reboot was not performed, so boot-time mount ordering remains unverified. The existing safe-mode file affects UI resource replacement, not these Engine memory edits. Do not use Engine's “Remove Stems Support” button: its removal code targets the client path.
