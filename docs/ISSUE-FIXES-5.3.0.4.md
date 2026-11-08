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
| **#150**（Spotify） | `pkg/providers/spotify_connect.go`<br>`rootfs-overlay/etc/avahi/` | 内置 Avahi `_spotify-connect._tcp` mDNS 广播与标准 768-bit Diffie-Hellman 公钥，手机 Spotify App 可在局域网直接发现 `EngineX OS`；修复 OAuth 登录下曲目下载 `410` | `pkg/providers/providers_test.go`、`tests/test_rootfs_overlay.sh` |
| **Multi-Deck Hub** | `cmd/sc5000-relay/{main,pages,web,decks}.go`<br>`internal/stems/stems.go`<br>`deploy/stems/sc5000-stemsd.service`<br>`pkg/webui/relay*.go` | `sc5000-relay` 控制台新增「已连接碟机集群 (Multi-Deck Hub)」：汇总同一后端下多台碟机的在线状态、机位角色（Role `13` / `24`）、固件版本与流媒体登录状态，支持碟机访问码或主密码登录，并实时探测碟机 About 页的 Stems 算力节点状态（`sc5000-stemsd` 局域网直连） | `cmd/sc5000-relay/relay_test.go`、`pkg/webui/relay_probe_test.go`、`internal/stems/stems_test.go` |

> 上表「模块」「验证」列按当前工作区的未提交改动文件列出，发版前请对照最终提交核对，并补上实际跑过的测试结果。

## 2. 待发版时补全

- 发布日期、构建提交、20 个产物 SHA-256 表
- 发版门禁结果（`tools/az01-packer/release-gate.sh`，上一版用 `v5.3.0.3` 的 JP07 / JP13 / JC11）
- 回滚点：`v5.3.0.3`（构建提交 `37e83ea`）
- 实机影响与升级说明
- 本次包含的所有关联 GitHub Issues 逐条留言（#49、#105、#135、#147、#150）
