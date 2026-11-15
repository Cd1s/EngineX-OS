#!/usr/bin/env python3
"""Build reproducible, source-included official Mod archives (stdlib + Go).

Run (on a build host with Go, never on a deck): python3 tools/mods/build_official_mods.py
Check without writing: python3 tools/mods/build_official_mods.py --check
The decks have no Python (#208): each Mod ships QML, a busybox start.sh and one
static helper binary per architecture (linux/arm GOARM=7 for gen-1 and gen-2
armhf, linux/arm64 for AZ04), built here with CGO_ENABLED=0 -trimpath from
docs/mods/src. The Go sources ship in the package too. No network or device access.
"""

import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[2]
DEST = ROOT / "docs/mods"
SRC = DEST / "src"
DATE = "2026-11-12"
LEGACY_COMMENT_DATE = "2026-11-08"


def tri(s, t, e):
    return dict(s=s, t=t, e=e)


def comment(author, body, number, fragment="", date=LEGACY_COMMENT_DATE):
    return dict(author=author, body=body, date=date, issueNumber=number,
                url=f"https://github.com/Cd1s/EngineX-OS/issues/{number}{fragment}")


MODS = [
    dict(id="tailscale-remote", version="1.104.5", category="network", issueNumber=130,
         author="EngineX Official", tags=["Tailscale", "Headscale", "SSH", "SFTP"],
         name=tri("Tailscale 远程访问", "Tailscale 遠端存取", "Tailscale Remote Access"),
         summary=tri("用户态 Tailscale / Headscale 状态卡片与诊断助手。", "使用者態 Tailscale / Headscale 狀態卡片與診斷助手。", "Userspace Tailscale / Headscale status card and diagnostic helper."),
         description=tri("检测现有客户端并显示连接状态；使用系统远程访问页面按需下载、校验与登录。此包不内置二进制，也不自动开启远程访问。", "偵測既有用戶端並顯示連線狀態；使用系統遠端存取頁面按需下載、校驗與登入。此套件不內建二進位檔，也不自動開啟遠端存取。", "Detects the installed client and displays connectivity. Use the system Remote access page to download, verify and sign in. No binary is bundled and remote access is never enabled automatically."),
         upstreamReleaseUrl="https://github.com/Cd1s/EngineX-OS/releases/download/tailscale-v1.104.5/",
         initialComments=[comment("Cd1s", "The client is a trimmed Tailscale v1.104.5 build", 130, "#issuecomment-6707656533", "2026-11-07")]),
    dict(id="dmx-studio", version="1.2.0", category="lighting", issueNumber=134,
         issueNumbers=[134, 137, 209], author="EngineX Official & @TribalDE & @Djpd66",
         tags=["DMX512", "FTDI", "uDMX", "Art-Net", "sACN", "BPM", "EFX", "MIDI"],
         name=tri("DMX 独立灯光工作站", "DMX 獨立燈光工作站", "DMX Studio"),
         summary=tri("多域灯光输出、自动节拍/乐句 EFX、烟雾控制、MIDI 时钟与 512 通道监视器。", "多域燈光輸出、自動節拍/樂句 EFX、煙霧控制、MIDI 時鐘與 512 通道監視器。", "Multi-universe lighting, beat/phrase-synced EFX, smoke controls, MIDI clock and 512-channel monitor."),
         description=tri("独立静态 helper（armhf + arm64，无需 Python），提供 FTDI/OpenDMX、uDMX 16c0:05dc、Art-Net/sACN 多域输出。无需加载碟机面板即可自动从 stream-sync 读取 MASTER 碟机真实节拍相位（内存时钟，绝不写入 show.json；无播放轨时平滑回退手动 BPM），支持 Circle/8-Figure/Pan-Tilt Sweep、可选乐句切场（phraseBeats）与 24 PPQN ALSA MIDI Clock 输出。需显式启动 helper；实机灯具与 MIDI 外设待实机验证，通用灯库须按灯具手册核对。", "獨立靜態 helper（armhf + arm64，無須 Python），提供 FTDI/OpenDMX、uDMX 16c0:05dc、Art-Net/sACN 多域輸出。無須載入播放機面板即可自動從 stream-sync 讀取 MASTER 播放機真實節拍相位（記憶體時鐘，絕不寫入 show.json；無播放軌時平滑退回手動 BPM），支援 Circle/8-Figure/Pan-Tilt Sweep、可選樂句切場（phraseBeats）與 24 PPQN ALSA MIDI Clock 輸出。須明確啟動 helper；實機燈具與 MIDI 外設待實機驗證，通用燈庫須依燈具手冊核對。", "Standalone static helper (armhf + arm64, no Python needed) for FTDI/OpenDMX, uDMX 16c0:05dc and multi-universe Art-Net/sACN. Reads the MASTER deck's live beat phase directly from stream-sync without requiring a deck panel (in-memory clock, never writes show.json; smooth manual BPM fallback), supporting Circle/8-Figure/Pan-Tilt Sweep, optional phrase scenes (phraseBeats) and 24 PPQN ALSA MIDI clock out. Start the helper explicitly. Fixture output, beat-synced shows and MIDI clock need real hardware validation; check generic profiles against the fixture manual."),
         initialComments=[comment("RattyDAVE", "Some sort of interface to control DMX lights.", 134), comment("Djpd66", 'A momentary "Smoke Flash" button (active only while held down)', 134, "#issuecomment-6711005897", "2026-11-07")]),
    dict(id="jog-logo-studio", version="1.2.0", category="ui", issueNumber=137,
         author="EngineX Official & @Djpd66", tags=["Jog Display", "600x600", "PNG", "Logo"],
         name=tri("转盘台标工作室", "轉盤台標工作室", "Jog Logo Studio"),
         summary=tri("裁切 600×600 logo.png，并同步到所选媒体盘。", "裁切 600×600 logo.png，並同步至所選媒體碟。", "Crop a 600×600 logo.png and synchronize selected media drives."),
         description=tri("中心裁切图片，预览、备份并写入媒体盘 Engine Library/logo.png；提供多盘同步和启用/停用。需要已安装的 ffmpeg。加载媒体配置后显示，只有带转盘屏的机型适用；尚未验证所有机型。", "中心裁切圖片，預覽、備份並寫入媒體碟 Engine Library/logo.png；提供多碟同步和啟用/停用。需要已安裝的 ffmpeg。載入媒體設定後顯示，僅有轉盤螢幕的機型適用；尚未驗證所有機型。", "Center-crops images, previews, backs up and writes Engine Library/logo.png on selected media drives, with multi-drive sync and enable/disable. Requires an existing ffmpeg. Reload the media profile to display. Only models with a jog display benefit; all-model hardware validation is pending."),
         initialComments=[comment("Djpd66", "set custom logos on the jog wheel displays", 137)]),
    dict(id="sampler-pad-studio", version="1.2.0", category="audio", issueNumber=137,
         issueNumbers=[120, 137], author="EngineX Official & @tribalwalker-sketch & @Djpd66",
         tags=["Sampler", "8 Pads", "Loop", "WAV"],
         name=tri("八格采样打击垫管理器", "八格取樣打擊墊管理器", "Sampler Pad Studio"),
         summary=tri("八格音源管理、重命名、清空与 Loop 切片导出。", "八格音源管理、重新命名、清空與 Loop 切片匯出。", "Eight-slot sample management, rename, clear and loop-slice export."),
         description=tri("可视化管理独立八格采样库，导入 WAV、重命名、清空并按采样点精确切片；其他格式使用已安装的 ffmpeg。导出后在原生浏览器加载到打击垫。固件暂未暴露原生打击垫直接绑定接口，本 Mod 不声称自动绑定或播放。", "視覺化管理獨立八格取樣庫，匯入 WAV、重新命名、清空並依取樣點精確切片；其他格式使用已安裝的 ffmpeg。匯出後在原生瀏覽器載入至打擊墊。韌體尚未公開原生打擊墊直接綁定介面，本 Mod 不宣稱自動綁定或播放。", "Manage an independent eight-slot sample bank: import WAV, rename, clear and slice precisely by sample offsets; other formats use an existing ffmpeg. Load exported files into the native pads through the browser. Firmware has no exposed direct native-pad binding API; this Mod does not claim automatic binding or playback."),
         initialComments=[comment("tribalwalker-sketch", "One control - a Control Center row, or a long-press on an empty sampler pad", 120), comment("Djpd66", "dedicated UI tab for managing the sampler", 137)]),
    dict(id="minimal-club-mode", version="1.2.1", category="system", issueNumber=136,
         author="EngineX Official", tags=["Club", "USB", "SD", "Pro DJ Link", "Offline"],
         name=tri("纯粹俱乐部演出模式", "純粹俱樂部演出模式", "Minimal Club Mode"),
         summary=tri("暂停可选后台服务，优先本地 USB/SD 与 Pro DJ Link 演出。", "暫停選用背景服務，優先本機 USB/SD 與 Pro DJ Link 演出。", "Pause optional background services for local USB/SD and Pro DJ Link performance."),
         description=tri("记录并暂停下载、deemix、stems、广播后台服务，退出时只恢复之前运行的服务。保留 Engine、音频、Rekordbox 桥和 Pro DJ Link；不修改音频调度，不承诺未经测量的 100% 算力或绝对稳定性。显式启用且可恢复，不永久 mask 服务。", "記錄並暫停下載、deemix、stems、廣播背景服務，退出時僅恢復原先執行中的服務。保留 Engine、音訊、Rekordbox 橋與 Pro DJ Link；不修改音訊排程，不承諾未經量測的 100% 算力或絕對穩定性。明確啟用且可還原，不永久 mask 服務。", "Records and pauses optional download, deemix, stems and broadcasting services, restoring only those previously running. Preserves Engine, audio, Rekordbox bridge and Pro DJ Link. Does not change audio scheduling or promise an unmeasured 100% CPU budget or absolute stability. Explicit, reversible activation; no permanent service masks."),
         initialComments=[comment("u/krimsamp (quoted in issue #136)", "Can we have a stripped down version", 136)],
         ),
    dict(id="midi-controller-mapper", version="1.2.0", category="midi", issueNumber=137,
         author="EngineX Official", tags=["MIDI", "CDJ-400", "4-Deck", "Jog", "Soft Takeover"],
         name=tri("通用 MIDI 控制器映射器", "通用 MIDI 控制器映射器", "Generic MIDI Controller Mapper"),
         summary=tri("将外部 USB MIDI 控制器（首发 Pioneer CDJ-400）映射到内部 Deck 3/4。", "將外部 USB MIDI 控制器（首發 Pioneer CDJ-400）映射至內部 Deck 3/4。", "Map external USB MIDI controllers (Pioneer CDJ-400 first) to internal Deck 3/4."),
         description=tri("低优先级 (nice 19) ALSA Sequencer helper 与按需 60 Hz QML 桥接（未启用时零轮询），将外部 USB MIDI 控制器直接映射到四轨模式下的内部 Deck 3/4，无需切换硬件 Layer。含 CDJ-400 连续速度转盘积分器 (64=停止)、带 ▲▼ 指示的变速推子软接管、热拔插与 MIDI 风暴自动停用保护。CDJ-400 标准 USB MIDI 模式不接收外部 Note/CC LED 回传，状态与 ▲▼ 指示显示在屏幕上。需实机验证。", "低優先級 (nice 19) ALSA Sequencer helper 與按需 60 Hz QML 橋接（未啟用時零輪詢），將外部 USB MIDI 控制器直接映射至四軌模式下的內部 Deck 3/4，無須切換硬體 Layer。含 CDJ-400 連續速度轉盤積分器 (64=停止)、帶 ▲▼ 指示的變速推桿軟接管、熱拔插與 MIDI 風暴自動停用保護。CDJ-400 標準 USB MIDI 模式不接收外部 Note/CC LED 回傳，狀態與 ▲▼ 指示顯示於螢幕上。須實機驗證。", "Low-priority (nice 19) ALSA Sequencer helper and on-demand 60 Hz QML bridge (zero polling when disabled) mapping external USB MIDI controllers directly to internal Deck 3/4 in 4-deck mode without switching hardware layers. Includes CDJ-400 continuous velocity jog time-integration (64=stop), tempo slider soft takeover with ▲▼ indicators, and hot-unplug/storm auto-disable. CDJ-400 in standard USB MIDI mode does not accept external MIDI Note/CC LED feedback; status and ▲▼ indicators are shown on screen. Hardware validation pending."),
         initialComments=[comment("Djpd66", "connect a USB MIDI controller (Pioneer CDJ-400) to control Deck 3/4", 137, "", "2026-11-10")],
         ),
]


def json_bytes(value):
    return (json.dumps(value, ensure_ascii=False, indent=2) + "\n").encode()


HELPERS = {"armv7": dict(GOARCH="arm", GOARM="7"), "arm64": dict(GOARCH="arm64")}


def build_helpers(out):
    """One static helper per architecture; -trimpath keeps builds reproducible for a given Go."""
    import os
    for arch, env in HELPERS.items():
        target = out / f"enginex-mod-helper-{arch}"
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid=",
                        "-o", str(target), "./docs/mods/src/helper"], cwd=ROOT, check=True,
                       env=dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOFLAGS="", **env))
    return {arch: (out / f"enginex-mod-helper-{arch}").read_bytes() for arch in HELPERS}


def go_sources(mid):
    files = {}
    for part in ("helper", "modkit", mid):
        for source in sorted((SRC / part).glob("*.go")):
            if not source.name.endswith("_test.go"):
                files[f"src/{part}/{source.name}"] = source.read_bytes()
    return files


def package_files(mod, helpers):
    mid = mod["id"]
    manifest = {key: mod[key] for key in ("id", "version", "author", "category")}
    manifest.update(name=mod["name"]["e"], description=mod["description"]["e"], official=True,
                    homepage="https://enginexdj.com/#mods", license="MIT",
                    qmlFiles={"Panel.qml": f"EngineXMods/{mid}/Panel.qml"},
                    script="tailscale-helper.sh" if mid == "tailscale-remote" else "start.sh")
    files = {"mod.json": json_bytes(manifest), "Panel.qml": (SRC / mid / "Panel.qml").read_bytes(),
             "README.md": (SRC / mid / "README.md").read_bytes(),
             "LICENSE": (ROOT / "LICENSE").read_bytes()}
    if mid == "tailscale-remote":
        files["tailscale-helper.sh"] = (SRC / mid / "tailscale-helper.sh").read_bytes()
    else:
        for arch, blob in helpers.items():
            files[f"bin/enginex-mod-helper-{arch}"] = blob
        files.update(go_sources(mid))
        files["start.sh"] = (SRC / "start.sh").read_bytes().replace(b"@MOD_ID@", mid.encode())
    presets_dir = SRC / mid / "presets"
    if presets_dir.is_dir():
        for preset in sorted(presets_dir.glob("*.json")):
            files[f"presets/{preset.name}"] = preset.read_bytes()
    return files


def archive(files):
    out = io.BytesIO()
    with gzip.GzipFile(fileobj=out, mode="wb", filename="", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for name, data in sorted(files.items()):
                entry = tarfile.TarInfo(name)
                entry.size = len(data)
                entry.mode = 0o755 if name.endswith(".sh") or name.startswith("bin/") else 0o644
                entry.mtime = 0
                tar.addfile(entry, io.BytesIO(data))
    return out.getvalue()


def validate(files):
    manifest = json.loads(files["mod.json"])
    assert re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}", manifest["id"])
    assert manifest["script"] in files and manifest["qmlFiles"]
    for src, dst in manifest["qmlFiles"].items():
        assert not Path(dst).is_absolute() and ".." not in Path(dst).parts and dst.endswith(".qml")
        assert 0 < len(files[src]) <= 512 * 1024
        files[src].decode("utf-8")
    for name, data in files.items():
        assert not Path(name).is_absolute() and ".." not in Path(name).parts
        # The decks have no Python (#208): no package may ship or call it.
        assert not name.endswith((".py", ".pyc")), f"{name}: no Python on the deck"
        if name.endswith((".sh", ".qml")):
            assert b"python" not in data.lower(), f"{name} mentions python"
        if name.startswith("bin/"):
            assert data[:4] == b"\x7fELF", f"{name} is not an ELF binary"
        if name.endswith(".sh"):
            subprocess.run(["sh", "-n"], input=data, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="verify committed outputs without writing")
    args = parser.parse_args()
    registry = dict(version=1, updatedAt=DATE, mods=[])
    with tempfile.TemporaryDirectory() as temp:
        helpers = build_helpers(Path(temp))
    for mod in MODS:
        files = package_files(mod, helpers)
        validate(files)
        blob = archive(files)
        assert blob == archive(files), "archive must be deterministic"
        path = DEST / "packages" / f'{mod["id"]}-{mod["version"]}.tar.gz'
        if not args.check:
            for stale in (DEST / "packages").glob(f'{mod["id"]}-*.tar.gz'):
                if stale != path:
                    stale.unlink()
        if args.check:
            assert path.read_bytes() == blob, f"stale package: {path}"
        else:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(blob)
        entry = dict(mod, official=True, downloadUrl=f"mods/packages/{path.name}",
                     sha256=hashlib.sha256(blob).hexdigest(), size=len(blob),
                     models="All 16 Models", hardwareValidation="not-tested", stars=0, downloads=0,
                     license="MIT", sourceUrl="https://github.com/Cd1s/EngineX-OS/tree/main/docs/mods/src",
                     runtimeStatus="background-helper" if mod["id"] != "tailscale-remote" else "system-api-required",
                     **({} if mod["id"] == "tailscale-remote" else dict(helperArch=["armv7", "arm64"], requiresPython=False)))
        registry["mods"].append(entry)
        print(f'{mod["id"]}: {len(files)} files, {len(blob)} bytes, sha256={entry["sha256"]}')
    data = json_bytes(registry)
    if args.check:
        assert (DEST / "registry.json").read_bytes() == data, "stale registry"
    else:
        (DEST / "registry.json").write_bytes(data)
    print("PASS: deterministic archives, static helpers, shell syntax, no Python and registry integrity")


if __name__ == "__main__":
    main()
