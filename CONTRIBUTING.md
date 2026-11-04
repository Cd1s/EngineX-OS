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

### Tests

Run on a Linux x86_64 host with Go, an armhf cross compiler and `qemu-arm`:

```sh
go vet ./... && go test -race ./...
bash tests/test_rootfs_overlay.sh            # bash, not sh
python3 -m unittest discover -s tools/az01-packer
python3 -m unittest discover -s tools/stems
# C hooks: build commands are in the header of each file
#   tools/ui-mod/test_dsp_smooth.c   (also with ASan/UBSan and TSan)
#   tools/ui-mod/test_stems_feature.c (armhf + qemu-arm)
#   tools/edisksd-hook/test_edisksd_hook.c
```

## Releasing a new version

Maintainers follow the checklist in [`.claude/rules/release-workflow.md`](./.claude/rules/release-workflow.md). In short, for version `<VERSION>`:

1. **Version file**: `rootfs-overlay/etc/sc5000/version.txt`.
2. **Three READMEs** (`README.md`, `README.zh-CN.md`, `README.zh-TW.md`): the hero image `alt`, the version in the intro and the performance table, and all 8 download links in "How to Install & Update":
   `https://github.com/Cd1s/EngineX-OS/releases/latest/download/<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img`
3. **Hero banners** `docs/assets/hero-banner*.svg`: only if they show a version (they do not today).
4. **Build** all 8 images and `SHA256SUMS.txt` with `tools/az01-packer/build-release.sh <VERSION> ...` from a clean checkout, then spot-check one image per group.
5. **Tag and Release** `v<VERSION>`: upload the 8 images and `SHA256SUMS.txt`, and put the SHA-256 table of all 8 models in the release notes.

| Image group | Models (prefix, board) |
| --- | --- |
| SC5000 | `SC5000` JP07, `SC5000M` JP08, `PRIMEGO` JP11 |
| SC6000 | `SC6000` JP13, `SC6000M` JP14 |
| PRIME 4 | `PRIME4` JC11, `PRIME2` JC16, `MIXSTREAMPRO` NH08 |
