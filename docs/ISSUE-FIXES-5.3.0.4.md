# EngineX OS 5.3.0.4 发版与 Issue 修复记录（草稿，尚未发布）

- **状态**：开发中。版本号文件 `rootfs-overlay/etc/sc5000/version.txt` 仍为 `5.3.0.3`，本文件只记录 5.3.0.4 已在实机测试中发现并解决的修复点，发布时再补全：日期、构建提交、20 个产物 SHA-256、发版门禁结果、回滚点（上一版 `v5.3.0.3` 构建提交 `37e83ea`）。
- **版本范围**：`v5.3.0.3` → `v5.3.0.4`
- **同步文档**：`README.md`、`README.zh-CN.md`、`README.zh-TW.md` 的「5.3.0.4 新功能」章节与 `docs/site/content.js` 的「5.3.0.4 新功能」章节（`tone: "new"`，发布时去掉提示）

---

## 1. Issue 与修复总表

| Issue / PR | 模块 | 改动与修复摘要 | 验证与回归测试 |
| --- | --- | --- | --- |
| **#49** | `pkg/webui/shadow.go`<br>`tools/ui-mod/build_packs.py` | **四轨联动完整脱离卸载与流媒体歌名显示**：对端碟机关机、重启或退歌时发送 UDP 告别包，本机在 350 ms 防抖后自动卸载影子轨（`Unload` + `UnSync` + 自动交回 `MASTER`），清除残留波形与曲目；移除 `RemoteState` 死锁订阅，对端关机不再阻塞本机切换流媒体音源；流媒体曲目在影子碟机上同步显示真实 `Artist - Title`，不再显示内部数字缓存文件名 | `pkg/webui/shadow_test.go`、双 SC5000 实机（待发版前复核） |
| **UI 降载** | `tools/ui-mod/build_packs.py` | 新增 `scTr` QML 属性 translator 缓存与 `QuickCue` 500 ms 节流，消除按 `MASTER` / `SYNC` 时的瞬时卡顿，降低 `EMain` GUI 线程 CPU 占用 | 双 SC5000 实机（待发版前复核） |
| **歌单左滑 Stems** | `tools/ui-mod/build_packs.py`<br>`tools/ui-mod/i18n.tsv` | 自动识别原厂已带 `Stem` 动作的本地曲目，去重并本地化为单一 `分轨 Stems / 分軌 Stems / Stems` 按钮，本地与流媒体 / LAN 曲目左滑体验统一 | `tools/az01-packer/test_ui_mod.py` |
| **#147 / #135** | `tools/ui-mod/build_packs.py` | Browse 旋钮波形缩放改为 10 级等比几何步进（约 1.46×/格）加 16 ms 帧合并防抖；默认直接旋转 Auto Loop 旋钮调整 Loop 大小，并自动开启 `LinkBeatJumpLoopSize` 使 Beat Jump 联动；修复 `JP07` 开启 `swap_loop_order` 时的 `beatJumpEnabled` 判定 | `tools/az01-packer/test_ui_mod.py`、实机（待发版前复核） |
| **#105** | `pkg/webui/` OmniSource | 开启 OmniSource 时搜索栏仍可跨流媒体与全部 U 盘曲库检索，但各流媒体自己的歌单页不再混入 `[USB: ...]` 与其他平台歌单 | `pkg/providers/providers_test.go` |
| **#150**（SoundCloud） | `pkg/providers/soundcloud.go` | 修复用户 OAuth `refresh_token` 过期报 `HTTP 400`：自动回退到 App Token 与公开主页接口加载 `Liked Tracks & Sets` | `pkg/providers/soundcloud_test.go` |
| **#150**（Spotify） | `pkg/providers/spotify_connect.go`<br>`rootfs-overlay/etc/avahi/` | 内置 Avahi `_spotify-connect._tcp` mDNS 广播，`stream-sync` 另带纯 Go 的 `_spotify-connect._tcp.local.` mDNS 广播器（有线 `eth0` + 无线 `wlan0` 双网卡也能被发现），并使用标准 768-bit Diffie-Hellman 公钥，手机 Spotify App 可在局域网直接发现 `EngineX OS`；修复 OAuth 登录下曲目下载 `410` | `pkg/providers/providers_test.go`、`tests/test_rootfs_overlay.sh` |
| **流媒体缓存隔离与 Deemix 共享** | `pkg/deemixclient/`<br>`pkg/providers/` | Spotify（`spotify-<id>.*`）、Beatport（`beatport-<id>.*`）、SoundCloud（`soundcloud-<id>.*`）及「官方直连」模式网易云（`netease-<id>.*`）各用独立下载通道与缓存命名空间，即使同一首歌在 Deezer / 网易云下载过，在 Spotify 点播也会重新拉取并独立缓存；网易云「Deemix 优先」时与 Deezer 同走 Deemix 无损音源并共享缓存，互相点播同一首不重复下载，切回「官方直连」后各自独立 | 待补：缓存命中 / 隔离测试（文档按需求撰写，发版前对照最终代码核对） |
| **SoundCloud Go+ 与高清智能路由** | `pkg/providers/soundcloud.go` | 超过 10 分钟的 DJ Set / 长混音始终直连 SoundCloud 官方流（原始 320k / WAV → 256k / 160k AAC → 渐进式 MP3）；Go+ 专享曲目与非 Set 单曲：已绑定 Go+ 优先直连官方高音质流，否则自动在已登录的 Deezer / 网易云 / Beatport 匹配无损 / 320k 并缓存为 `soundcloud-<id>.*`，都未登录或 SoundCloud 独占时直连，Go+ 试听曲目无可用平台时提示扫码登录 SoundCloud Go+ 或其他流媒体账号 | 待补：`pkg/providers/soundcloud_test.go`（文档按需求撰写，发版前对照最终代码核对） |
| **Multi-Deck Hub** | `cmd/sc5000-relay/{main,pages,web,decks}.go`<br>`internal/stems/stems.go`<br>`deploy/stems/sc5000-stemsd.service`<br>`pkg/webui/relay*.go` | `enginex-relay` 控制台新增「已连接碟机集群 (Multi-Deck Hub)」：汇总同一后端下多台碟机的在线状态、机位角色（Role `13` / `24`）、固件版本与流媒体登录状态，支持碟机访问码或主密码登录，并实时探测碟机 About 页的 Stems 算力节点状态（`enginex-stemsd` 局域网直连） | `cmd/sc5000-relay/relay_test.go`、`pkg/webui/relay_probe_test.go`、`internal/stems/stems_test.go` |

| **品牌统一与免域名公网连接** | `README*.md`、`docs/`、`deploy/`、`pkg/webui/{tidal_gateway,server}.go` | 面向用户的组件名统一为 `enginex-relay`（兼容旧名 `sc5000-relay`）、`enginex-stemsd`（兼容旧名 `sc5000-stemsd`）、`enginex-suite`（兼容旧名 `sc5000-suite`），运行目录写作 `/data/enginex`（`/data/sc5000-custom` 保留兼容软链接）；文案中的 `SC5000 Stream Sync` 改为 `EngineX OS`（硬件型号 `SC5000` 不变）。新增免域名一键公网连接说明：碟机 `:8080` 控制台点「开启免域名公网隧道 (Cloudflare Quick Tunnel)」自动安装并启动 `cloudflared`，生成 `https://*.trycloudflare.com` 公网 HTTPS 地址，流媒体登录二维码自动使用该地址回调；自建中继运行 `enginex-relay -public auto`（或 `run-home-worker.sh --with-tunnel`）免域名启动 | 待补：`pkg/webui` 单测（隧道功能由并行改动实现，文档发版前对照最终代码核对） |
| **#164**（Mixstream Pro+ 安装发现） | `cmd/enginex-installer/main.go`<br>`cmd/enginex-installer/reuse_*.go`<br>`cmd/enginex-installer/main_test.go`<br>`docs/manual/02-installation-and-ota.md`<br>`README*.md` | 支持 UDP 11224 的严格六字节 EAAS 发现；广播只作为未验证候选，不推断型号、不据此发送 SSH 凭据；`listenEAAS` 与 `listenAirD` 配置 `SO_REUSEADDR`/`SO_REUSEPORT`，消除与运行中 Engine DJ 争抢 11224 与 51337 端口冲突；识别现代 fastboot 35+ 的 `unknown option -- i`；在 Windows USB 诊断中明确 Google fastboot 需指定 Android GUID `{F72FE0D4-CBCB-407D-8814-9ED673D0DD6B}` 并推荐 WSL + usbipd-win（说明须以 `wsl -u root` 运行，直通节点属 root 权限）；将 #164 实测的 NH08S USB ID `15e4:103f` / `15e4:303f` 纳入统一设备表并确认为标准 U-Boot fastboot 协议；文档补充原厂 fastboot 不支持 `fetch` 分区回读说明与二代机无需预先备份的一键还原原厂路径（Web 控制台 / `--uninstall` / `uninstall.sh`）。 | `TestDiscoveryListenersSharePortsWithSOReuseAddr`、`TestFastbootStopsOnUnknownOption`、`TestWindowsMixstreamProPlusDetection` 与全部 48 个 installer 单测全部通过；实机测试反馈验证通过 |
| **一代机电脑线刷** | `cmd/enginex-installer/main.go`<br>`tools/az01-packer/az01_packer.py` | 2026-11-09 实机发现安装器无法线刷一代机：SC5000 在 Computer Update Mode 下对 `fastboot getvar product` 回 `Variable not implemented`，`modeB` 直接报「读不到 fastboot product」；且 `az01_packer.py unpack` 在 macOS / Windows 上因缺 debugfs 拒绝解包（unpack 本身不调用 debugfs）。现在拿不到 product 时按 USB ID 识别一代机（表取自官方 5.3.0 镜像头 `usb_devices`：SC5000 组 `15e4:d006/d00a/d00c`、SC6000 组 `d00d/d00e`、PRIME 4 组 `d008/d00b/203f`），ID 可出现在行内任意位置（Windows `pnputil` 格式）；debugfs 检查移到真正调用它的 `ExtFS.cmd()` | `cmd/enginex-installer` 新增 3 个用例；`test_az01_packer.py::test_cli_unpack_needs_no_debugfs`；Mac 本机无 debugfs 解包真实 JP07 镜像成功（rootfs sha256 与编译机一致）。同日右碟手动按同样步骤 fastboot 线刷成功；修复后的安装器本身尚未在实机上跑 |

> 上表「模块」「验证」列按当前工作区的未提交改动文件列出，发版前请对照最终提交核对，并补上实际跑过的测试结果。

## 2. 待发版时补全

- 发布日期、构建提交、20 个产物 SHA-256 表
- 发版门禁结果（`tools/az01-packer/release-gate.sh`，上一版用 `v5.3.0.3` 的 JP07 / JP13 / JC11）
- 回滚点：`v5.3.0.3`（构建提交 `37e83ea`）
- 实机影响与升级说明
- 本次包含的所有关联 GitHub Issues 逐条留言（#49、#105、#135、#147、#150）

## 2026-11-10：#164 Mixstream Pro+ (NH08S) 真机安装验证与后续

- **真机结果**：提问者在 NH08S 上用 WSL 2 + usbipd-win + fastboot（root）和 PR #173 的 Linux 版安装器完整刷入 5.3.0.3：识别 `rk3288-az05-nh08s`，data 分区 2,413,280,768 字节写入约 74 秒，`INSTALLER_EXIT=0`，开机后 `:8080` 出现 Stream Sync 控制台，首次开机钩子激活正常。这是 NH08S 第一例安装；`SO_REUSEADDR` 修复与 `gen2USB` 识别也在真机上得到确认。
- **改动**：`isDeckUI()` 在 `/v1/sc5000/ui` 返回 403/401（控制台要访问码）时，改看 `/` 上的登录页是否是 `<title>Stream Sync</title>`，认得出已装 EngineX 的碟机，避免局域网发现和 OTA 发现漏掉它；单测 `TestIsDeckUIRecognisesTheLoginPage` 覆盖 JSON、403/401 加登录页、别家 403、404、200 非 JSON。官网安装章节补上经真机验证的 WSL + usbipd-win 步骤和 fastboot 的 AVB 提示说明。
- **不改**：NH08S 不发 StageLinQ `airD`，局域网发现只能靠 EAAS 线索，属于板子本身的特性；fastboot 的 `avb footer` 提示是 fastboot 对非 AVB 镜像的探测输出，刷机会自动跳过。
- **未验证**：OTA 在线升级、`uninstall.sh`、#166 的 Mixstream Filter 旋钮 / FX 还没有在 NH08S 真机上验证（提问者表示愿意测）。
