#!/usr/bin/env python3
"""
twin.py — Dual-deck SC5000 (JP07) sandbox for EngineX OS shadow-track development.

Runs two isolated SC5000 instances (A: role 13, 192.168.100.1; B: role 24, 192.168.100.2)
in separate mount + PID + network namespaces connected by a veth pair with tc netem
latency/jitter matching real SC5000 hardware (ping RTT min/avg/max ~ 1.8 / 4.2 / 23.8 ms).
Both instances run native armhf Engine + libengine-ui-hook.so + stream-sync with eglfs_kms
OpenGL rendering (1280x800) and a CLOCK_MONOTONIC-paced ALSA null audio clock.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import datetime
import json
import os
import re
import shutil
import subprocess
import time
import uuid
from pathlib import Path
from PIL import Image, ImageDraw, ImageFont

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
WORK = Path(os.environ.get("SHADOW_TWIN_WORK", "/var/tmp/shadow-twin"))
LAB = Path(os.environ.get("SHADOW_TWIN_LAB", "/root/build/sc5000-lab"))
FW_IMG = LAB / "fw-5.3.0/SC5000-5.3.0-Update.img"
QRC_DIR = LAB / "5.3.0/qrc"

DEFAULT_REF = "origin/agent/sandbox-main-20261110"
HW_REF = "origin/agent/shadow-grid-halfwave-20261110"

SIDES = ("A", "B")
SIDE_CFG = {
    "A": {
        "netns": "shadow-twin-a",
        "ip": "192.168.100.1",
        "peer_ip": "192.168.100.2",
        "role": "13",
        "deck": 1,
        "shadow_deck": 2,
        "player": "1",
        "track": "track-a.wav",
        "machine_id": "11111111111111111111111111111111",
    },
    "B": {
        "netns": "shadow-twin-b",
        "ip": "192.168.100.2",
        "peer_ip": "192.168.100.1",
        "role": "24",
        "deck": 2,
        "shadow_deck": 1,
        "player": "2",
        "track": "track-b.wav",
        "machine_id": "22222222222222222222222222222222",
    },
}

BOOTSTRAP_JS = r"""
(function() {
    try {
        if (PlanckGUI.views.outOfBoxUpdateScreenShowing) PlanckGUI.views.outOfBoxUpdateScreenShowing = false;
        if (PlanckGUI.views.onBoardingController) {
            PlanckGUI.views.onBoardingController.close();
            if (typeof PlanckGUI.views.onBoardingController.onIgnoreOnBoarding === "function")
                PlanckGUI.views.onBoardingController.onIgnoreOnBoarding();
        }
        if (PlanckGUI.views.prompts && typeof PlanckGUI.views.prompts.drop === "function")
            PlanckGUI.views.prompts.drop("StemsDownload");
        if (PlanckGUI.views.sourceScreenView && typeof PlanckGUI.views.sourceScreenView.close === "function")
            PlanckGUI.views.sourceScreenView.close();
        if (PlanckGUI.views.controlCenter) {
            if (typeof PlanckGUI.views.controlCenter.close === "function")
                PlanckGUI.views.controlCenter.close();
            PlanckGUI.views.controlCenter.dragPosition = 0.0;
        }
        var pClose = root.rtTr("/GUI/CloseDialogs"); if (pClose) pClose.state = true;
        var pWifi = root.rtTr("/GUI/ShowWifiView"); if (pWifi) pWifi.state = false;
        var pBrowse = root.rtTr("/GUI/ShowBrowseView"); if (pBrowse) pBrowse.state = false;
        var pPads = root.rtTr("/GUI/PadsViewEnabled"); if (pPads) pPads.state = false;
        var pFx = root.rtTr("/GUI/FxViewEnabled"); if (pFx) pFx.state = false;
        var win = root.parent || root;
        function dis(o) {
            if (!o) return;
            if ((o.objectName === "DismissButton" || o.objectName === "NextButton") && typeof o.clicked === "function") o.clicked();
            if (o.view && typeof o.view.onDismissed === "function") {
                if (o.notifier && o.notifier.close) o.notifier.close();
                o.view.onDismissed();
            }
            if (o.children) for (var i = 0; i < o.children.length; i++) dis(o.children[i]);
        }
        dis(win);
    } catch (e) {}
    if (root.scViewApply) {
        root.scViewApply({
            pads: false,
            fx: false,
            library: false,
            deckCount: "4 Decks",
            waveformCount: "4 Waveforms",
            waveformOrientation: "Horizontal"
        });
    }
    var top = root; while (top.parent) top = top.parent;
    var q = [top], out = [], n = 0;
    while (q.length && n < 60000) {
        var o = q.pop(); n++;
        if (o.objectName && String(o.objectName).indexOf("EngineX_Shadow_") === 0 && o.objectName !== "EngineX_Shadow_Content") {
            var mv = null;
            for (var i = 0; i < o.children.length; i++)
                if (o.children[i].objectName === "EngineX_Shadow_Content") mv = o.children[i];
            out.push({d: o, moving: mv});
        }
        var k = o.children;
        if (k) for (var j = 0; j < k.length; j++) q.push(k[j]);
    }
    root.rtShadowItems = out;
    return JSON.stringify({
        role: root.scFourDeckRole,
        displayOnly: root.scShadowDisplayOnly,
        shadowItems: out.length
    });
})()
"""

QML_RT_HELPERS = """
\tfunction rtTr(path) { const p = Planck.getProperty(path); return p ? p.translator : null }
\tproperty var rtShadowItems: []
\tproperty var rtPressQueue: []
\tfunction rtPress(path) { rtPressQueue = rtPressQueue.concat([{path:path,step:0}]); rtPressTimer.restart() }
\tTimer { id: rtPressTimer; interval:120; onTriggered: {
\t\tconst next=[]; for(const e of root.rtPressQueue) {const t=root.rtTr(e.path); if(!t)continue; t.state=e.step===1; if(e.step<2)next.push({path:e.path,step:e.step+1})}
\t\troot.rtPressQueue=next; if(next.length)restart()
\t}}
"""

PERF_JS = r"""
(function(){
var start=START, playing=PLAYING, d=DECK, label=LABEL;
var timer=Qt.createQmlObject('import QtQml;Timer{interval:16;repeat:true;running:false;property var tick;onTriggered:tick()}',root,'rt2Window');
var intervals=[], swaps=[], states=[], prev=0, swapPrev=0, lastState=0, started=false, warm=false, masterSent=false, syncSent=false;
var conn=Qt.createQmlObject('import QtQml;Connections{target:null;ignoreUnknownSignals:true;property var cb;function onFrameSwapped(){if(cb)cb()}}',root,'rt2Swap');
var canaryC=Qt.createQmlObject('import QtQml;Component{QtObject{property var cb;Component.onDestruction:if(cb)cb()}}',root,'rt2Canary'),gcs=[],alive=true;
function canary(){var o=canaryC.createObject(null);o.cb=function(){if(!alive)return;gcs.push(Date.now());canary()}}
conn.target=planckQuickView;
conn.cb=function(){var now=Date.now();if(!started)return;if(swapPrev)swaps.push(now-swapPrev);swapPrev=now};
function g(n,p){var t=root.rtTr('/Engine/Deck'+n+p);return t}
function pause(){for(var n=1;n<=4;n++){var t=g(n,'/Play');if(t)t.state=false}}
timer.tick=function(){
 var now=Date.now();
 if(playing && now>=start-3000 && !warm){warm=true;g(d,'/Track/SetSongPosition').value=0;g(d,'/Play').state=true}
 if(playing && now>=start-2000 && !masterSent){masterSent=true;if(d===1)root.rtPress('/Engine/Deck1/RequestMaster')}
 if(playing && now>=start-1000 && !syncSent){syncSent=true;if(d===2)root.rtPress('/Engine/Deck2/Sync')}
 if(now<start)return;
 if(!started){started=true;prev=now;console.warn('RT2BEGIN '+JSON.stringify([label,now,d,playing]));return}
 intervals.push(now-prev);prev=now;
 if(now-lastState>=1000){lastState=now;var row=[now];for(var n=1;n<=4;n++){var p=g(n,'/PlayState'),ld=g(n,'/Track/SongLoaded'),pos=g(n,'/Track/TrackPosition'),b=g(n,'/CurrentBPM'),ma=g(n,'/DeckIsMaster'),sy=g(n,'/SyncMode');row.push([n,p?+p.state:null,ld?+ld.state:null,pos?pos.unnormalized:null,b?b.unnormalized:null,ma?ma.state:null,sy?sy.string:null])}states.push(row)}
 if(now-start>=DURATION_MS){alive=false;pause();timer.stop();conn.target=null;console.warn('RT2GUI '+JSON.stringify({label:label,start:start,end:now,timer:intervals,swap:swaps,states:states,gcs:gcs}));conn.destroy();timer.destroy()}
};canary();pause();timer.start();return 'window-armed '+label;
})()
"""


def sh(cmd: str, check: bool = True, capture: bool = False, timeout: int = 180) -> subprocess.CompletedProcess:
    full = f". /etc/profile.d/buildtools.sh 2>/dev/null || true; {cmd}"
    return subprocess.run(
        ["bash", "-c", full],
        check=check,
        capture_output=capture,
        text=True,
        timeout=timeout,
    )


def git_out(args: str, cwd: Path = REPO, check: bool = True) -> str:
    r = sh(f'git -C "{cwd}" {args}', check=check, capture=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def classify_line(ref: str, full_commit: str) -> tuple[str, str]:
    clean = ref.removeprefix("origin/").removeprefix("refs/heads/").removeprefix("refs/remotes/origin/")
    if "sandbox-main" in clean:
        return clean, "sandbox-main"
    if "shadow-grid-halfwave" in clean:
        return clean, "shadow-grid-halfwave"
    sbx_head = git_out(f"rev-parse {DEFAULT_REF}", check=False)
    hw_head = git_out(f"rev-parse {HW_REF}", check=False)
    if full_commit and full_commit == sbx_head and clean == full_commit[: len(clean)]:
        return "agent/sandbox-main-20261110", "sandbox-main"
    if full_commit and full_commit == hw_head and clean == full_commit[: len(clean)]:
        return "agent/shadow-grid-halfwave-20261110", "shadow-grid-halfwave"
    slug = re.sub(r"[^a-zA-Z0-9]+", "-", clean).strip("-").lower()
    if len(slug) > 24:
        slug = slug[:24].rstrip("-")
    return clean, (slug or "detached")


def prepare_build_source(
    ref: str | None = None,
    fetch: bool = False,
    local: bool = False,
) -> tuple[Path, dict]:
    WORK.mkdir(parents=True, exist_ok=True)
    ver_file = WORK / "build-version.json"

    if local:
        branch = git_out("rev-parse --abbrev-ref HEAD", REPO, check=False) or "local"
        full_sha = git_out("rev-parse HEAD", REPO)
        short_sha = full_sha[:7]
        dirty = bool(git_out("status --porcelain --untracked-files=no", REPO, check=False))
        subj = git_out("log -1 --format=%s HEAD", REPO)
        line_name, line_slug = classify_line(branch, full_sha)
        info = {
            "line": f"{line_name}{'-dirty' if dirty else ''}",
            "line_slug": f"{line_slug}{'-dirty' if dirty else ''}",
            "requested_ref": "local",
            "resolved_ref": branch,
            "commit": f"{short_sha}{'-dirty' if dirty else ''}",
            "full_commit": full_sha,
            "subject": subj,
            "source_dir": str(REPO),
            "updated_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        }
        ver_file.write_text(json.dumps(info, indent=2))
        return REPO, info

    if fetch:
        print("==> Fetching latest commits from origin...", flush=True)
        sh(f'git -C "{REPO}" fetch origin --prune')

    if ref is None:
        if ver_file.is_file():
            prev = json.loads(ver_file.read_text())
            ref = prev.get("requested_ref") or DEFAULT_REF
            if ref == "local":
                ref = DEFAULT_REF
        else:
            ref = DEFAULT_REF

    # Prefer origin/<ref> when a branch name like agent/sandbox-main-20261110 is given
    resolved_ref = ref
    if not ref.startswith("origin/"):
        cand = f"origin/{ref}"
        if git_out(f"rev-parse --verify -q {cand}", REPO, check=False):
            resolved_ref = cand

    full_sha = git_out(f"rev-parse --verify {resolved_ref}", REPO)
    short_sha = full_sha[:7]
    subj = git_out(f"log -1 --format=%s {full_sha}", REPO)
    line_name, line_slug = classify_line(ref, full_sha)

    src_dir = WORK / "src"
    if not (src_dir / ".git").exists():
        sh(f'git -C "{REPO}" worktree prune', check=False)
        shutil.rmtree(src_dir, ignore_errors=True)
        sh(f'git -C "{REPO}" worktree add --force --detach "{src_dir}" {full_sha}')
    else:
        cur_sha = git_out("rev-parse HEAD", src_dir, check=False)
        if cur_sha != full_sha:
            sh(f'git -C "{src_dir}" checkout --force --detach {full_sha}')
            sh(f'git -C "{src_dir}" clean -fd')

    info = {
        "line": line_name,
        "line_slug": line_slug,
        "requested_ref": ref,
        "resolved_ref": resolved_ref,
        "commit": short_sha,
        "full_commit": full_sha,
        "subject": subj,
        "source_dir": str(src_dir),
        "updated_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }
    ver_file.write_text(json.dumps(info, indent=2))
    return src_dir, info


def ensure_base_artifacts(
    src_dir: Path,
    ver_info: dict,
    build_suite: bool = True,
    build_packs: bool = True,
    build_hook: bool = False,
) -> None:
    t0 = time.time()
    (WORK / "base/ro").mkdir(parents=True, exist_ok=True)
    (WORK / "bin").mkdir(parents=True, exist_ok=True)
    (WORK / "music").mkdir(parents=True, exist_ok=True)
    (WORK / "out").mkdir(parents=True, exist_ok=True)

    built_commit_file = WORK / "bin/built-commit.txt"
    prev_built = built_commit_file.read_text().strip() if built_commit_file.is_file() else ""
    commit_changed = prev_built != f"{ver_info['full_commit']}:{src_dir}"

    ext2 = WORK / "base/image/rootfs.ext2"
    if not ext2.is_file():
        print("==> Unpacking official SC5000 5.3.0 rootfs.ext2...", flush=True)
        shutil.rmtree(WORK / "base/image", ignore_errors=True)
        sh(f'python3 "{src_dir}/tools/az01-packer/az01_packer.py" unpack "{FW_IMG}" --output "{WORK}/base/image" > "{WORK}/base/unpack.json"')
        for p in (WORK / "base/image").glob("*.payload"):
            p.unlink(missing_ok=True)

    ro_mnt = WORK / "base/ro"
    if subprocess.run(["mountpoint", "-q", str(ro_mnt)]).returncode != 0:
        sh(f'mount -o loop,ro "{ext2}" "{ro_mnt}"')

    fixture_so = WORK / "bin/alsa-null-fixture.so"
    fixture_c = HERE / "alsa_null_fixture.c"
    if not fixture_so.is_file() or fixture_c.stat().st_mtime > fixture_so.stat().st_mtime:
        print("==> Compiling ALSA null + monotonic clock + vsync fixture...", flush=True)
        sh(f'arm-linux-gnueabihf-gcc -O2 -shared -fPIC -Wall -Wextra "{fixture_c}" -o "{fixture_so}" -ldl')

    hook_so = src_dir / "rootfs-overlay/etc/sc5000/libengine-ui-hook.so"
    if build_hook or commit_changed or not hook_so.is_file():
        print(f"==> Building libengine-ui-hook.so ({ver_info['line']} @ {ver_info['commit']})...", flush=True)
        sh(f'cd "{src_dir}" && sh tools/ui-mod/build.sh')

    suite_bin = WORK / "bin/sc5000-suite"
    if build_suite or commit_changed or not suite_bin.is_file():
        print(f"==> Building sc5000-suite (armhf) ({ver_info['line']} @ {ver_info['commit']})...", flush=True)
        sh(f'cd "{src_dir}" && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o "{suite_bin}" ./cmd/sc5000-suite')

    packs_dir = WORK / "packs"
    if build_packs or commit_changed or not (packs_dir / "en").is_dir():
        print(f"==> Building UI packs (--dev) ({ver_info['line']} @ {ver_info['commit']})...", flush=True)
        shutil.rmtree(packs_dir, ignore_errors=True)
        packs_dir.mkdir(parents=True, exist_ok=True)
        sh(f'python3 "{src_dir}/tools/ui-mod/build_packs.py" --dev --qrc "{QRC_DIR}" --out "{packs_dir}" > "{WORK}/packs.log"')
        for em in packs_dir.glob("*/Skins/Embedded/EmbeddedMain.qml"):
            txt = em.read_text()
            if "function rtTr(" not in txt:
                txt = txt.replace("id: root", "id: root" + QML_RT_HELPERS, 1)
            txt = txt.replace(
                "interval: 500; running: true; repeat: true\n\t\tproperty string last:",
                "interval: 100; running: true; repeat: true\n\t\tproperty string last:",
                1,
            )
            em.write_text(txt)

    built_commit_file.write_text(f"{ver_info['full_commit']}:{src_dir}\n")

    if not (WORK / "music/track-a.wav").is_file() or not (WORK / "music/track-b.wav").is_file():
        print("==> Synthesizing test tracks via internal/testaudio...", flush=True)
        sh(f'cd "{REPO}" && go run ./tools/shadow-twin/gen_testaudio.go --out "{WORK}/music" --seconds 90')

    print(
        f"==> Artifacts ready in {time.time() - t0:.1f}s "
        f"[line={ver_info['line']} commit={ver_info['commit']} ({ver_info['subject']})]",
        flush=True,
    )


def setup_network(delay_ms: float = 2.0, jitter_ms: float = 1.1) -> None:
    for side, cfg in SIDE_CFG.items():
        ns = cfg["netns"]
        sh(f"ip netns del {ns} 2>/dev/null || true", check=False)
        sh(f"ip netns add {ns}")
        sh(f"ip -n {ns} link set lo up")

    sh("ip link add eth0 netns shadow-twin-a type veth peer name eth0 netns shadow-twin-b")
    for side, cfg in SIDE_CFG.items():
        ns = cfg["netns"]
        ip = cfg["ip"]
        peer = cfg["peer_ip"]
        sh(f"ip -n {ns} addr add {ip}/24 brd 192.168.100.255 dev eth0")
        sh(f"ip -n {ns} link set eth0 up")
        sh(f"ip -n {ns} route add default via {peer} dev eth0")
        # Match real SC5000 inter-deck ping (min/avg/max = 1.815 / 4.201 / 23.819 ms)
        sh(f"tc -n {ns} qdisc replace dev eth0 root netem delay {delay_ms}ms {jitter_ms}ms distribution paretonormal")


def write_instance_configs(side: str, mode: str, src_dir: Path, ver_info: dict) -> None:
    cfg = SIDE_CFG[side]
    sdir = WORK / side
    ui_dir = sdir / "data/enginex/ui"
    ui_dir.mkdir(parents=True, exist_ok=True)
    (sdir / "data/music").mkdir(parents=True, exist_ok=True)
    (sdir / "data/AppDataUser/AIR Music Technology/Engine/State").mkdir(parents=True, exist_ok=True)
    (sdir / "data/inMusicTempFiles").mkdir(parents=True, exist_ok=True)
    (sdir / "logs").mkdir(parents=True, exist_ok=True)
    (sdir / "tmp").mkdir(parents=True, exist_ok=True)
    (sdir / "run").mkdir(parents=True, exist_ok=True)

    sc_link = sdir / "data/sc5000-custom"
    if not sc_link.exists() and not sc_link.is_symlink():
        sc_link.symlink_to("enginex")

    for wav in (WORK / "music").glob("*.wav"):
        dst = sdir / "data/music" / wav.name
        if not dst.exists() or wav.stat().st_mtime > dst.stat().st_mtime or wav.stat().st_size != dst.stat().st_size:
            shutil.copy2(wav, dst)

    # The forced modes keep the key every line reads; auto is the shadow_mode default.
    shadow = "shadow_mode=auto\n" if mode == "auto" else f"shadow_legacy_engine={'1' if mode == 'engine' else '0'}\n"
    settings = (
        f"lang=en\n"
        f"four_deck_waveform=1\n"
        f"four_deck_role={cfg['role']}\n"
        f"{shadow}"
        f"debug_telemetry=1\n"
    )
    (ui_dir / "settings.conf").write_text(settings)

    views = {
        "active": 1,
        "views": {
            "1": {
                "pads": False,
                "fx": False,
                "library": False,
                "deckCount": "4 Decks",
                "waveformCount": "4 Waveforms",
                "waveformOrientation": "Horizontal",
            },
            "2": {
                "pads": False,
                "fx": False,
                "library": False,
                "deckCount": "2 Decks",
                "waveformCount": "2 Waveforms",
                "waveformOrientation": "Horizontal",
            },
            "3": {
                "pads": False,
                "fx": False,
                "library": False,
                "deckCount": "4 Decks",
                "waveformCount": "4 Waveforms",
                "waveformOrientation": "Vertical",
            },
        },
    }
    (ui_dir / "views.json").write_text(json.dumps(views, indent=2))

    # Sync UI packs and icons into /data/enginex/ui/
    dst_pack = ui_dir / "packs/en"
    shutil.rmtree(dst_pack, ignore_errors=True)
    dst_pack.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(WORK / "packs/en", dst_pack)
    if (WORK / "packs/icons").is_dir():
        dst_icons = ui_dir / "icons"
        shutil.rmtree(dst_icons, ignore_errors=True)
        shutil.copytree(WORK / "packs/icons", dst_icons)

    # Clear stale QML disk cache and crash sentinels
    shutil.rmtree(sdir / "data/AppDataUser/AIR Music Technology/Engine/.cache/qml", ignore_errors=True)
    for sentinel in ("safe-mode", "boot-count", "boot-token", "last-crash", "running-sentinel"):
        (ui_dir / sentinel).unlink(missing_ok=True)

    inst_ver = dict(ver_info)
    inst_ver["side"] = side
    inst_ver["role"] = cfg["role"]
    inst_ver["mode"] = mode
    (sdir / "version.json").write_text(json.dumps(inst_ver, indent=2))


def start_namespace_holder(side: str, src_dir: Path) -> int:
    cfg = SIDE_CFG[side]
    sdir = WORK / side
    for d in ("root", "upper", "work", "data", "tmp", "run", "logs"):
        (sdir / d).mkdir(parents=True, exist_ok=True)

    screen_cfg = {
        "device": "/dev/dri/card0",
        "headless": "1280x800",
        "hwcursor": False,
    }
    (sdir / "ScreenConfiguration.json").write_text(json.dumps(screen_cfg, indent=2))
    (sdir / "interrupts").write_text("           CPU0\n 41:        100     GICv2  32 Level     ttyS0\n")

    holder_script = sdir / "ns-holder.sh"
    holder_script.write_text(f"""#!/bin/bash
set -euo pipefail
SDIR="{sdir}"
R="$SDIR/root"
RO="{WORK}/base/ro"
OV="{src_dir}/rootfs-overlay"

mount --make-rprivate /
mount -t overlay overlay -o "lowerdir=$OV:$RO,upperdir=$SDIR/upper,workdir=$SDIR/work" "$R"

mount --bind "$SDIR/tmp" "$R/tmp"
mount --bind "$SDIR/run" "$R/run"
mount --bind "$SDIR/data" "$R/data"
mount -t tmpfs tmpfs "$R/dev"
mount -t tmpfs tmpfs "$R/sys"
mount -t tmpfs tmpfs "$R/var"

mkdir -p "$R/var/volatile/tmp" "$R/var/volatile/log" "$R/var/volatile/cache" "$R/var/lib/dbus"
ln -sf volatile/tmp "$R/var/tmp"
ln -sf volatile/log "$R/var/log"
ln -sf ../run "$R/var/run"
ln -sf ../run/lock "$R/var/lock"
printf "%s\\n" "{cfg['machine_id']}" > "$R/var/lib/dbus/machine-id"

mkdir -p "$R/dev/dri" "$R/dev/shm" "$R/dev/pts"
for n in null zero full random urandom; do
    touch "$R/dev/$n"
    mount --bind "/dev/$n" "$R/dev/$n"
done
mount --bind /dev/dri "$R/dev/dri"

if [ -d /usr/lib/arm-linux-gnueabihf/dri ]; then
    mkdir -p "$R/usr/lib/arm-linux-gnueabihf" "$R/usr/share/glvnd" "$R/usr/share/drirc.d"
    mount --bind /usr/lib/arm-linux-gnueabihf "$R/usr/lib/arm-linux-gnueabihf"
    mount -o remount,bind,ro "$R/usr/lib/arm-linux-gnueabihf"
    [ -d /usr/share/glvnd ] && mount --bind /usr/share/glvnd "$R/usr/share/glvnd" && mount -o remount,bind,ro "$R/usr/share/glvnd"
    [ -d /usr/share/drirc.d ] && mount --bind /usr/share/drirc.d "$R/usr/share/drirc.d" && mount -o remount,bind,ro "$R/usr/share/drirc.d"
    mount --bind /usr/lib/arm-linux-gnueabihf/ld-linux-armhf.so.3 "$R/lib/ld-linux-armhf.so.3"
fi

for d in class devices bus; do
    mkdir -p "$R/sys/$d"
    mount --bind "/sys/$d" "$R/sys/$d"
    mount -o remount,bind,ro "$R/sys/$d"
done
mkdir -p "$R/sys/firmware/devicetree/base"
printf JP07 > "$R/sys/firmware/devicetree/base/inmusic,product-code"
printf 'inmusic,jp07\\0rockchip,rk3288\\0' > "$R/sys/firmware/devicetree/base/compatible"

mount -t proc proc "$R/proc"
mount --bind "$SDIR/interrupts" "$R/proc/interrupts"
mount -t tmpfs tmpfs "$R/proc/irq"
mkdir -p "$R/proc/irq/41"
printf 'f\\n' > "$R/proc/irq/41/smp_affinity"
printf '0-3\\n' > "$R/proc/irq/41/smp_affinity_list"

mount --bind "$SDIR/ScreenConfiguration.json" "$R/usr/Engine/ScreenConfiguration/Default/ScreenConfiguration.json"
if [ -f "$R/usr/Engine/ScreenConfiguration/JP07/ScreenConfiguration.json" ]; then
    mount --bind "$SDIR/ScreenConfiguration.json" "$R/usr/Engine/ScreenConfiguration/JP07/ScreenConfiguration.json"
fi

# Shim destructive commands inside sandbox
mkdir -p "$R/run/gate"
printf '#!/bin/sh\\necho "SHIM $0 $*" >> /tmp/gate-shim.log\\n' > "$R/run/gate/record"
chmod 755 "$R/run/gate/record"
for p in /usr/sbin/kexec /usr/bin/systemctl /sbin/reboot /usr/Engine/Scripts/encrypt-fs.sh /usr/sbin/az01-update; do
    t=$(chroot "$R" /bin/readlink -f "$p" 2>/dev/null || true)
    [ -n "$t" ] && [ -e "$R$t" ] && mount --bind "$R/run/gate/record" "$R$t"
done

mkdir -p "$R/run/dbus"
chroot "$R" /usr/bin/dbus-daemon --system --fork --nopidfile

echo "$$" > "$SDIR/ready"
trap 'exit 0' TERM INT
while :; do
    sleep 3600 &
    wait $! || true
done
""")
    holder_script.chmod(0o755)
    (sdir / "ready").unlink(missing_ok=True)

    proc = subprocess.Popen(
        ["ip", "netns", "exec", cfg["netns"], "unshare", "-m", "-p", "-f", "--kill-child", str(holder_script)],
        stdout=open(sdir / "logs/holder.log", "w"),
        stderr=subprocess.STDOUT,
    )
    (sdir / "sup.pid").write_text(str(proc.pid))

    deadline = time.time() + 15.0
    while time.time() < deadline:
        if (sdir / "ready").is_file():
            children = sh(f"pgrep -P {proc.pid}", check=False, capture=True).stdout.split()
            if children:
                init_pid = int(children[0])
                (sdir / "init.pid").write_text(str(init_pid))
                return init_pid
        time.sleep(0.05)
    raise RuntimeError(f"Instance {side} namespace holder failed to start")


def init_pid_of(side: str) -> int:
    p = WORK / side / "init.pid"
    if not p.is_file():
        raise RuntimeError(f"Instance {side} is not running (no {p})")
    return int(p.read_text().strip())


def ns_chroot_cmd(side: str, inner_cmd: str) -> list[str]:
    pid = init_pid_of(side)
    rdir = WORK / side / "root"
    return [
        "nsenter", "-t", str(pid), "-m", "-n", "-p", "--",
        "chroot", str(rdir),
        "/usr/bin/env", "-i",
        "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "HOME=/tmp/home",
        "sh", "-c", inner_cmd,
    ]


def start_services(side: str, src_dir: Path | None = None) -> None:
    sdir = WORK / side
    # Stop any existing Engine or stream-sync in this namespace first
    subprocess.run(
        ns_chroot_cmd(
            side,
            "pkill -TERM -f '^/usr/Engine/Engine' 2>/dev/null || true; "
            "pkill -TERM -f '/tmp/stream-sync|shadow-twin-sync|sc5000-suite' 2>/dev/null || true",
        ),
        check=False,
    )
    time.sleep(0.3)
    subprocess.run(
        ns_chroot_cmd(
            side,
            "pkill -KILL -f '^/usr/Engine/Engine' 2>/dev/null || true; "
            "pkill -KILL -f '/tmp/stream-sync|shadow-twin-sync|sc5000-suite' 2>/dev/null || true",
        ),
        check=False,
    )

    # Reset Engine state/library and inMusicTempFiles on restart so stream-sync cleanup
    # never invalidates m.db entries and Engine starts with a consistent database UUID.
    shutil.rmtree(sdir / "data/AppDataUser", ignore_errors=True)
    shutil.rmtree(sdir / "data/.config", ignore_errors=True)
    shutil.rmtree(sdir / "data/inMusicTempFiles", ignore_errors=True)
    shutil.rmtree(sdir / "data/sc5000-custom/shadow", ignore_errors=True)
    (sdir / "data/AppDataUser/AIR Music Technology/Engine/State").mkdir(parents=True, exist_ok=True)
    (sdir / "data/inMusicTempFiles").mkdir(parents=True, exist_ok=True)

    # Sync /etc/sc5000 overlay files (libengine-ui-hook.so, engine-ui-guard, etc.) into the live chroot
    if src_dir is not None and (src_dir / "rootfs-overlay/etc/sc5000").is_dir():
        for f in (src_dir / "rootfs-overlay/etc/sc5000").iterdir():
            if f.is_file():
                dst = sdir / "tmp" / f.name
                shutil.copy2(f, dst)
                subprocess.run(
                    ns_chroot_cmd(side, f"cp -f /tmp/{f.name} /etc/sc5000/{f.name} && chmod 755 /etc/sc5000/{f.name}"),
                    check=False,
                )
                dst.unlink(missing_ok=True)

    # Install latest sc5000-suite (as shadow-twin-sync + stream-sync symlink) and alsa-null-fixture.so
    for f in ("shadow-twin-sync", "stream-sync", "alsa-null-fixture.so", "alsa-clock.txt"):
        (sdir / "tmp" / f).unlink(missing_ok=True)
    shutil.copy2(WORK / "bin/sc5000-suite", sdir / "tmp/shadow-twin-sync")
    (sdir / "tmp/stream-sync").symlink_to("shadow-twin-sync")
    shutil.copy2(WORK / "bin/alsa-null-fixture.so", sdir / "tmp/alsa-null-fixture.so")
    (sdir / "tmp/home").mkdir(parents=True, exist_ok=True)
    (sdir / "tmp/ui-eval.js").write_text("noop\nvoid 0;\n")
    (sdir / "tmp/engine_runguard.lock").unlink(missing_ok=True)

    # Run engine-ui-guard pre (writes /run/sc5000-assignments/role, StreamingAccounts.json, etc.)
    subprocess.run(ns_chroot_cmd(side, "/etc/sc5000/engine-ui-guard pre"), check=True)

    # Start stream-sync (--deemixd="" avoids spawning deemixd; --disable-guard avoids SCHED_IDLE starvation under software GL)
    sync_log = open(sdir / "logs/stream-sync.log", "w")
    subprocess.Popen(
        ns_chroot_cmd(side, "exec /tmp/stream-sync --deemixd='' --disable-guard --listen :8080"),
        stdout=sync_log,
        stderr=subprocess.STDOUT,
    )

    cfg = SIDE_CFG[side]
    for _ in range(50):
        r = subprocess.run(
            ["ip", "netns", "exec", cfg["netns"], "curl", "-sf", "http://127.0.0.1:8080/v1/sc5000/ui"],
            capture_output=True,
        )
        if r.returncode == 0:
            break
        time.sleep(0.1)
    else:
        raise RuntimeError(f"stream-sync on {side} failed to listen on :8080")

    eng_log = open(sdir / "logs/engine.log", "w")
    eng_cmd = (
        "exec /usr/bin/env "
        "HOME=/tmp/home "
        "LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib:/usr/qt/lib "
        "LD_PRELOAD=/tmp/alsa-null-fixture.so:/etc/sc5000/libengine-ui-hook.so "
        "SBX_CARD=JP07 SBX_OUT=4 SBX_IN=2 SBX_CLOCK_HZ=48000 SBX_HEADLESS_VSYNC=1 "
        "QT_QPA_PLATFORM=eglfs "
        "QT_QPA_EGLFS_INTEGRATION=eglfs_kms "
        "LIBGL_ALWAYS_SOFTWARE=1 "
        "GALLIUM_DRIVER=llvmpipe "
        "LP_NUM_THREADS=2 "
        "QML_DISABLE_DISK_CACHE=1 "
        "QML_XHR_ALLOW_FILE_READ=1 "
        "MALLOC_ARENA_MAX=1 "
        "/usr/Engine/Engine -d0"
    )
    subprocess.Popen(
        ns_chroot_cmd(side, eng_cmd),
        stdout=eng_log,
        stderr=subprocess.STDOUT,
    )


def clean_log_rows(lines: list[str]) -> list[str]:
    rows = []
    for line in lines:
        if "[EMain" in line or line.startswith("SCEVAL "):
            s_line = re.sub(r"^.*?\[W\]\s*", "", line)
            s_line = re.sub(r"\s+(?:\[\d{4}-[^\]]+\]\s+)?\[EMain.*$", "", s_line)
            rows.append(s_line.strip())
    return rows


def ev(side: str, js: str, name: str = "eval", timeout: float = 12.0) -> str:
    sdir = WORK / side
    log_path = sdir / "logs/engine.log"
    ident = f"tw-{name}-{uuid.uuid4().hex[:8]}"
    (sdir / "tmp/ui-eval.js").write_text(f"{ident}\n{js}\n")
    deadline = time.time() + timeout
    needle = f"SCEVAL {ident}"
    while time.time() < deadline:
        if log_path.is_file():
            text = log_path.read_text(errors="replace")
            idx = text.find(needle)
            if idx >= 0:
                time.sleep(0.05)
                text = log_path.read_text(errors="replace")
                rows = clean_log_rows(text[idx:].splitlines())
                if rows:
                    head = re.sub(r"^SCEVAL \S+ =\s*", "", rows[0])
                    if " ERR " in rows[0]:
                        raise RuntimeError(f"{side} eval error: {rows[0]}")
                    return head
        time.sleep(0.08)
    raise TimeoutError(f"{side}: timed out waiting for SCEVAL {ident}")


def evj(side: str, js: str, name: str = "evalj", timeout: float = 12.0):
    sdir = WORK / side
    log_path = sdir / "logs/engine.log"
    ident = f"tw-{name}-{uuid.uuid4().hex[:8]}"
    (sdir / "tmp/ui-eval.js").write_text(f"{ident}\n{js}\n")
    deadline = time.time() + timeout
    needle = f"SCEVAL {ident}"
    while time.time() < deadline:
        if log_path.is_file():
            text = log_path.read_text(errors="replace")
            idx = text.find(needle)
            if idx >= 0:
                time.sleep(0.05)
                text = log_path.read_text(errors="replace")
                rows = clean_log_rows(text[idx:].splitlines())
                if rows:
                    head = re.sub(r"^SCEVAL \S+ =\s*", "", rows[0])
                    if " ERR " in rows[0]:
                        raise RuntimeError(f"{side} eval error: {rows[0]}")
                    buf = head
                    for j in range(1, len(rows) + 1):
                        try:
                            return json.loads(buf)
                        except json.JSONDecodeError:
                            if j < len(rows):
                                buf += rows[j]
        time.sleep(0.08)
    raise TimeoutError(f"{side}: timed out waiting for JSON SCEVAL {ident}")


def wait_engine_ready(side: str, timeout: float = 55.0) -> dict:
    sdir = WORK / side
    log_path = sdir / "logs/engine.log"
    deadline = time.time() + timeout
    while time.time() < deadline:
        if log_path.is_file():
            txt = log_path.read_text(errors="replace")
            if "SchedulerManager:" in txt:
                break
        time.sleep(0.2)
    else:
        raise TimeoutError(f"{side}: Engine did not finish loading UI packs within {timeout}s")

    while time.time() < deadline:
        try:
            res = evj(side, BOOTSTRAP_JS, name="boot", timeout=4.0)
            if res.get("role"):
                time.sleep(1.5)
                res = evj(side, BOOTSTRAP_JS, name="boot2", timeout=4.0)
                return res
        except Exception:
            time.sleep(0.4)
    raise TimeoutError(f"{side}: Engine QML bootstrap timed out")


def both(fn, sides=SIDES):
    with concurrent.futures.ThreadPoolExecutor(len(sides)) as ex:
        futs = [ex.submit(fn, s) for s in sides]
        return [f.result() for f in futs]


def current_mode() -> str:
    conf = WORK / "A/data/enginex/ui/settings.conf"
    text = conf.read_text() if conf.is_file() else ""
    if "shadow_mode=auto" in text:
        return "auto"
    return "engine" if "shadow_legacy_engine=1" in text else "display"


def instance_version(side: str) -> dict:
    vf = WORK / side / "version.json"
    if vf.is_file():
        return json.loads(vf.read_text())
    bf = WORK / "build-version.json"
    if bf.is_file():
        d = json.loads(bf.read_text())
        d["side"] = side
        d["role"] = SIDE_CFG[side]["role"]
        d["mode"] = current_mode()
        return d
    return {
        "line": "unknown",
        "line_slug": "unknown",
        "commit": "unknown",
        "full_commit": "",
        "subject": "",
        "side": side,
        "role": SIDE_CFG[side]["role"],
        "mode": current_mode(),
    }


def cmd_up(args) -> None:
    t0 = time.time()
    cmd_down(argparse.Namespace(clean=False), quiet=True)
    src_dir, ver_info = prepare_build_source(
        ref=args.ref,
        fetch=args.fetch,
        local=args.local,
    )
    ensure_base_artifacts(
        src_dir=src_dir,
        ver_info=ver_info,
        build_suite=True,
        build_packs=True,
        build_hook=args.build_hook,
    )
    print("==> Configuring veth pair + tc netem between shadow-twin-a and shadow-twin-b...", flush=True)
    setup_network(delay_ms=args.delay_ms, jitter_ms=args.jitter_ms)

    mode = args.mode
    print(
        f"==> Starting instances A (role 13) and B (role 24) "
        f"[line={ver_info['line']} commit={ver_info['commit']} mode={mode}]...",
        flush=True,
    )
    for s in SIDES:
        write_instance_configs(s, mode, src_dir, ver_info)
        start_namespace_holder(s, src_dir)

    both(lambda s: start_services(s, src_dir))
    info = both(wait_engine_ready)
    print(
        f"==> Twin sandbox UP in {time.time() - t0:.1f}s "
        f"[line={ver_info['line']} commit={ver_info['commit']} mode={mode}]: A={info[0]} B={info[1]}",
        flush=True,
    )


def cmd_down(args, quiet: bool = False) -> None:
    if not quiet:
        print("==> Stopping shadow-twin sandbox...", flush=True)
    for s in SIDES:
        sdir = WORK / s
        for pidfile in ("sup.pid", "init.pid"):
            pf = sdir / pidfile
            if pf.is_file():
                try:
                    pid = int(pf.read_text().strip())
                    subprocess.run(["kill", "-TERM", str(pid)], check=False)
                except Exception:
                    pass
    time.sleep(0.3)
    for s in SIDES:
        sdir = WORK / s
        for pidfile in ("sup.pid", "init.pid"):
            pf = sdir / pidfile
            if pf.is_file():
                try:
                    pid = int(pf.read_text().strip())
                    subprocess.run(["kill", "-KILL", str(pid)], check=False)
                except Exception:
                    pass
                pf.unlink(missing_ok=True)
        sh(f"ip netns del {SIDE_CFG[s]['netns']} 2>/dev/null || true", check=False)

    sh("pkill -KILL -f 'ns-holder\\.sh|shadow-twin-sync' 2>/dev/null || true", check=False)

    if getattr(args, "clean", False) or getattr(args, "purge", False):
        ro_mnt = WORK / "base/ro"
        if subprocess.run(["mountpoint", "-q", str(ro_mnt)]).returncode == 0:
            sh(f'umount -l "{ro_mnt}" 2>/dev/null || true', check=False)
        if (WORK / "src").exists():
            sh(f'git -C "{REPO}" worktree remove --force "{WORK}/src" 2>/dev/null || true', check=False)
            sh(f'git -C "{REPO}" worktree prune 2>/dev/null || true', check=False)
        if getattr(args, "purge", False):
            shutil.rmtree(WORK, ignore_errors=True)
            if not quiet:
                print(f"==> Purged {WORK}", flush=True)
        else:
            for item in WORK.iterdir() if WORK.is_dir() else []:
                if item.name != "out":
                    if item.is_dir() and not item.is_symlink():
                        shutil.rmtree(item, ignore_errors=True)
                    else:
                        item.unlink(missing_ok=True)
            if not quiet:
                print(f"==> Cleaned unpacked dirs, mounts, and namespaces (preserved {WORK}/out)", flush=True)
    elif not quiet:
        print("==> Sandbox stopped cleanly.", flush=True)


def cmd_update(args) -> None:
    t0 = time.time()
    src_dir, ver_info = prepare_build_source(
        ref=args.ref or DEFAULT_REF,
        fetch=not args.no_fetch,
        local=args.local,
    )
    ensure_base_artifacts(
        src_dir=src_dir,
        ver_info=ver_info,
        build_suite=True,
        build_packs=True,
        build_hook=True,
    )
    mode = args.mode or current_mode()

    # Check if namespaces are already up; if not, bring them up
    running = all((WORK / s / "init.pid").is_file() for s in SIDES)
    if not running:
        setup_network(delay_ms=2.0, jitter_ms=1.1)
        for s in SIDES:
            write_instance_configs(s, mode, src_dir, ver_info)
            start_namespace_holder(s, src_dir)
    else:
        for s in SIDES:
            write_instance_configs(s, mode, src_dir, ver_info)

    both(lambda s: start_services(s, src_dir))
    info = both(wait_engine_ready)
    print(
        f"==> Updated & deployed in {time.time() - t0:.1f}s:\n"
        f"    line:   {ver_info['line']} (resolved from {ver_info['resolved_ref']})\n"
        f"    commit: {ver_info['commit']} ({ver_info['full_commit']})\n"
        f"    title:  {ver_info['subject']}\n"
        f"    mode:   {mode}\n"
        f"    ready:  A={info[0]} B={info[1]}",
        flush=True,
    )


def cmd_deploy(args) -> None:
    t0 = time.time()
    src_dir, ver_info = prepare_build_source(
        ref=args.ref,
        fetch=False,
        local=args.local,
    )
    ensure_base_artifacts(
        src_dir=src_dir,
        ver_info=ver_info,
        build_suite=args.suite or not (args.packs or args.hook),
        build_packs=args.packs or not (args.suite or args.hook),
        build_hook=args.hook,
    )
    mode = args.mode or current_mode()
    for s in SIDES:
        write_instance_configs(s, mode, src_dir, ver_info)
    both(lambda s: start_services(s, src_dir))
    info = both(wait_engine_ready)
    print(
        f"==> Redeployed [line={ver_info['line']} commit={ver_info['commit']} mode={mode}] "
        f"in {time.time() - t0:.1f}s: A={info[0]} B={info[1]}",
        flush=True,
    )


def cmd_load(args) -> None:
    t0 = time.time()
    sides = list(SIDES) if args.side == "AB" else [args.side]

    def load_one(s: str):
        cfg = SIDE_CFG[s]
        d = cfg["deck"]
        track_path = args.track or f"/data/music/{cfg['track']}"
        url = track_path if track_path.startswith("file://") else f"file://{track_path}"
        js = (
            f"for(var n=1;n<=4;n++){{var p=root.rtTr('/Engine/Deck'+n+'/Play');if(p)p.state=false;}}"
            f"root.rtTr('/Client/Librarian/AddToDatabaseAndLoadToDeck{d}').url={json.dumps(url)};"
            f"'load-requested'"
        )
        ev(s, js, name="load")

        deadline = time.time() + 25.0
        while time.time() < deadline:
            st = evj(
                s,
                f"JSON.stringify({{"
                f"loaded: !!(root.rtTr('/Engine/Deck{d}/Track/SongLoaded')||{{}}).state,"
                f"bpm: (root.rtTr('/Engine/Deck{d}/OriginalBPM')||{{}}).unnormalized||0,"
                f"len: (root.rtTr('/Engine/Deck{d}/Track/TrackLength')||{{}}).unnormalized||0,"
                f"sr: (root.rtTr('/Engine/Deck{d}/Track/SampleRate')||{{}}).value||0"
                f"}})",
                name="poll-load",
            )
            if st["loaded"] and st["bpm"] > 60 and abs(st["bpm"] - 120.0) > 0.1 and st["len"] > 0:
                break
            time.sleep(0.4)
        else:
            raise TimeoutError(f"{s}: track load/analysis did not complete on Deck {d}")

        time.sleep(2.0)
        ev(s, f"root.rtTr('/Engine/Deck{d}/Track/SetSongPosition').value=0.20; 'seeked'", name="seek")
        return st

    res = both(load_one, sides=sides)
    deadline = time.time() + 20.0
    while time.time() < deadline:
        ok = True
        for s in SIDES:
            cfg = SIDE_CFG[s]
            sd = cfg["shadow_deck"]
            r = subprocess.run(
                ["ip", "netns", "exec", cfg["netns"], "curl", "-sf", "http://127.0.0.1:8080/v1/sc5000/shadow"],
                capture_output=True,
                text=True,
            )
            if r.returncode != 0:
                ok = False
                continue
            j = json.loads(r.stdout)
            decks = [d for d in (j.get("decks") or []) if d.get("deck") == sd]
            if not decks:
                ok = False
                continue
            d0 = decks[0]
            if j.get("displayOnly", True):
                if d0.get("loading") or not d0.get("displayDetail"):
                    ok = False
            else:
                if d0.get("loading") or not d0.get("path"):
                    ok = False
                else:
                    loaded = evj(s, f"JSON.stringify(!!(root.rtTr('/Engine/Deck{sd}/Track/SongLoaded')||{{}}).state)", name="sh-ld")
                    if not loaded:
                        ok = False
        if ok:
            if current_mode() != "display":
                time.sleep(2.5)
            break
        time.sleep(0.5)

    if not args.keep_view:
        both(lambda s: ev(s, BOOTSTRAP_JS, name="postload"))
    print(f"==> Loaded & analyzed tracks + shadow sync in {time.time() - t0:.1f}s: {res}", flush=True)


def cmd_play(args) -> None:
    action = args.action
    if action in ("stop", "pause"):
        both(lambda s: ev(s, "for(var n=1;n<=4;n++){var p=root.rtTr('/Engine/Deck'+n+'/Play');if(p)p.state=false};'paused'", name="pause"))
        print("==> Paused all decks on A and B.", flush=True)
        return

    sides = list(SIDES) if action in ("start", "AB") else [action]

    def start_one(s: str):
        d = SIDE_CFG[s]["deck"]
        return ev(s, f"root.rtTr('/Engine/Deck{d}/Play').state=true; 'playing'", name="play")

    both(start_one, sides=sides)
    time.sleep(1.2)
    pos = both(
        lambda s: evj(
            s,
            f"JSON.stringify({{"
            f"play: !!(root.rtTr('/Engine/Deck{SIDE_CFG[s]['deck']}/PlayState')||{{}}).state,"
            f"posSamples: Math.round((root.rtTr('/Engine/Deck{SIDE_CFG[s]['deck']}/Track/TrackPosition')||{{}}).unnormalized||0)"
            f"}})",
            name="pos",
        ),
        sides=sides,
    )
    print(f"==> Playback active on {sides}: {pos}", flush=True)


def cmd_sync(args) -> None:
    leader = args.leader
    follower = "B" if leader == "A" else "A"
    ld_deck = SIDE_CFG[leader]["deck"]
    fl_deck = SIDE_CFG[follower]["deck"]
    ev(leader, f"root.rtPress('/Engine/Deck{ld_deck}/RequestMaster'); '{leader}{ld_deck}-master'", name="master")
    time.sleep(0.5)
    ev(follower, f"root.rtPress('/Engine/Deck{fl_deck}/Sync'); '{follower}{fl_deck}-sync'", name="sync")
    time.sleep(0.5)
    print(f"==> Synced: {leader} Deck {ld_deck} = MASTER, {follower} Deck {fl_deck} = SYNC", flush=True)


def cmd_nudge(args) -> None:
    for spec in args.specs:
        s, direction, ms_str = spec.split(":")
        s = s.upper()
        d = SIDE_CFG[s]["deck"]
        ms = int(ms_str)
        js = (
            f"(function(){{"
            f"var t=root.rtTr('/Engine/Deck{d}/Track/Nudge{direction}');"
            f"t.state=true;"
            f"var k=Qt.createQmlObject('import QtQml;Timer{{interval:{ms};running:true;property var cb;onTriggered:{{cb();destroy()}}}}',root);"
            f"k.cb=function(){{t.state=false}};"
            f"return 'nudge {s}{d} {direction} {ms}ms'"
            f"}})()"
        )
        res = ev(s, js, name="nudge")
        print(f"==> {res}", flush=True)


VIEW_PRESETS = (
    "4deck",
    "2deck",
    "vertical",
    "browse",
    "control-center",
    "cc",
    "layout",
    "layout-view",
    "layout-4deck",
    "preferences-display",
    "display",
    "preferences-playback",
    "settings-device",
    "settings-services",
    "settings-about",
    "1",
    "2",
    "3",
    "4",
    "5",
)


def switch_view_one(side: str, preset: str) -> str:
    cc_helper = r"""
    function openCC(tabIdx, subTabIdx, scrollY) {
        function findObj(item, name) {
            if (!item) return null;
            if (item.objectName === name) return item;
            var ch = item.children || [];
            for (var i = 0; i < ch.length; i++) { var f = findObj(ch[i], name); if (f) return f; }
            return null;
        }
        function findAll(item, name, out) {
            if (!item) return;
            if (item.objectName === name) out.push(item);
            var ch = item.children || [];
            for (var i = 0; i < ch.length; i++) findAll(ch[i], name, out);
        }
        function findFlickables(item, out) {
            if (!item) return;
            if (item.contentHeight !== undefined && item.contentY !== undefined && item.visible) out.push(item);
            var ch = item.children || [];
            for (var i = 0; i < ch.length; i++) findFlickables(ch[i], out);
        }
        PlanckGUI.views.controlCenter.open();
        PlanckGUI.views.controlCenter.dragPosition = 1.0;
        var top = root.parent || root;
        var bar = findObj(top, "ControlCenter_TabBar");
        if (bar) {
            bar.currentIndex = tabIdx;
            for (var i = 0; i < bar.count; i++) {
                var it = bar.itemAt(i);
                if (it) it.checked = (i === tabIdx);
            }
        }
        if (subTabIdx !== null && subTabIdx !== undefined && subTabIdx >= 0) {
            var subBars = [];
            findAll(top, "ControlCenter_SubTabBar", subBars);
            var sb = tabIdx === 2 ? subBars[0] : subBars[1];
            if (sb) {
                sb.currentIndex = subTabIdx;
                if (sb.itemAt && sb.itemAt(subTabIdx)) sb.itemAt(subTabIdx).checked = true;
            }
        }
        if (scrollY !== null && scrollY !== undefined) {
            var tv = findObj(top, "ControlCenterTabView");
            var fls = [];
            findFlickables(tv, fls);
            for (var j = 0; j < fls.length; j++) {
                if (fls[j].contentHeight > fls[j].height) {
                    fls[j].contentY = Math.min(scrollY, Math.max(0, fls[j].contentHeight - fls[j].height));
                }
            }
        }
    }
    """
    close_cc = (
        "if(PlanckGUI.views.controlCenter){"
        "if(typeof PlanckGUI.views.controlCenter.close==='function')PlanckGUI.views.controlCenter.close();"
        "PlanckGUI.views.controlCenter.dragPosition=0.0;"
        "}"
    )

    if preset in ("1", "2", "3", "4", "5"):
        js = f"{close_cc} root.scViewSelect({int(preset)}); 'view-{preset}'"
    elif preset == "4deck":
        js = (
            f"{close_cc} root.rtTr('/GUI/ShowBrowseView').state=false;"
            "root.scViewApply({pads:false,fx:false,library:false,deckCount:'4 Decks',waveformCount:'4 Waveforms',waveformOrientation:'Horizontal'});"
            "'4deck-horizontal'"
        )
    elif preset == "2deck":
        js = (
            f"{close_cc} root.rtTr('/GUI/ShowBrowseView').state=false;"
            "root.scViewApply({pads:false,fx:false,library:false,deckCount:'2 Decks',waveformCount:'2 Waveforms',waveformOrientation:'Horizontal'});"
            "'2deck-horizontal'"
        )
    elif preset == "vertical":
        js = (
            f"{close_cc} root.rtTr('/GUI/ShowBrowseView').state=false;"
            "root.scViewApply({pads:false,fx:false,library:false,deckCount:'4 Decks',waveformCount:'4 Waveforms',waveformOrientation:'Vertical'});"
            "'4deck-vertical'"
        )
    elif preset == "browse":
        js = f"{close_cc} var p=root.rtTr('/GUI/ShowBrowseView'); p.state=!p.state; 'browse='+p.state"
    elif preset in ("control-center", "cc"):
        js = f"(function(){{ {cc_helper} openCC(0, -1, null); return 'control-center'; }})()"
    elif preset in ("layout", "layout-view"):
        js = f"(function(){{ {cc_helper} openCC(1, -1, 0); return 'layout-view'; }})()"
    elif preset == "layout-4deck":
        js = f"(function(){{ {cc_helper} openCC(1, -1, 900); return 'layout-4deck'; }})()"
    elif preset in ("preferences-display", "display"):
        js = f"(function(){{ {cc_helper} openCC(2, 2, 0); return 'preferences-display'; }})()"
    elif preset == "preferences-playback":
        js = f"(function(){{ {cc_helper} openCC(2, 0, 0); return 'preferences-playback'; }})()"
    elif preset == "settings-device":
        js = f"(function(){{ {cc_helper} openCC(3, 0, 0); return 'settings-device'; }})()"
    elif preset == "settings-services":
        js = f"(function(){{ {cc_helper} openCC(3, 3, 0); return 'settings-services'; }})()"
    elif preset == "settings-about":
        js = f"(function(){{ {cc_helper} openCC(3, 4, 0); return 'settings-about'; }})()"
    else:
        raise ValueError(f"Unknown view preset: {preset}")

    res = ev(side, js, name="view")
    # Allow 300ms for ControlCenter ColorAnimation (200ms) and Flickable layout to settle
    time.sleep(0.30)
    return res


def cmd_view(args) -> None:
    sides = list(SIDES) if args.side == "AB" else [args.side]
    res = both(lambda s: switch_view_one(s, args.preset), sides=sides)
    print(f"==> View switched ({args.preset}) on {sides}: {res}", flush=True)


def load_watermark_font(size: int = 13):
    for cand in (
        "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
        "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
        str(WORK / "base/ro/usr/Engine/Fonts/Roboto-Medium.ttf"),
        str(WORK / "base/ro/usr/Engine/Fonts/Roboto-Regular.ttf"),
    ):
        if os.path.isfile(cand):
            try:
                return ImageFont.truetype(cand, size)
            except Exception:
                pass
    return ImageFont.load_default()


def annotate_image(im: Image.Image, text: str) -> Image.Image:
    rgba = im.convert("RGBA")
    overlay = Image.new("RGBA", rgba.size, (0, 0, 0, 0))
    draw = ImageDraw.Draw(overlay)
    font = load_watermark_font(13)
    bbox = draw.textbbox((0, 0), text, font=font)
    tw = bbox[2] - bbox[0]
    th = bbox[3] - bbox[1]
    pad_x, pad_y = 8, 4
    box_w = tw + pad_x * 2
    box_h = th + pad_y * 2
    x0 = max(4, rgba.width - box_w - 6)
    y0 = rgba.height - box_h - 4
    draw.rounded_rectangle([x0, y0, x0 + box_w, y0 + box_h], radius=4, fill=(10, 12, 16, 205), outline=(0, 195, 227, 180), width=1)
    draw.text((x0 + pad_x, y0 + pad_y - 1), text, font=font, fill=(235, 245, 255, 255))
    return Image.alpha_composite(rgba, overlay).convert("RGB")


def grab_one(side: str, name: str) -> tuple[Path, Path, dict]:
    sdir = WORK / side
    ver = instance_version(side)
    in_tmp = f"/tmp/shot-{name}.png"
    host_tmp = sdir / f"tmp/shot-{name}.png"
    host_tmp.unlink(missing_ok=True)
    # Grab root.parent (QQuickRootItem) so both EmbeddedMainWindow and QQuickOverlay
    # (Control Center drawer,Preferences, Layout -> View, modals) are captured.
    js = (
        f"(function(){{"
        f"var win = root.parent || root;"
        f"win.grabToImage(function(r){{ r.saveToFile({json.dumps(in_tmp)}); }});"
        f"return 'grabbing'"
        f"}})()"
    )
    ev(side, js, name="shot")
    deadline = time.time() + 10.0
    while time.time() < deadline:
        if host_tmp.is_file() and host_tmp.stat().st_size > 1024:
            data = host_tmp.read_bytes()
            if data.endswith(b"IEND\xaeB`\x82"):
                im = Image.open(host_tmp).convert("RGB")
                wm_text = (
                    f"{ver['line']} @ {ver['commit']} | "
                    f"{side} (role {ver['role']}) | "
                    f"mode={ver['mode']} | {name}"
                )
                im = annotate_image(im, wm_text)
                slug = ver.get("line_slug", "line")
                sha = ver.get("commit", "unknown")
                tagged_path = WORK / "out" / f"{name}-{slug}-{sha}-{side}.png"
                plain_path = WORK / "out" / f"{name}-{side}.png"
                tagged_path.parent.mkdir(parents=True, exist_ok=True)
                im.save(tagged_path)
                shutil.copy2(tagged_path, plain_path)
                return tagged_path, plain_path, ver
        time.sleep(0.05)
    raise TimeoutError(f"{side}: grabToImage did not write {host_tmp}")


def cmd_shot(args) -> None:
    t0 = time.time()
    sides = list(SIDES) if args.side == "AB" else [args.side]
    if args.ui:
        both(lambda s: switch_view_one(s, args.ui), sides=sides)

    tagged_paths = []
    plain_paths = []
    vers = []
    for idx, s in enumerate(sides):
        if idx > 0:
            time.sleep(1.0)
        tp, pp, ver = grab_one(s, args.name)
        tagged_paths.append(tp)
        plain_paths.append(pp)
        vers.append(ver)

    ver0 = vers[0]
    slug = ver0.get("line_slug", "line")
    sha = ver0.get("commit", "unknown")
    if len(tagged_paths) == 2:
        im_a = Image.open(tagged_paths[0]).convert("RGB")
        im_b = Image.open(tagged_paths[1]).convert("RGB")
        comb = Image.new("RGB", (im_a.width + im_b.width + 16, im_a.height), (16, 17, 20))
        comb.paste(im_a, (0, 0))
        comb.paste(im_b, (im_a.width + 16, 0))
        ab_tagged = WORK / "out" / f"{args.name}-{slug}-{sha}-AB.png"
        ab_plain = WORK / "out" / f"{args.name}-AB.png"
        comb.save(ab_tagged)
        shutil.copy2(ab_tagged, ab_plain)
        tagged_paths.append(ab_tagged)
        plain_paths.append(ab_plain)

    meta = {
        "name": args.name,
        "ui_preset": args.ui,
        "line": ver0.get("line"),
        "line_slug": slug,
        "commit": sha,
        "full_commit": ver0.get("full_commit"),
        "subject": ver0.get("subject"),
        "mode": ver0.get("mode"),
        "caption": f"[{ver0.get('line')} @ {sha}] mode={ver0.get('mode')} name={args.name}" + (f" ui={args.ui}" if args.ui else ""),
        "files": [str(p) for p in tagged_paths],
        "latest_aliases": [str(p) for p in plain_paths],
    }
    meta_path = WORK / "out" / f"{args.name}-{slug}-{sha}.json"
    meta_path.write_text(json.dumps(meta, indent=2))
    print(
        f"==> Captured eglfs screenshots in {time.time() - t0:.2f}s:\n"
        f"    caption: {meta['caption']}\n"
        f"    commit:  {sha} ({ver0.get('subject', '')})\n"
        f"    files:   {[str(p) for p in tagged_paths]}",
        flush=True,
    )


def sample_proc(side: str, duration_s: int, out_file: Path) -> dict:
    """Sample EMain and whole-Engine CPU utilization at 4 Hz inside the instance."""
    iters = max(4, int(duration_s * 4))
    script = f"""
p=$(ps -eo pid,comm | awk '$2=="EMain"||$2=="Engine"{{print $1;exit}}')
hz=$(getconf CLK_TCK)
echo "META pid=$p hz=$hz"
t0=$(awk '{{print $1}}' /proc/uptime)
s0=$(cat /proc/$p/task/$p/stat 2>/dev/null)
p0=$(cat /proc/$p/stat 2>/dev/null)
max_d=0
prev_t=""
i=0
while [ "$i" -lt {iters} ]; do
    cur=$(cat /proc/$p/task/$p/stat 2>/dev/null) || break
    ticks=$(echo "$cur" | awk '{{print $14+$15}}')
    if [ -n "$prev_t" ]; then
        d=$((ticks - prev_t))
        [ "$d" -gt "$max_d" ] && max_d=$d
    fi
    prev_t=$ticks
    i=$((i+1))
    sleep 0.25
done
t1=$(awk '{{print $1}}' /proc/uptime)
s1=$(cat /proc/$p/task/$p/stat 2>/dev/null)
p1=$(cat /proc/$p/stat 2>/dev/null)
echo "START $t0 $s0"
echo "END $t1 $s1"
echo "PROC0 $p0"
echo "PROC1 $p1"
echo "MAX_QUARTER_TICKS $max_d"
"""
    r = subprocess.run(
        ns_chroot_cmd(side, script),
        capture_output=True,
        text=True,
        timeout=duration_s + 30,
    )
    out_file.write_text(r.stdout)

    def ticks_of(stat_line: str) -> int:
        after = stat_line[stat_line.rfind(")") + 2 :].split()
        return int(after[11]) + int(after[12])

    lines = r.stdout.splitlines()
    start_l = next(l for l in lines if l.startswith("START "))
    end_l = next(l for l in lines if l.startswith("END "))
    p0_l = next(l for l in lines if l.startswith("PROC0 "))
    p1_l = next(l for l in lines if l.startswith("PROC1 "))
    max_q = int(next(l for l in lines if l.startswith("MAX_QUARTER_TICKS ")).split()[1])

    t0 = float(start_l.split()[1])
    t1 = float(end_l.split()[1])
    dt = max(0.1, t1 - t0)
    emain_cpu = (ticks_of(end_l) - ticks_of(start_l)) / dt
    proc_cpu = (ticks_of(p1_l) - ticks_of(p0_l)) / dt
    return {
        "duration_s": round(dt, 2),
        "emain_cpu_pct": round(emain_cpu, 2),
        "engine_total_cpu_pct": round(proc_cpu, 2),
        "max_250ms_ticks": max_q,
    }


def parse_rt2gui(log_path: Path, label: str) -> dict:
    rows = []
    for line in log_path.read_text(errors="replace").splitlines():
        if "[EMain" in line and "[W]" in line:
            s_line = re.sub(r"^.*?\[W\]\s*", "", line)
            s_line = re.sub(r"\s+(?:\[\d{4}-[^\]]+\]\s+)?\[EMain.*$", "", s_line)
            rows.append(s_line.strip())
        elif "RT2GUI " in line:
            rows.append(line[line.index("RT2GUI ") :].strip())
    objs = []
    for i, line in enumerate(rows):
        if not line.startswith("RT2GUI"):
            continue
        buf = line[6:].lstrip()
        for j in range(i, min(i + 700, len(rows))):
            if j > i:
                buf += rows[j]
            try:
                o = json.loads(buf)
                if o.get("label") == label:
                    objs.append(o)
                break
            except json.JSONDecodeError:
                pass
    if not objs:
        raise RuntimeError(f"No RT2GUI record for label={label} in {log_path}")
    return objs[-1]


def dist(arr: list[float]) -> dict:
    return {
        "n": len(arr),
        "over33": sum(x > 33 for x in arr),
        "over50": sum(x > 50 for x in arr),
        "over100": sum(x > 100 for x in arr),
        "max": max(arr, default=0),
    }


def cmd_perf(args) -> None:
    label = args.label
    dur_ms = int(args.seconds * 1000)
    ver0 = instance_version("A")
    print(
        f"==> Running {args.seconds}s performance window '{label}' on A and B "
        f"[line={ver0.get('line')} commit={ver0.get('commit')} mode={ver0.get('mode')}]...",
        flush=True,
    )

    def reset_one(s: str):
        d = SIDE_CFG[s]["deck"]
        ev(
            s,
            f"root.rtTr('/Engine/Deck{d}/Play').state=false;"
            f"root.rtTr('/Engine/Deck{d}/Track/SetSongPosition').value=0.1;"
            f"root.rtPress('/Engine/Deck{d}/UnSync');'reset'",
            name=f"{label}-reset",
        )

    both(reset_one)
    start_ms = int(time.time() * 1000) + 4000

    def arm_one(s: str):
        d = SIDE_CFG[s]["deck"]
        js = (
            PERF_JS.replace("START", str(start_ms))
            .replace("PLAYING", "true")
            .replace("DECK", str(d))
            .replace("LABEL", json.dumps(label))
            .replace("DURATION_MS", str(dur_ms))
        )
        return ev(s, js, name=f"{label}-arm")

    both(arm_one)
    while time.time() * 1000 < start_ms:
        time.sleep(0.05)

    proc_stats = both(lambda s: sample_proc(s, args.seconds, WORK / "out" / f"{label}-proc-{s}.txt"))
    time.sleep(2.5)
    both(lambda s: ev(s, "for(var n=1;n<=4;n++){var p=root.rtTr('/Engine/Deck'+n+'/Play');if(p)p.state=false};'paused'", name=f"{label}-stop"))

    results = {}
    for idx, s in enumerate(SIDES):
        ver = instance_version(s)
        g = parse_rt2gui(WORK / s / "logs/engine.log", label)
        gcs = [x for x in g.get("gcs", []) if g["start"] <= x <= g["end"]]
        out = {
            "label": label,
            "side": s,
            "line": ver.get("line"),
            "commit": ver.get("commit"),
            "mode": ver.get("mode"),
            "seconds": args.seconds,
            "timer": dist(g["timer"]),
            "swap": dist(g["swap"]),
            "emain_cpu_pct": proc_stats[idx]["emain_cpu_pct"],
            "engine_total_cpu_pct": proc_stats[idx]["engine_total_cpu_pct"],
            "gc": len(gcs),
            "states_first": g["states"][0] if g.get("states") else None,
            "states_last": g["states"][-1] if g.get("states") else None,
        }
        (WORK / "out" / f"{label}-analysis-{s}.json").write_text(json.dumps(out, indent=2))
        results[s] = out
    print(json.dumps(results, indent=2), flush=True)


def cmd_ping(args) -> None:
    r = sh(
        f"ip netns exec shadow-twin-a ping -c {args.count} -i 0.02 192.168.100.2",
        capture=True,
    )
    print(r.stdout, end="", flush=True)


def cmd_status(args) -> None:
    if args.fetch:
        sh(f'git -C "{REPO}" fetch origin --prune', check=False)

    sbx_full = git_out(f"rev-parse {DEFAULT_REF}", REPO, check=False)
    sbx_short = sbx_full[:7] if sbx_full else "unknown"
    sbx_subj = git_out(f"log -1 --format=%s {DEFAULT_REF}", REPO, check=False)

    hw_full = git_out(f"rev-parse {HW_REF}", REPO, check=False)
    hw_short = hw_full[:7] if hw_full else "unknown"
    hw_subj = git_out(f"log -1 --format=%s {HW_REF}", REPO, check=False)

    out = {
        "upstream": {
            "sandbox_line": {
                "ref": DEFAULT_REF,
                "commit": sbx_short,
                "full_commit": sbx_full,
                "subject": sbx_subj,
            },
            "hardware_line": {
                "ref": HW_REF,
                "commit": hw_short,
                "full_commit": hw_full,
                "subject": hw_subj,
            },
        },
        "instances": {},
    }
    for s in SIDES:
        cfg = SIDE_CFG[s]
        sdir = WORK / s
        ver = instance_version(s)
        inst_full = ver.get("full_commit", "")
        aclk = {}
        aclk_file = sdir / "tmp/alsa-clock.txt"
        if aclk_file.is_file():
            for line in aclk_file.read_text().splitlines():
                if "=" in line:
                    k, v = line.split("=", 1)
                    aclk[k] = v
        sh_resp = subprocess.run(
            ["ip", "netns", "exec", cfg["netns"], "curl", "-sf", "http://127.0.0.1:8080/v1/sc5000/shadow"],
            capture_output=True,
            text=True,
        )
        peer_resp = subprocess.run(
            ["ip", "netns", "exec", cfg["netns"], "curl", "-sf", "http://127.0.0.1:8080/peer/decks"],
            capture_output=True,
            text=True,
        )
        out["instances"][s] = {
            "ip": cfg["ip"],
            "role": cfg["role"],
            "line": ver.get("line"),
            "commit": ver.get("commit"),
            "full_commit": inst_full,
            "subject": ver.get("subject"),
            "mode": ver.get("mode"),
            "matches_sandbox_line": bool(inst_full and inst_full == sbx_full),
            "matches_hardware_line": bool(inst_full and inst_full == hw_full),
            "alsa_clock": aclk,
            "shadow": json.loads(sh_resp.stdout) if sh_resp.returncode == 0 and sh_resp.stdout else None,
            "own_peer_decks": json.loads(peer_resp.stdout) if peer_resp.returncode == 0 and peer_resp.stdout else None,
        }
    print(json.dumps(out, indent=2), flush=True)


def cmd_eval(args) -> None:
    res = ev(args.side.upper(), args.js, name="cli")
    print(res, flush=True)


def main() -> None:
    ap = argparse.ArgumentParser(description="EngineX OS dual-deck SC5000 sandbox (shadow-twin)")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_up = sub.add_parser("up", help="Start dual SC5000 sandbox instances A (13) and B (24)")
    p_up.add_argument("--ref", default=None, help=f"Git branch or commit to build (default: {DEFAULT_REF})")
    p_up.add_argument("--fetch", action="store_true", help="Fetch origin before building")
    p_up.add_argument("--local", action="store_true", help="Build from current working tree instead of git ref worktree")
    p_up.add_argument("--mode", choices=("display", "engine", "auto"), default="display", help="Shadow mode: display (shadow_legacy_engine=0), engine (=1) or auto (shadow_mode=auto)")
    p_up.add_argument("--delay-ms", type=float, default=2.0, help="One-way veth netem delay in ms (default 2.0 -> ~4.2 ms RTT)")
    p_up.add_argument("--jitter-ms", type=float, default=1.1, help="One-way veth netem jitter in ms (default 1.1)")
    p_up.add_argument("--build-hook", action="store_true", help="Also force recompile libengine-ui-hook.so")
    p_up.set_defaults(func=cmd_up)

    p_down = sub.add_parser("down", help="Stop sandbox and unmount namespaces")
    p_down.add_argument("--clean", action="store_true", help="Remove unpacked rootfs, worktree, and instance dirs (keeps out/)")
    p_down.add_argument("--purge", action="store_true", help="Remove entire /var/tmp/shadow-twin directory including out/")
    p_down.set_defaults(func=cmd_down)

    p_upd = sub.add_parser(
        "update",
        help=f"Fetch latest git ref (default: {DEFAULT_REF}), rebuild suite + hook + UI packs, deploy to A & B, and print commit",
    )
    p_upd.add_argument(
        "ref",
        nargs="?",
        default=DEFAULT_REF,
        help=f"Branch or commit to build (default: {DEFAULT_REF}; e.g. agent/shadow-grid-halfwave-20261110 or 8f5bbdb)",
    )
    p_upd.add_argument("--mode", choices=("display", "engine", "auto"), default=None, help="Optionally set shadow mode (display or engine)")
    p_upd.add_argument("--no-fetch", action="store_true", help="Skip git fetch origin")
    p_upd.add_argument("--local", action="store_true", help="Build from local repo working tree")
    p_upd.set_defaults(func=cmd_update)

    p_dep = sub.add_parser("deploy", help="Rebuild and hot-redeploy suite and/or UI packs into running sandbox")
    p_dep.add_argument("--ref", default=None, help="Optional git ref to build from")
    p_dep.add_argument("--local", action="store_true", help="Build from current repo working tree")
    p_dep.add_argument("--suite", action="store_true", help="Rebuild sc5000-suite")
    p_dep.add_argument("--packs", action="store_true", help="Rebuild UI packs")
    p_dep.add_argument("--hook", action="store_true", help="Rebuild libengine-ui-hook.so")
    p_dep.add_argument("--mode", choices=("display", "engine", "auto"), default=None, help="Optionally switch shadow mode")
    p_dep.set_defaults(func=cmd_deploy)

    p_load = sub.add_parser("load", help="Load and analyze test tracks on A (Deck 1) and B (Deck 2)")
    p_load.add_argument("side", nargs="?", choices=("A", "B", "AB"), default="AB")
    p_load.add_argument("--track", default=None, help="Optional custom track path inside chroot")
    p_load.add_argument("--keep-view", action="store_true", help="Do not reset view after load")
    p_load.set_defaults(func=cmd_load)

    p_play = sub.add_parser("play", help="Start or stop playback on A and B")
    p_play.add_argument("action", nargs="?", choices=("start", "stop", "pause", "A", "B", "AB"), default="start")
    p_play.set_defaults(func=cmd_play)

    p_sync = sub.add_parser("sync", help="Set Master on leader and Sync on follower")
    p_sync.add_argument("leader", nargs="?", choices=("A", "B"), default="A")
    p_sync.set_defaults(func=cmd_sync)

    p_nud = sub.add_parser("nudge", help="Nudge deck(s), e.g. A:Forward:120 B:Backward:80")
    p_nud.add_argument("specs", nargs="+", help="Format: <A|B>:<Forward|Backward>:<ms>")
    p_nud.set_defaults(func=cmd_nudge)

    p_view = sub.add_parser("view", help="Switch layout/view or open Control Center / Settings / Layout pages")
    p_view.add_argument("preset", choices=VIEW_PRESETS)
    p_view.add_argument("side", nargs="?", choices=("A", "B", "AB"), default="AB")
    p_view.set_defaults(func=cmd_view)

    p_shot = sub.add_parser("shot", help="Capture eglfs OpenGL screenshot(s) with line+commit filename and corner watermark")
    p_shot.add_argument("name", help="Output filename prefix under /var/tmp/shadow-twin/out/")
    p_shot.add_argument("side", nargs="?", choices=("A", "B", "AB"), default="AB")
    p_shot.add_argument("--ui", choices=VIEW_PRESETS, default=None, help="Optionally switch UI view/page before capturing")
    p_shot.set_defaults(func=cmd_shot)

    p_perf = sub.add_parser("perf", help="Capture playback performance window (frame swaps >33ms, EMain CPU)")
    p_perf.add_argument("label", help="Output label")
    p_perf.add_argument("--seconds", type=int, default=60, help="Window duration in seconds (default 60)")
    p_perf.set_defaults(func=cmd_perf)

    p_ping = sub.add_parser("ping", help="Measure veth netem ping RTT between A and B")
    p_ping.add_argument("-c", "--count", type=int, default=50)
    p_ping.set_defaults(func=cmd_ping)

    p_st = sub.add_parser("status", help="Show running line/commit on A & B, compare with upstream lines, and show shadow/clock state")
    p_st.add_argument("--fetch", action="store_true", help="Fetch origin before comparing upstream commits")
    p_st.set_defaults(func=cmd_status)

    p_ev = sub.add_parser("eval", help="Evaluate QML JS on A or B via /tmp/ui-eval.js")
    p_ev.add_argument("side", choices=("A", "B", "a", "b"))
    p_ev.add_argument("js", help="JavaScript expression")
    p_ev.set_defaults(func=cmd_eval)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
