# Contributing to EngineX OS

Thanks for helping. EngineX OS runs on decks that people play live on, so the bar is: never break audio, never lose a DJ's library, never brick a deck.

## Issues

- Use the **Bug report** or **Feature request** form. Blank issues are disabled.
- Always give the hardware model and the EngineX OS version (Settings → About).
- Remove passwords, Deezer ARL, tokens and private keys from logs before posting.

## Pull requests

1. Fork, branch from `main`, keep one topic per PR.
2. Fill in the PR template checklist. A PR that touches the audio path, the UI hook or the firmware packer needs a test.
3. Commit messages follow `type(scope): summary`, e.g. `fix(guard): restart Engine after a failed factory reset`.
4. Do not commit firmware images, extracted root filesystems, vendor binaries or build output (`.gitignore` blocks the common ones).

### Ground rules

- **Real-time audio**: the 96 kHz `SCHED_RR 49` audio thread must never block, wait on a lock, allocate or use a large stack frame.
- **All models**: the same image group serves several models (see the table below). Check every model of the group, or limit the change explicitly.
- **Destructive operations**: Factory Reset, `az01-update --erase-data` and boots with a blank `/data` are tested in a sandbox or VM (`unshare -m` + `qemu-arm` with the unpacked rootfs), never on a real deck.
- **Engine databases**: while Engine has a library open, no other process may write `m.db` / `hm.db`.
- **Vendor units**: never put a hard `Requires=`, `BindsTo=` or `Requisite=` on a unit the vendor ships, because a new base may drop it (5.3.0 dropped `etc.mount`, and the 5.0.4.2 → 5.3.0.1 online update lost the UI packs and SSH). Use `RequiresMountsFor=` for mounts and `Wants=` + `After=` for everything else.
- **No regressions**: every fix ships with a test that fails on the old code. Before changing a mechanism, look up the issues it already fixed (`git log -S`, the issue numbers in tests and `docs/ISSUE-FIXES-*.md`) and keep their tests green. A fix to a shared mechanism (MASTER/sync election, hook sites, header layout, patch dicts) also tests the neighbouring behaviours. Never delete or weaken an existing test to make a change pass. Every new real-deck bug adds a line to [`docs/REGRESSION-CHECKLIST.md`](./docs/REGRESSION-CHECKLIST.md).

### Mods

The decks have no Python, no compiler and no package manager (#208). An EngineX Mod is QML (`mod.json` → `qmlFiles`) plus an optional busybox `sh` lifecycle script (`start.sh`) and an optional static helper binary for both deck CPU families: 32-bit armhf (`GOOS=linux GOARCH=arm GOARM=7`, all gen-1 and armhf gen-2 models) and 64-bit arm64 (`GOOS=linux GOARCH=arm64`, PRIME 4 G2 / SYSTEM ONE), selected at runtime via `uname -m`. Go with `CGO_ENABLED=0` is recommended so the binary has no runtime library dependencies on the deck. Start from [`docs/mods/src/helper-template/`](./docs/mods/src/helper-template/) (`README.md`, `main.go`, `start.sh`).

### Tests & Experimental Inspection

Run builds and tests on **the build host** (`REMOTE_BUILD_HOST=<build host> remote-build ...`, aarch64 with native 32-bit `armhf` execution). For experimental component inspection and ABI/resource analysis, use **`craft-anything`** (`craft-anything-cli` / `craft-anything` skill) with **`deepseek-v4.1-flash`** (`deepseek4.1 flash`) on the build host:

```sh
gofmt -l . && go vet ./... && go test -race ./...
bash tests/test_rootfs_overlay.sh            # bash, not sh
JC11S_ROOTFS=/path/to/official/PRIME4PLUS/rootfs bash tools/az01-packer/test_sclive4_bootstrap.sh
python3 -m unittest discover -s tools/az01-packer
python3 -m unittest discover -s tools/stems
# C compatibility adapters: build commands are in the header of each file
#   tools/ui-mod/test_dsp_smooth.c   (also with ASan/UBSan and TSan)
#   tools/ui-mod/test_stems_feature.c (armhf native on the build host)
#   tools/edisksd-hook/test_edisksd_hook.c
#   tools/ui-mod/test_az04_hook.c
```

The armhf bootstrap command above includes both SC LIVE 4 and the separate
official PRIME4PLUS (JC11S) rootfs. Without `JC11S_ROOTFS`, a standalone run
warns that JC11S was not tested; release validation must supply the fixture.
An explicitly supplied missing directory is an error. For the actual published
archive through the installer shim, set `JC11S_RELEASE_BUNDLE` to its verified
PRIME4PLUS data-bundle path when running Go tests. See
`docs/issue169-prime4plus-investigation.md` for official-image and bundle digests.

## Releasing a new version

Maintainers follow the checklist in [`.claude/rules/release-workflow.md`](./.claude/rules/release-workflow.md). In short, for version `<VERSION>`:

1. **Version file**: `rootfs-overlay/etc/enginex/version.txt`.
2. **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`) and **`docs/site/content.js`** (+ `?v=N` in `docs/index.html`): the hero image `alt`, the version in the intro and the performance table, and all **20** primary download links in "How to Install & Update" (4 `EngineX-Installer-<VERSION>-*` + 8 first-gen `*-Update.img` + 8 second-gen `*-Update.img`; `*-Data-Bundle.tar.gz` is also published alongside them in `5.3.0.4`/`5.3.0.5` for legacy Gen-2 OTA before retiring in `5.3.0.6+`, #215):
   `https://github.com/Cd1s/EngineX-OS/releases/latest/download/<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`
3. **Hero banners** `docs/assets/hero-banner*.svg`: only if they show a version (they do not today).
4. **Build** all 16 `*-Update.img` images (8 Gen-1 + 8 Gen-2), 8 compatibility `*-Data-Bundle.tar.gz` archives, 4 `EngineX-Installer-<VERSION>-*` archives, and `SHA256SUMS.txt` (28 artifacts total in `5.3.0.4`/`5.3.0.5`) with `tools/az01-packer/build-release.sh <VERSION> <fw> none ...` and `tools/az01-packer/build-installer.sh <VERSION> <out>` from a clean checkout, then spot-check one image per group. Public images never carry an SSH key (`none`).
5. **Release gate** (mandatory, also for a new vendor base and for same-version re-releases): run `tools/az01-packer/release-gate.sh <previous release .img> <new .img> <VERSION> <new work dir>` as root on the build host, once per image group (JP07, JP13, JC11). It audits the new rootfs (unit dependencies, commands, tool paths, shared libraries), runs the previous release's online update to the new image, and then boots the new rootfs on the same `/data` and checks every core feature: services, UI hook counts, DSP guards, streaming gateway, Stems, Rekordbox, storage and the next online update. No release unless it exits 0.
6. **Tag and Release** `v<VERSION>`: upload all 28 release artifacts (16 `*-Update.img` + 8 `*-Data-Bundle.tar.gz` + 4 `EngineX-Installer-*`) and `SHA256SUMS.txt` (29 files), and put the SHA-256 table of all 28 artifacts in the release notes.

| Image / arch group | Models (prefix, board) |
| --- | --- |
| Gen-1 · SC5000 | `SC5000` JP07, `SC5000M` JP08, `PRIMEGO` JP11 |
| Gen-1 · SC6000 | `SC6000` JP13, `SC6000M` JP14 |
| Gen-1 · PRIME 4 | `PRIME4` JC11, `PRIME2` JC16, `MIXSTREAMPRO` NH08 |
| Gen-2 · AZ0x (armhf) | `SCLIVE4` JP21, `SCLIVE2` JP20, `PRIME4PLUS` JC11S, `PRIMEGOPLUS` JP11S, `MIXSTREAMPROPLUS` NH08S, `MIXSTREAMPROGO` NH10 |
| Gen-2 · AZ04 (aarch64) | `PRIME4G2` JP24, `SYSTEMONE` RMZ2 |
