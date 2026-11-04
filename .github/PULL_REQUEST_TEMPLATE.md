## What and why / 改了什么、为什么

<!-- The problem on the deck, the root cause, and the fix. Link the issue: Fixes #123 -->

## How it was tested / 怎么验证的

<!-- Commands you ran and their result. Which deck(s) you tried it on, if any. -->

## Checklist / 自检清单

- [ ] **Zero audio xruns / 实时音频铁律**: nothing in the 96 kHz `SCHED_RR 49` audio thread blocks, waits on a lock, allocates memory or uses a large stack frame.
- [ ] **All 8 models / 全机型兼容**: the change works on `JP07` / `JP08` / `JP11` / `JP13` / `JP14` / `JC11` / `JC16` / `NH08`, or is limited to the models it is meant for.
- [ ] **Tests pass / 自动化测试全绿**: `go test -race ./...`, `bash tests/test_rootfs_overlay.sh`, the C hook tests (`tools/ui-mod/test_*.c`, `tools/edisksd-hook/test_edisksd_hook.c`) and `python3 -m unittest discover -s tools/az01-packer`.
- [ ] **No private data / 零隐私泄露**: no private keys, tokens, Deezer ARL, passwords, private domains or host names, and no copyrighted vendor binaries or firmware images.
- [ ] **Three READMEs in sync / 三语文档同步**: user-visible features or version changes are updated in `README.md`, `README.zh-CN.md` and `README.zh-TW.md`.
- [ ] **No destructive tests on a real deck / 不在实机做破坏性测试**: Factory Reset, `az01-update --erase-data` and blank-`/data` boots were only tested in a sandbox or VM.
