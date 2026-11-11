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

## Development loop

Every change goes through the same loop. Full image builds (20 artifacts) and the three-group release gate are for release candidates only.

1. **Unit tests** for what you touched (the commands above).
2. **Sandbox screenshot** (UI changes): `tools/dev/ui-sheet` renders the views in the twin sandbox before and after your change and puts them on one sheet. `deck-push ... ui` refuses a UI pack that has no sheet.
3. **Quick gate**: `tools/dev/quick-gate --group SC5000` builds one image group and runs the fast subset of the release gate in a few minutes: static audit, new-version boot, rendered UI probe, hook edit count, no QML errors.
4. **Push to a locked dev deck**: `tools/dev/deck-push lock <deck> --purpose "..."`, then `tools/dev/deck-push <deck> ui|hook|edisksd|go:<daemon>|all`. It builds only that component and backs up what it replaces on the build host. It bind-mounts the new files from `/run`, so a reboot rolls back and `--rollback` does it without one. Then it restarts only the affected service and prints a smoke check.
5. **Smoke PASS, then commit.** Afterwards run `deck-push <deck> --rollback` and `deck-push unlock <deck>`.

Decks are listed in the git-ignored `tools/dev/decks.ini` (template: `tools/dev/decks.ini.example`). Never commit deck addresses. Details are in [`tools/dev/README.md`](./tools/dev/README.md).

## Releasing a new version

Maintainers follow the checklist in [`.claude/rules/release-workflow.md`](./.claude/rules/release-workflow.md). In short, for version `<VERSION>`:

1. **Version file**: `rootfs-overlay/etc/sc5000/version.txt`.
2. **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`): the hero image `alt`, the version in the intro and the performance table, and all 8 download links in "How to Install & Update":
   `https://github.com/Cd1s/EngineX-OS/releases/latest/download/<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`
3. **Hero banners** `docs/assets/hero-banner*.svg`: only if they show a version (they do not today).
4. **Build** all 8 images and `SHA256SUMS.txt` with `tools/az01-packer/build-release.sh <VERSION> <fw> none ...` from a clean checkout, then spot-check one image per group. Public images never carry an SSH key (`none`).
5. **Release gate** (mandatory, also for a new vendor base and for same-version re-releases): run `tools/az01-packer/release-gate.sh <previous release .img> <new .img> <VERSION> <new work dir>` as root on the build host, once per image group (JP07, JP13, JC11). It audits the new rootfs (unit dependencies, commands, tool paths, shared libraries), runs the previous release's online update to the new image, and then boots the new rootfs on the same `/data` and checks every core feature: services, UI hook counts, DSP guards, streaming gateway, Stems, Rekordbox, storage and the next online update. No release unless it exits 0.
6. **Tag and Release** `v<VERSION>`: upload the 8 images and `SHA256SUMS.txt`, and put the SHA-256 table of all 8 models in the release notes.

| Image group | Models (prefix, board) |
| --- | --- |
| SC5000 | `SC5000` JP07, `SC5000M` JP08, `PRIMEGO` JP11 |
| SC6000 | `SC6000` JP13, `SC6000M` JP14 |
| PRIME 4 | `PRIME4` JC11, `PRIME2` JC16, `MIXSTREAMPRO` NH08 |
