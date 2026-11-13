# EngineX dev loop tools

The standard loop for every change: **unit tests → sandbox screenshot (UI) → quick-gate → deck-push to a locked deck → smoke PASS → commit**. Full image builds and the three-group release gate are only for release candidates (`.claude/rules/release-workflow.md`). Every tool here runs on the build host as root.

| Tool | What it does |
| --- | --- |
| `deck-push` | Builds one component, backs up what it replaces, bind-mounts the new files on a dev deck, restarts only the affected service and runs the smoke check |
| `deck-lock acquire/release/status/run`, `deck` | Shared on-deck leases and commands guarded by the lease |
| `quick-gate` | Runs one image group through the fast subset of `release-gate.sh` |
| `ui-sheet` | Makes a before/after sheet of the views from the twin sandbox; `deck-push ... ui` needs one |
| `fake-deck.sh`, `test_deck_push*` | A fake first-generation deck and the tests that use it |

## deck-push

```sh
export PATH="$PWD/tools/dev:$PATH"        # select this branch's deck helper
export DECK_PUSH_OWNER="$PASEO_AGENT_ID"   # a unique agent id, shared across its commands
tools/dev/deck-lock acquire left --purpose "header layout" --ttl 2h
tools/dev/deck-push left ui                # hook | edisksd | go:<daemon> | all
tools/dev/deck left -- uptime              # every deck command needs the lease
tools/dev/deck-push smoke left
tools/dev/deck-push left --rollback
tools/dev/deck-lock release left
tools/dev/deck-lock status                 # read-only; no lease needed
tools/dev/deck-lock run right left --purpose "sync pair" --wait -- ./pair-test.sh
tools/dev/deck-lock run left --purpose "read-only check" --ssh -- uptime
```

Acquire, deploy, test, roll back, release. Left and right can serve different agents;
a two-deck test takes **left then right**, whatever order the caller supplies.
If right is busy, an acquisition releases any lease it just took on left before
waiting (`--wait`) or refusing (exit 3). Existing leases owned by the caller are
borrowed, so a nested wrapper keeps them. `run` renews local command leases,
terminates the command on lease loss, and releases its own leases on exit or signal.
`deck` passes literal argv; use `sh -c '...'` explicitly for shell syntax.

The owner is `--owner`, else `DECK_PUSH_OWNER`, else `PASEO_AGENT_ID`.
`/run/enginex-dev-lock/owner` on each deck records agent id, host, start time,
purpose and expiry (1 h default, configurable with `--ttl`). `mkdir` serializes
updates and deck operations. An executing deck command holds the lease until it finishes.
A reboot clears the lease **and every development mount**: reacquire before
redeploying or testing. An expired idle lease may be acquired by another agent.
An untrappable kill during an operation can leave its mutex busy; fail closed,
check the process with the maintainer, and use an approved reboot to clear it.
Do not remove an active mutex or use raw SSH to bypass the lock.

`deck-push lock/unlock/status` remain compatible aliases. Smoke, rollback and
status with deck details also require ownership. `deck-push backups` is local;
`deck-push left all --dry-run` never contacts a deck.

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
3. Copies what the deck shows at each target into `<state_dir>/backups/<deck>/<time>-<component>/targets.tar` and uploads it with `agent-backup put` before changing anything (a returned backup id is mandatory), then appends a line to `backups/index.jsonl` (owner, commit, paths and sha256 before and after).
4. Uploads to `/run/enginex-dev` (tmpfs) and bind-mounts each file over its target. The files underneath are never written. The digest before the first push is kept in `/run/enginex-dev/orig`.
5. Restarts only the affected services, then polls the deck and prints `PASS`/`FAIL` with evidence:
   Engine up (`EMain`, restarted by this push); the UI hook's `applied 0x...` count equals `expect_applied`; the hook loaded the pack; no QML load errors or crashes in Engine's journal since the restart (the release gate's patterns); the configured daemons are `active`; the stream-sync API answers; the API version equals `/etc/sc5000/version.txt`; the deck's manifest commit and every pushed digest match.

`--rollback` lazily unmounts everything deck-push mounted (a restarted service may still map the pushed file), restarts the services those pushes restarted, checks that every target is back to its digest from before the first push, and runs the smoke check again. A reboot rolls back too, because `/run` and the mounts do not survive it.

deck-push and its deck side (`deck-agent.sh`, sent over SSH as the argument of `sh -c`, never installed) only touch the five targets above, `/run/enginex-dev` and `/run/enginex-dev-lock`. They only restart these five units. They never reboot the deck, never run `az01-update` and never start a Factory Reset. First-generation decks only (armhf, `/etc/sc5000` hooks); a second-generation `/data` bundle has other paths.

### Decks

Copy `tools/dev/decks.ini.example` to `~/.config/enginex-dev/decks.ini`, or set
`DECK_PUSH_CONFIG`. A git-ignored `tools/dev/decks.ini` also works. Use the canonical
`[left]` and `[right]` names. `ssh` is a local SSH alias; with `transport = ssm`
it is an exact vault alias discovered using `sshctl list` / `sshctl host list`.
Never commit actual aliases, addresses or credentials. The optional `ssh_options`
apply only to SSH transport; host-key verification stays enabled. `state_dir`
holds local backup copies and sandbox sheets, while locks always live on decks.
Use `deck-lock status` to inspect leases without acquiring them.

## quick-gate

```sh
tools/dev/quick-gate --group SC5000           # builds SC5000 (JP07) from this tree, then the quick gate
tools/dev/quick-gate --group PRIME4 --image <new .img> --version 5.3.0.4
```

This runs `build-release.sh` with `ONLY_GROUP=<group>` (one group of first-generation images, no bundles, no SSH key) and then `GATE_QUICK=1 release-gate.sh - <image>`. The gate keeps its static audit, the boot of the new rootfs on an empty `/data`, the services, the UI hook patch, DSP and QML counts, the render probe and the APIs. It skips the previous release, both online updates, the stale-QML-cache seed and the power-cut/second-boot cycle. The evidence is in `/var/tmp/quick-gate.*/gate/results.txt`.

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

Run these tests only on the authorized ARM build hosts; quick-gate needs the lab on the release-gate host. The sandbox creates two named fake decks for concurrent-agent tests and uses a fake backup-vault executable. It never connects to a real deck or changes the host `/tmp` mode or mount.
