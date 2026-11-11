# EngineX dev loop tools

The standard loop for every change: **unit tests → sandbox screenshot (UI) → quick-gate → deck-push to a locked deck → smoke PASS → commit**. Full image builds and the three-group release gate are only for release candidates (`.claude/rules/release-workflow.md`). Every tool here runs on the build host as root.

| Tool | What it does |
| --- | --- |
| `deck-push` | Builds one component, backs up what it replaces, bind-mounts the new files on a dev deck, restarts only the affected service and runs the smoke check |
| `deck-push lock/unlock/status` | One owner per deck, with a purpose and an expiry |
| `quick-gate` | Runs one image group through the fast subset of `release-gate.sh` |
| `ui-sheet` | Makes a before/after sheet of the views from the twin sandbox; `deck-push ... ui` needs one |
| `fake-deck.sh`, `test_deck_push*` | A fake first-generation deck and the tests that use it |

## deck-push

```sh
tools/dev/deck-push lock deck-a --purpose "header row 2 layout" --ttl 2h
tools/dev/deck-push deck-a ui                # or hook | edisksd | go:stream-sync | go:sc5000-deemixd | go:rb-bridge | all
tools/dev/deck-push deck-a hook --commit 207dc55
tools/dev/deck-push deck-a all --dry-run     # builds and shows the plan; never contacts the deck
tools/dev/deck-push smoke deck-a             # the smoke check again, read-only
tools/dev/deck-push deck-a --rollback
tools/dev/deck-push unlock deck-a
tools/dev/deck-push status [deck-a]          # locks; with a deck name, also its dev mounts and last push
tools/dev/deck-push backups [deck-a]
```

The owner is `--owner`, else `DECK_PUSH_OWNER`, else `PASEO_AGENT_ID`. A push or rollback is refused (exit code 3) unless that owner holds the deck's lock. An expired lock counts as free. Only the owner can unlock, and this tool never breaks a lock that someone else holds.

| Component | Built like `build-release.sh` | Replaced on the deck | Restarted |
| --- | --- | --- | --- |
| `ui` | `build_packs.py` (tag `dev <commit>`) | `/data/enginex/ui/packs`, `/data/enginex/ui/icons` | `engine.service` |
| `hook` | `tools/ui-mod/build.sh` | `/etc/sc5000/libengine-ui-hook.so` | `engine.service` |
| `edisksd` | `tools/edisksd-hook/build.sh` | `/etc/sc5000/libedisksd-hook.so` | `edisksd.service` (it re-reads the drives) |
| `go:<daemon>` | `go build ./cmd/sc5000-suite` (armhf) | `/data/enginex/bin/sc5000-suite` | `<daemon>.service` only; other applets pick the new binary up at their next start |
| `all` | all of the above | all of the above | all five |

What a push does:

1. Builds from the working tree (default) or from a clean `git archive` of `--commit`. `--from DIR` pushes prebuilt artifacts instead.
2. Refuses a UI pack that has no `ui-sheet` record for its exact digest, unless you pass `--no-sandbox REASON`; the reason is logged.
3. Copies what the deck shows at each target into `<state_dir>/backups/<deck>/<time>-<component>/targets.tar` and appends a line to `backups/index.jsonl` (owner, commit, paths and sha256 before and after).
4. Uploads to `/run/enginex-dev` (tmpfs) and bind-mounts each file over its target. The files underneath are never written. The digest before the first push is kept in `/run/enginex-dev/orig`.
5. Restarts only the affected services, then polls the deck and prints `PASS`/`FAIL` with evidence:
   Engine up (`EMain`, restarted by this push); the UI hook's `applied 0x...` count equals `expect_applied`; the hook loaded the pack; no QML load errors or crashes in Engine's journal since the restart (the release gate's patterns); the configured daemons are `active`; the stream-sync API answers; the API version equals `/etc/sc5000/version.txt`; the deck's manifest commit and every pushed digest match.

`--rollback` lazily unmounts everything deck-push mounted (a restarted service may still map the pushed file), restarts the services those pushes restarted, checks that every target is back to its digest from before the first push, and runs the smoke check again. A reboot rolls back too, because `/run` and the mounts do not survive it.

deck-push and its deck side (`deck-agent.sh`, sent over SSH as the argument of `sh -c`, never installed) only touch the five targets above and `/run/enginex-dev`. They only restart these five units. They never reboot the deck, never run `az01-update` and never start a Factory Reset. First-generation decks only (armhf, `/etc/sc5000` hooks); a second-generation `/data` bundle has other paths.

### Decks

`cp tools/dev/decks.ini.example tools/dev/decks.ini` (git-ignored), or point `DECK_PUSH_CONFIG` at a copy. Each `[section]` is one deck. `ssh` is an alias from the build host's `~/.ssh/config`, with `HostName`, `User root`, `IdentityFile` and any `ProxyJump` defined there. No addresses or host names go into the repository. `state_dir` (default `/var/lib/enginex-dev`) holds the locks, backups and sandbox sheets for every agent on the build host. Keep it out of `/var/tmp` and `/tmp`, because the release cleanup empties those.

## quick-gate

```sh
tools/dev/quick-gate --group SC5000           # builds SC5000 (JP07) from this tree, then the quick gate
tools/dev/quick-gate --group PRIME4 --image <new .img> --version 5.3.0.4
```

This runs `build-release.sh` with `ONLY_GROUP=<group>` (one group of first-generation images, no bundles, no SSH key) and then `GATE_QUICK=1 release-gate.sh - <image>`. The gate keeps its static audit, the boot of the new rootfs on an empty `/data`, the services, the UI hook patch, DSP and QML counts, the render probe and the APIs. It skips the previous release, both online updates and the stale-QML-cache seed. The evidence is in `/var/tmp/quick-gate.*/gate/results.txt`.

## ui-sheet

```sh
tools/dev/ui-sheet --views 4deck,control-center,settings-about   # before = HEAD (HEAD~1 if the tree is clean)
tools/dev/ui-sheet --base origin/agent/integration-5304-20261111 --dry-run
```

It starts the twin sandbox with a private `SHADOW_TWIN_WORK`, renders each view at the base ref, redeploys this working tree, renders the views again and stops the twin. The sheet goes to `<state_dir>/sheets/<UI pack digest>.png` with a `.json` record. The twin is one per host: `ui-sheet` refuses while any twin runs, so announce your slot in dev-chat. It needs `/dev/dri` on the build host.

## Tests

```sh
python3 -m unittest discover -s tools/dev     # unit tests; the FakeDeck cases need root
bash tools/dev/test_deck_push_e2e.sh [evidence dir]   # one real build of every component against the fake deck
shellcheck -s sh tools/dev/deck-agent.sh tools/dev/fake-deck/{systemctl,journalctl,EMain,daemon,sc5000-suite}
shellcheck tools/dev/fake-deck.sh tools/dev/quick-gate tools/dev/test_deck_push_e2e.sh
```

`fake-deck.sh` is a chroot onto an overlay of the build host's `/` in its own mount, pid and network namespaces. Its `/data` and `/etc/sc5000` persist in its directory like flash. Its `/run` is tmpfs, so `fake-deck.sh reboot` drops every mount like a real reboot. ssh reaches it through `sshd -i` under `nsenter`, so it opens no port. `systemctl`, `journalctl` and Engine (`EMain`) are stand-ins; the service wrappers and the pushed suite are real.
