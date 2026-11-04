# EngineX OS — notes for AI agents

Custom firmware for Engine OS 5.0.4 standalone decks (8 models, 3 image groups). Read [`CONTRIBUTING.md`](./CONTRIBUTING.md) first.

- **Releases**: follow [`.claude/rules/release-workflow.md`](./.claude/rules/release-workflow.md) step by step: version file, all three READMEs (24 download links), build, image checks, tag, Release Notes SHA-256 table, change record.
- **Version**: stays at the current one (5.0.4.1) unless the maintainer explicitly asks for a new version. Fixes are re-released over the same Release with `--clobber`.
- **Build and test only on the build host** (`remote-build`), never on the maintainer's laptop. Use a fresh `--clean` directory for release builds.
- **Never trigger Factory Reset or `az01-update --erase-data` on a real deck**; test them with `unshare -m` + `qemu-arm` on the unpacked rootfs.
- **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`) change together and keep the current layout: aligned grids, few emojis, direct `.img` download links.
- **No secrets or private names** in commits, Release Notes or logs: keys, tokens, Deezer ARL, passwords, the maintainer's private domains and host names.
