# EngineX OS — notes for AI agents

Custom system rebuilt on the official Engine OS 5.3.0+ base for 16 Denon DJ / Numark / Rane standalone units: 8 first-generation models as `.img` (3 image groups) and 8 second-generation models as `/data` bundles (6 armhf on SC LIVE 4's Engine, PRIME 4 G2 / SYSTEM ONE on the 64-bit AZ04 Engine). Engine 5.0.4 is no longer developed: new work targets 5.3.0+ only. Read [`CONTRIBUTING.md`](./CONTRIBUTING.md) first.

- **Releases**: follow [`.claude/rules/release-workflow.md`](./.claude/rules/release-workflow.md) step by step: version file, all three READMEs (24 download links), build, image checks, tag, Release Notes SHA-256 table, change record.
- **Version**: stays at the current one (5.0.4.2) unless the maintainer explicitly asks for a new version. Fixes are re-released over the same Release with `--clobber`.
- **Build and test only on `Oracle-Batam-arm`** (ssm alias; Oracle Batam, Indonesia, aarch64 with 32-bit EL0, so the deck's armhf code runs natively), never on the maintainer's laptop. Since 2026-11-05 this project no longer uses IN: run `REMOTE_BUILD_HOST=Oracle-Batam-arm remote-build ...` (the script still defaults to IN). Firmware, qrc and `fsck.hfsplus` live in `/root/build/sc5000-lab/` there. Use a fresh `--clean` directory for release builds.
- **Never trigger Factory Reset or `az01-update --erase-data` on a real deck**; test them with `unshare -m` + chroot on the unpacked rootfs on `Oracle-Batam-arm`.
- **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`) change together and keep the current layout: aligned grids, few emojis, direct download links (8 `.img` + 8 `Data-Bundle.tar.gz` per README).
- **Stock first**: unlock a feature Engine already has (a hidden setting, a board flag, a config value) before building our own; do not add an EngineX switch that duplicates a stock one.
- **No secrets or private names** in commits, Release Notes or logs: keys, tokens, Deezer ARL, passwords, the maintainer's private domains and host names.
