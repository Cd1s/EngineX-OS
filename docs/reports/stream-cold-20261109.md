# 流媒体第二轮：列表冷启动与点歌等待（2026-11-09）

基线 `origin/agent/integration3-20261109` / `dfdef97`；交付分支 `agent/stream-cold-20261109`。本轮没有部署、没有访问碟机，没有在碟机下载、播放或测量。构建/测试全部在编译机的独占任务目录 `/root/build/sc5000-s2-20261109` 运行。

## 实现与边界

- `pkg/providers/pages.go`：有总数的页码接口先读第一页，再以每平台共享的 2 个分页/详情请求槽读取后续已知页，按原页序合并。仅保留固定两项的工作窗口，不按总页数创建 goroutine。Deezer、Spotify、Beatport 使用它；未提供总数时继续原有串行分页。
- 网易云拿到完整 `trackIds` 后，每 200 首一组、最多 2 组并发；SoundCloud Web 歌单每 50 个缺失详情一组、最多 2 组并发。批次按原序合并，再按原歌单引用序组装 SoundCloud 曲目。
- 不猜 SoundCloud/Apple 的游标，不提前发布前缀。错误/429 不重试，最多已有同波另一请求在飞；不会发布失败页之后的结果。Deezer/Spotify/网易云保留整次失败；Beatport、SoundCloud Web 保留成功前缀。过小的陈旧总数不会截断仍有 next 的列表，超出已知范围后继续串行；总数过大仍受原分页上限约束。
- 上限是**分页/批量详情**请求数；Spotify liked 总数探测、网易云收藏专辑/电台等已有独立元数据探测保持原逻辑。不是整个平台所有 HTTP 的统一 QPS 限流器。真实账号 429 仍需 PM 测量；若出现，应将该平台并发调为 1，不能盲目重试。
- 网关在整表后才按 BPM/key/title/rating 等排序并分页。直接先返回平台首屏会改变全局排序。本轮选择完整列表并行取回；没有新增 partial-cache 状态或改变 TTL、后台刷新、磁盘策略、session 隔离、singleflight。原缓存和排序测试继续通过。
- `Jobs.File` 可发现缺少 `.sources.json` 索引的已完成文件：Deezer 仅正整数 ID，网易云/Spotify/Beatport 仅自己的前缀；先通过已有 `Cached` 的音频检查。`knownCodec`/`Fetch` 因而直接命中，避免进入音源获取和同步封面抓取。已有来源索引的 Deezer↔网易云 Deemix 共享及网易云 official 隔离继续适用。不把网易云 ID 当 Deezer ID，也不根据 ISRC 猜本地共享关系。SoundCloud 的 lossless/Go+ 回退保持原路径；Beatport 无损模式下的 AAC→无损升级仍交给原获取逻辑。

## 冷启动请求数与往返估算

R=一次上游请求耗时；P=该接口页数；B=详情批次数；L=Apple 已保存公开链接数；A/D=网易云收藏专辑/电台页数。正常成功、无额外登录刷新/重试时，已知总数页码接口的 `W(P)=1+ceil((P-1)/2)`。下表时间是关键路径往返数 × R，不是把并发请求数直接相乘。无总数时 `W(P)=P`。

| Provider / 路径 | 前：请求数；关键路径 | 后：请求数；关键路径 |
| --- | --- | --- |
| Deezer 歌单 / 曲目（100/页） | P；P×R | P；W(P)×R |
| Spotify 歌单（50/页） | P+1；P×R（liked 探测并行） | P+1；W(P)×R（探测慢时仍需等它） |
| Spotify 曲目（100/页）/likes（50/页） | P；P×R | P；W(P)×R；首个 `/items` 404/405 的兼容 fallback 各另加 1×R |
| 网易云曲目（200/批） | 1+B；(1+B)×R | 1+B；(1+ceil(B/2))×R |
| 网易云歌单目录 | P+A+D；max(P,A,D)×R | 相同；没有可依赖总数 |
| 网易云专辑 / 电台曲目 | 1 / P；1 / P×R | 相同；电台仍最多 500 个节目 |
| SoundCloud Web 歌单曲目（50 个缺失详情/批） | 1+B；(1+B)×R | 1+B；(1+ceil(B/2))×R |
| SoundCloud likes / official API 曲目 | P；P×R | 相同；next_href 可能是不透明游标 |
| SoundCloud 歌单目录 | Pown+Pliked；(Pown+Pliked)×R | 相同；保留自建、收藏及整体上限语义 |
| Beatport 歌单 / 曲目 | P+1 / P；(P+1) / P×R | P+1 / P；(W(P)+1) / W(P)×R；榜单仍只取原有一页 |
| Apple Music 歌单 / 曲目 | L+P / P；(L+P) / P×R | 相同；保留 next 链；开发者凭据未就绪时另有页面/脚本获取 |
| NAS Subsonic / WebDAV / Plex | 列表或曲目各 1；1×R | 相同；原接口一次完整返回 |
| 原生 TIDAL scope 网关 | Engine 每次分页对应 1；1×R | 相同；直通 TIDAL，不进入 provider 全量列表/本地浏览缓存 |

## 300 ms httptest 实测

编译机、`-race`、每请求等待 300 ms。优化路径的“前”是同一完整请求计划使用单槽的串行控制，“后”是双槽；不是旧二进制实机成绩。两次使用新 provider 实例，无网关热缓存。测试比较完整 ID 顺序、条数、请求数，并断言峰值 ≤2 和耗时至少下降 15%。未改路径的前/后是相同实现的两次冷请求。

| 路径 / 样本 | 请求数（前=后） | 前 → 后 |
| --- | --- | --- |
| Deezer 歌单 500 项 / 曲目 500 首 | 5 / 5 | 1.546 → 0.928 s / 1.545 → 0.928 s |
| Spotify 歌单 250+liked / 曲目 500 / likes 250 | 6 / 5 / 5 | 1.517 → 0.911 s / 1.528 → 0.918 s / 1.517 → 0.911 s |
| 网易云曲目 800 首 | 5 | 1.568 → 0.950 s |
| 网易云歌单目录 500 项 | 7 | 1.542 → 1.542 s（未改） |
| SoundCloud Web 歌单 201 首、200 缺详情 | 5 | 1.549 → 0.943 s |
| SoundCloud likes 500 首 / 自建 500+likes 合成行目录 | 5 / 6 | 1.612 → 1.604 s / 1.841 → 1.840 s（未改） |
| Beatport 歌单 250+chart / 曲目 500 | 6 / 5 | 1.817 → 1.212 s / 1.541 → 0.927 s |
| Apple Music 歌单 / 曲目，各 500 | 5 / 5 | 1.556 → 1.554 s / 1.576 → 1.574 s（未改） |
| NAS Subsonic / WebDAV / Plex 列表及曲目 | 各 1 | 0.301–0.302 → 0.301 s（未改） |
| 原生 TIDAL 列表 / items 首屏 | 各 1 | 0.302 → 0.302 s（未改） |

原始模拟测量输出见 `stream-cold-20261109-measurements.txt`。这是完整取回后可交给网关排序的时间；真实 HTTP 首屏还包括 BPM 丰富（原有最多约 400 ms 同步窗口）、本地评分、排序及 JSON。不将 SoundCloud likes 原先 >40 s 的异常宣称为已修复；本轮只对能确定独立请求的 Web 歌单详情缩短等待。

## 点歌 → 可播放链路

1. Engine 对本地 TIDAL 兼容网关请求 `playbackInfo` 或 `streamUrl`。`knownCodec` 对 Spotify/SoundCloud/Beatport、或允许有损回退的曲目先 `Jobs.Fetch`，完整文件出来后才确定 manifest codec/扩展名；已知固定 FLAC 的路径可以先给 manifest，但随后 `/v1/stream/...` 仍等完整文件。
2. `Jobs.Fetch` 查内存/持久来源索引，现在也查合法的 provider 完成文件；同 provider+track 的请求共享 flight。调用方取消不会终止已启动的下载。网易云 Deemix 模式先匹配 Deezer，其他模式遵循原 provider 获取/回退规则。
3. Deezer 经本机 deemixclient `POST /api/queue` → sc5000-deemixd 单下载槽 → 登录/媒体地址/CDN 流式解密到 `.deemix-<id>-*.part` → 非空/格式检查 → rename 为最终音频 → completed。client 默认每 1 s poll，看到完成后 SafePath/Inspect。directory 适配器还有两次稳定检查，不能直接暴露未完成文件。
4. 网易云 official 先 AudioURL，再下载到临时文件；Spotify/Beatport/SoundCloud 原下载器也先完成并检查，再发布缓存名。SoundCloud 长音频已有分段/Range 并行下载，不再额外叠加推测性音频并发。
5. `Fetch` 同步补封面、记录来源，排队分析/导入（播放不等分析），然后网关 `http.ServeFile` 提供已完成文件、Content-Length/Range。Engine 还会在 `/data/inMusicTempFiles` 写自己的副本；“服务端文件可用”不是“Engine UI 已可播放”。

已实现的磁盘命中回归测试：全新 Jobs、无来源索引、有验证过的最终音频文件且封面服务有 300 ms 延迟；Deezer/网易云/Spotify/Beatport 分别 **1.286 / 0.204 / 0.189 / 0.167 ms** 返回路径，全部 **0 上游请求**。同时覆盖 provider 同号隔离、网易云 official 不取共享 Deezer 文件、临时文件和路径穿越拒绝；原 Deemix 共享测试通过。缓存文件若没有嵌入封面，本次直接命中不会为它补图；网关原缩略图路径不受影响。

未实现：

- **边下边播**：Deezer 确实逐块写临时文件，但现有 ServeFile 按打开时尺寸响应，增长文件会给错误的总长度/Range 边界；MP4/HLS 重封装可能结束后才有可解码容器；Engine 是否等整个传输落盘也没有实机证据。要先用隔离合成流验证 Engine 对渐增 FLAC/MP3、Content-Length、尾部 Range、取消/重试的行为。
- **浏览选中预取**：现有浏览请求不能可靠区分“列出一行”和“真正选中”；没有前台/预取优先级与带宽抢占器，deemixd 又只有 1 个下载槽，错误预取可能直接延迟 LOAD。应先确认选中信号，再做独立可取消、低优先级、有界调度。
- **轮询缩短**：queue 默认 1 s 带来 0–1 s 发现延迟（均值约 0.5 s）。可评估完成通知/长轮询或只对本机健康 daemon 使用更短 poll；本轮没有增加轮询 QPS。
- **跨平台缓存搜索**：只保留已有来源索引的共享规则；不新增“同数字 ID/相似标题即共享”，也不跨网易云 official/Deemix 模式借文件。

## 验证、备份与回滚

编译机检查通过：`gofmt -l .` 无输出；`go vet ./...` 退出 0；`go test -race ./pkg/providers/... ./pkg/webui/... ./pkg/deemixclient/... ./internal/...` 全通过；上述冷启动、预算共享、总数陈旧/缺失、429/取消、缓存隔离及 TIDAL 直通模拟通过。另在编译机用 mock subprocess 检查测量脚本两个模式的参数构造、输出白名单及不调用音频接口，没有连接碟机。控制台输出均限宽/限行。首次输出管道被 head 截断后外层返回 SIGPIPE 141；远端测试均成功且三份证据已全部 fetch，141 不是构建/测试失败。

改前 WebDAV 备份 ID：

- `20261109T072451Z-arm-host-3-stream-cold-20261109-before-cf8725988902-providers.tar.gz`
- `20261109T072452Z-arm-host-3-stream-cold-20261109-before-4ca309497f7d-jobs.go`
- `20261109T073112Z-arm-host-3-stream-cold-remote-before-3b387a71a183-sc5000-s2-20261109-remote-before.txt`（远端任务目录原本不存在）

无需修改配置、服务、数据库。PM 若以后部署，回滚到基线二进制即可；元数据缓存格式没变，不必清缓存。远端只删除本任务创建的 `/root/build/sc5000-s2-20261109`，不触碰其他 agent 目录或固定实验材料。

## PM 空出碟机后执行

脚本 `tools/measure-stream-cold-20261109.py` 仅调用指定的 `sshctl run "$BUILD_HOST" --no-stdin --argv sh -c 'deck L/R ...'`（`BUILD_HOST` 为编译机的 ssm 别名）。不切 scope、不登录、不清缓存、不改文件；元数据 GET 仍可能沿原逻辑触发最多 12 张封面的后台内存预热，并非完全没有出站请求。输出仅 code/TTFB/total 或匿名文件数/字节数。UUID 从当前 Engine 已显示歌单取得，不输出列表响应、日志正文、媒体地址或任何凭据。

```sh
# 左/右分别用当前 scope 下已经显示的真实 playlist UUID。
python3 tools/measure-stream-cold-20261109.py --deck L --playlist <UUID> --repeat 3 | cut -c1-200 | head -n 40
python3 tools/measure-stream-cold-20261109.py --deck R --playlist <UUID> --repeat 3 | cut -c1-200 | head -n 40

# PM 手动 LOAD 前启动被动观察；脚本自身不请求音频/manifest。
python3 tools/measure-stream-cold-20261109.py --deck L --observe-seconds 30 | cut -c1-200 | head -n 40
```

列表脚本逐项测 ping、歌单目录、favorites、likes、tracks 首屏、items 首屏/第二页。连续三次主要反映现场缓存：要测冷路径，选从未打开的歌单/账号，不能把热缓存结果写作冷启动。全局排序分别由 PM 在 Engine 选好后重复，确认跨页顺序一致。

点歌实测须由 PM 手动记录 LOAD 时间和 Engine 显示可播放时间，并同步取被动样本。被动观察只 stat 默认内部 Stream 和 Engine 临时目录的匿名汇总（1 s 采样）；若实际配置其他缓存盘，需要改为该已确认目录。后台有其他读写时无法归因，文件完成/副本增长也不能代替 UI ready 的证据。脚本刻意不请求 `playbackInfo`、`streamUrl`、`/v1/stream`，因为即使 HEAD 也可能触发真实下载。真实点歌等待及增长文件可播放性留待 PM 释放碟机后验证。
