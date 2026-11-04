# Stems 远程分轨：现状、修复与验收（2026-11-03）

负责：本机 Paseo Opus 5.5 实现者（stems/relay/worker 与本文件）。共享的 `tools/ui-mod/build_packs.py`、`engine_ui_hook.c` 归 IN 上的 Opus（`e4e285aa…`），本文只记录需求，不改这两个文件。
证据分四类标注：**设计**（文档/方案）、**代码**（仓库）、**部署**（机器上的二进制/服务）、**实测**（真实会话或测试输出）。

## 1. 用户需求（本轮）

1. 按原厂方式使用 Stems：曲库里 Collection 下有独立的「Stems」行（用户已给官方界面对照图）。加载已完成分轨的曲目后，双按 HOT CUE 模式键，上排 1–4 分别为 Vocals/Melody/Bass/Drums，默认四轨全开（[官方说明](https://support.denondj.com/en/support/solutions/articles/69000862383-engine-dj-stems-setup)；SC6000/5000 共用同一份[用户手册](https://cdn.inmusicbrands.com/engine/N420/SC6000%20PRIME%2C%20SC6000M%20PRIME%2C%20SC5000%20PRIME%2C%20SC5000M%20PRIME%20-%20User%20Guide%20-%20v4.2.0%20%28RevB%29.pdf)）。不新增按钮或替代播放页。
2. 远程分轨要真正可用：clawbot 下载在线曲、先算好分轨再交给碟机；USB 曲只在必要时上传一次；缓存命中不重新计算；失败、取消和进度如实显示。
3. 整体要快，每一段的耗时都要有记录。

## 2. 实测基线（只读，2026-11-03 21:40–22:00 +07）

### 2.1 UI 与操作

- 当前「曲庫」标题栏的蓝框 Stems 入口是**上一轮自定义加上去的**（`build_packs.py` 中的 `STEMS_HEADER_ENTRY`）：点按只是按 stems 列排序，拖放是加入原厂渲染队列。它不是原厂的 Stems 列表，也不是旧版 UI 导致的。
  恢复方案：IN Opus 删除这个假入口，改用 REA 定位的原厂列表分支（file `0x6d3e00`，`beq` 改为 `b`，见 IN `/root/build/sc5000-compat-20261103-rea-stems/out/STEMS-HANDOFF.md`）。**截至本文，碟机尚未部署这个改动。**
- 原厂列表的筛选条件是 `stemsStatus != 0`，包括排队（1）、处理中（2）和完成（3）。所以列表里出现某首曲目，不代表它已经算完或能播放。
- HOT CUE 二次按键（代码证据，IN `/root/build/sc5000-lab/5.0.4/tree/usr/Engine/AssignmentFiles/PresetAssignmentFiles/JP07/JP07_Controller_Assignments.qml:154`）：JP07 的 `PadModeSelect` 只登记 4 个模式键（note 27/30/28/29），每个键的 primary/secondary 模式表在 C++ 设备配置里。REA 已证实（STEMS-CONTROL-HANDOFF，进行中）：再按一次同一个 primary 键时会查 secondary 并做资格检查；只有 `STEMS/CUES_STEMS/AUTO_STEMS` 多一道检查，读 `DeckStemsModeHandler+0x134`，它调用的 feature 接口与 `hasStemsPreRenderedPlayback` 相同。JP07 secondary 数组的取值仍在补证。
  **REA 已闭合（2026-11-03 晚，IN `…/out/STEMS-CONTROL-HANDOFF.md`）**：JP07 设备配置 `FUN_00f7586c`（file `0xf6586c`）中，primary 为 `[CUES, LOOPS, ROLL, SLICER]`，secondary 为 `[STEMS, AUTO, SAMPLER, FIXED]`，`padModeSelectType=3`（Rgb）。所以 HOT CUE 键原厂就映射到 STEMS，硬件和映射都没有缺口，**不能另造映射**。剩下要查的是当前曲目的 `Stemmed`、资格检查与 feature 门，以及 `.stems` 是否有效。
  实机要读的只读状态：`/Engine/DeckN/Track/SongLoaded`、`/Track/Stemmed`、`/Engine/DeckN/Pads/View`、`Planck.hasStemsPreRenderedPlayback()`。原调试通道已撤，播放中不为读这些状态重启 Engine。**HOT CUE 双击无反应的根因尚未闭合。**

### 2.2 链路各段

| 段 | 设计 | 实际部署 / 实测 |
|---|---|---|
| 在线曲下载 | 用户期望在 clawbot 下载 | **未实现**。relay 从第一次提交 c24a140（11-02）起对 `/api/audio` 等一律返回 410；在线曲由碟机本机的 `sc5000-deemixd` 下载。git 里从来没有远端缓存方案 |
| 分轨输入 | — | Engine 在碟机上解码，float32 PCM 实时逐块上传：deck → `relay.example.com`（碟机上解析为 192.168.1.6:443，即路由器边缘）→ clawbot relay :6595 → stemsd :6596 → `worker.py` |
| 分离器 | — | numpy/scipy 频谱 HPSS / 声像近似，**不是神经网络模型**（clawbot 上没有 torch/demucs）。四轨相加等于原曲 |
| 打包 | — | Engine 在碟机上自行编码并加密，写入 U 盘 `Engine Library/Stems/<trackId> <uuid>.stems` |
| 缓存 | — | **无**。同一首重新渲染会全量重新上传、重新计算 |
| stemsd 监听 | 仓库写的是 127.0.0.1 | clawbot 实际监听 `0.0.0.0:6596`（有 token 鉴权）。本轮未改 |

### 2.3 11-03 的真实会话（relay 日志 + Engine 日志 + 文件时间互相印证）

| trackId | 时长 | 格式 | relay 会话 | 速度 | 客户端结束 | .stems |
|---|---|---|---|---|---|---|
| 5955 | 428 s | FLAC | 13m48s | 0.52× 实时 | 日志轮转，不可考 | 34.5 MB，13:22 |
| 235 | 231 s | MP3 320 | 6m22s | 0.60× | relay 结束后**正好 120 s** `i/o timeout`，退出码 1 | 18.5 MB，14:18 |
| 5493 | 260 s | MP3 320 | 7m04s | 0.61× | 同上，正好 120 s | 21.0 MB，14:29 |
| — | — | — | 4m50s | — | Engine 重启，会话被杀 | 无 |

- **已证明的收尾阻断**：后端已经完成（trailer `ok`），碟机客户端却收不到响应结尾，等满 idle 120 s 后报错。结果每首多花 2 分钟，并且 Engine 看到的是子进程失败。
- 那 3 个 `.stems` 文件中，有 2 个是在客户端报错的会话里写出来的。`stemsStatus` 由 Engine 的 SQL 函数 `GET_STEMS_STATUS` 计算，很可能只看文件是否存在。因此**不能把它们当作可播放**，要用实机加载和逐轨静音来验证。
- 速度对照：clawbot 回环直连、60 s 音频、1024 帧一块的 lockstep 跑法，用时 10.6 s（5.7× 实时）；在 IN 上直接跑 worker 约 12× 实时。实机会话只有 0.5–0.6× 实时，所以瓶颈在碟机 → 边缘 → relay 这条路的逐块往返或碟机侧，不在算力。

## 3. 本轮修复（代码已完成，IN 测试通过）

**应用层结束帧**：不再依赖可能被边缘代理吞掉的 HTTP trailer 或 EOF。

- 客户端请求头加 `X-Stems-Framing: 1`。stemsd 把 worker 的输出包成 `'D' + uint32 长度 + 字节`。**只有** `worker.Wait()` 成功且拷贝无错时才发 `'E' + "ok"`，否则发 `"failed"`。relay 只透传这一对请求头/响应头。Engine 收到的字节流和原来逐字节相同。
- `internal/stems/protocol.go` 的 `Session` 按 `worker.py` 的方言解析两个方向，检查：init 的声道数和块大小、每块 payload 长度、`numReturnChannels == 4×声道`、done 之后以 `returnFrames=0` 收尾、`FramesOut == FramesIn`、两个方向都停在消息边界上。
  客户端只有在 `E=ok` **并且** `Session.Complete()==nil` 时才成功退出；中途断网、后端失败、结尾截断都会失败退出。
- 兼容性：旧客户端、旧 relay 或旧 stemsd 遇到这一路时，自动退回原来的 trailer 判定。
- 成功时客户端往 stderr 写一行汇总，Engine 日志会收录：`remote stems: ok <帧> frames at <Hz> in <块数> blocks (max <maxBlockSize>), <秒>`。第一次真实渲染就能拿到 Engine 实际的块大小和逐块耗时。
- `cmd/stems-probe`：在任意机器上扮演 Engine，lockstep 发块，done 之后**保持 stdin 打开**，计时子进程何时退出，再用 `Session` 校验。

改动文件：`internal/stems/{stems.go,protocol.go,protocol_test.go,stems_test.go}`、`cmd/sc5000-relay/{stems.go,stems_test.go}`、`cmd/stems-probe/main.go`。

**测试**（IN，`/root/build/sc5000-stems-remote-20261103`）：`gofmt`、`go vet`；`go test -race -count=5 ./internal/stems`、`go test -race ./cmd/sc5000-relay` 全部通过。覆盖的情形：

- 一个 HTTP/1.1 边缘：上传不 EOF 就不结束回复；Engine 端 done 之后 stdin 不关。新客户端照样立即完成，而且边缘这时仍挂着。
- worker 退出码 1、结尾缺 `returnFrames=0`，都判为失败。
- 帧大于 64 KiB、帧超过 1 MiB 上限、短写，以及流被切碎送达的情况。
- 少回帧、缺 done、停在 payload 中间、块超过 `maxBlockSize`、返回声道数不对，都被拒绝。

IN 上用真实 `worker.py` 跑 20 s：新、旧客户端都在 done 之后 0.05 s 退出（这条路上没有边缘）。

## 4. 部署记录

| 时间（UTC） | 目标 | 内容 | 备份 / 回滚 |
|---|---|---|---|
| 15:04 | <relay-host> | `/opt/sc5000-stems/sc5000-stemsd` 换成 sha `4dff3c2d…`，`/usr/local/bin/sc5000-relay` 换成 sha `095de783…`；重启这两个服务，单元和 worker 未改 | 前置备份 `20261103T150337Z-<workstation>-before-stems-framing-20261103-38aa93e7f6a8-<relay-host>-sc5000-stems-relay-prestate.tar.gz`（sha `38aa93e7…`，已 get 回来核对 sha 和归档成员）。部署前确认没有 worker 在跑。部署后两个服务都是 active，`/healthz` 和 stemsd `healthz` 正常 |
| 15:11 | 碟机 | `/data/sc5000-custom/bin/stems-processor` 换成 sha `77c1948a…`（第一版） | 前置备份 `20261103T151048Z-<workstation>-before-stems-client-framing-20261103-fa60a01e684f-sc5000-stems-processor-3044ec.tar.gz`（tar sha `fa60a01e…`，内含旧客户端 sha `3044ec15…`，已 get 回来核对） |
| 15:14 | 碟机 | 换成 sha `6c5d3397…`（v2：Engine 一个字节都没发就关 stdin 时，按握手处理，返回 0） | 同上。中间版本 `77c1948a…` 可从源码重建，不单独备份 |

碟机替换步骤：确认没有 `stems-processor` 进程 → 复制到同目录临时文件 → `chmod 755` → `sync` → `mv -f` 原子替换 → 读回 sha。`/usr/local/bin/stems-processor` 仍是指向它的链接，sha 一致。Engine 未重启（PID 25208 前后相同），DB、U 盘 `.stems` 和路由器都没动。
冒烟测试：`stems-processor -s -hv 1.0.0 -pv 1.6.0 </dev/null` 返回 0、输出 26 字节，与旧版行为相同。

碟机回滚（可独立执行）：

```sh
agent-backup get 20261103T151048Z-<workstation>-before-stems-client-framing-20261103-fa60a01e684f-sc5000-stems-processor-3044ec.tar.gz --output pre.tar.gz
tar -xzf pre.tar.gz   # 得到 data/sc5000-custom/bin/stems-processor，sha 3044ec15…
# 传到碟机 /tmp 后，在没有 stems-processor 进程时执行：
cp /tmp/stems-processor /data/sc5000-custom/bin/.stems-processor.new && chmod 755 /data/sc5000-custom/bin/.stems-processor.new && mv -f /data/sc5000-custom/bin/.stems-processor.new /data/sc5000-custom/bin/stems-processor
```

clawbot 回滚（可独立执行）：

```sh
agent-backup get 20261103T150337Z-<workstation>-before-stems-framing-20261103-38aa93e7f6a8-<relay-host>-sc5000-stems-relay-prestate.tar.gz --output pre.tar.gz
# 传到 <relay-host> 后：
tar -C / -xzf pre.tar.gz usr/local/bin/sc5000-relay opt/sc5000-stems/sc5000-stemsd
systemctl restart sc5000-stemsd sc5000-deemix-relay
```

新后端向后兼容，碟机现有客户端不受影响，所以回滚只在后端本身出问题时才需要。

## 5. 验收清单

| # | 项目 | 状态 |
|---|---|---|
| A1 | 收尾：同一条真实路径上，旧客户端是否在 done 后正好 120 s 才退出、新客户端是否立即退出（碟机上用 stems-probe 跑 20 s 合成音频） | **通过（合成音频）**，见 5.1 |
| A2 | 碟机换上新客户端，用一首真实短曲走 Engine 渲染全链路：Engine 日志出现 `remote stems: ok`、没有 `i/o timeout`、`.stems` 写出、记录每段耗时 | **渲染链路通过**，见 5.2；四轨可播仍属 A3 |
| A3 | 加载 A2 的曲目：`Track/Stemmed` 为真，双按 HOT CUE 进入 Stems pad，四轨分别静音/独奏都生效（听感）；重启后仍然有效 | 待做（依赖 REA 闭合和原厂列表补丁） |
| A4 | 失败要如实：渲染中断开后端 / 取消，Engine 显示失败，不留下当成功的状态 | 待做 |
| A5 | 原厂 Stems 列表恢复（IN Opus）：Collection 下有独立 Stems 行，假入口删掉 | IN 已改，未部署 |
| A6 | 已有的 3 个 `.stems`（5955/235/5493）是否可播：只读加载验证，不删除 | 待做 |
| B1 | 速度：拿到 Engine 的实际 maxBlockSize 和逐块往返后再决定是否做客户端流水线（见 6.2） | 待 A1/A2 数据 |
| B2 | 缓存与在线曲远端预取（见 6.1） | 设计中 |

### 5.1 A1 实测（碟机 → relay.example.com → 192.168.1.6 → relay → stemsd，2026-11-03 15:07–15:10 UTC）

各跑一次，均为 20 s 合成立体声 44.1 kHz，`-block 1024`，lockstep，done 后 stdin 保持打开，外层限时 180 s。开始前确认碟机没有 `stems-processor` 进程、clawbot 没有 worker 进程。

| 客户端 sha | 块数 | 帧 入/出 | 协议校验 | 连接 | 逐块等待合计 | done 前耗时 | done 后退出 | 退出码 | relay 记录 |
|---|---|---|---|---|---|---|---|---|---|
| 旧 `3044ec15…` | 862 | 882000/882000 | ok | 0.86 s | 23.87 s（27.7 ms/块） | 25.85 s | **120.00 s** | 1，`read tcp …->192.168.1.6:443: i/o timeout` | 15:07:57 结束，25.5 s |
| 新 `77c1948a…` | 862 | 882000/882000 | ok | 0.74 s | 23.55 s（27.3 ms/块） | 25.40 s | **0.04 s** | 0，`remote stems: ok … 25.4s` | 15:10:33 结束，25.1 s |

结论：

- relay 在 25 s 时已经结束会话，旧客户端仍要再等 120 s，所以卡住的地方在 relay 之后，即边缘到碟机这一段。新协议不依赖这一段转发结尾。
- 速度：真实路径每块往返约 27.5 ms，而 1024 帧只有 23.2 ms 音频，所以在 lockstep 下只能到 0.77× 实时。clawbot 回环约 3.8 ms/块（5.7× 实时）。这和 11-03 实机会话的 0.5–0.6× 吻合，Engine 实际块大小待 A2 的汇总日志确认。
- IN 测试（同一份代码）：worker 退出码 1 → 失败；缺结束帧 → 失败；正常流程 → 成功。另有 `TestFramedSessionFailures`、`TestFramedSessionEndsWithUploadOpen`、`TestSessionRejectsBadStreams`、`TestReadFramesSizesAndShortWrites`，`-race` 全部通过。
- **A1 用的是 v1 `77c1948a…`，现在部署的是 v2 `6c5d3397…`，v2 没有在真实链路上重跑 A1。** v2 相对 v1 只改了 `Session.Complete()` 的一个分支：Engine 一个字节都没发（`!init` 且输入流空闲）、后端又发了 `E=ok` 时，按握手返回成功。只要会话里有任何输入字节，判定就和 v1 完全一样；发了半个 init 头的情况仍然判失败，有反例测试。v2 在 IN 上 `go test -race -count=3 ./internal/stems`、`-race ./cmd/sc5000-relay` 都通过。碟机冒烟：`</dev/null` 返回 0、输出 26 字节。
- **这是合成音频的链路验收，不等于真实 Engine 短曲渲染（A2）和四轨播放（A3）通过。**

### 5.2 A2 实测：真实 Engine 渲染（2026-11-03 22:16–22:23 +07，客户端 v2 `6c5d3397…`）

用户指定的曲目在 DB 快照里只有一条：trackId 3117「A day by atmosphere supreme」/ Nujabes，MP3 320，239 s（超过 3 分钟，按用户指定没换），`../Contents/Nujabes/Kei Nishikori meets Nujabes/Nujabes - A day by atmosphere supreme.mp3`，9689457 B。由用户加入渲染队列，全程只读监看。

| 项 | 值 |
|---|---|
| 客户端启动 | 22:16:23（pid 4401）。Engine 写临时文件 `/tmp/EngineDJ/Stems/3117 <uuid>.mp4`，同时开着该 mp3 |
| Engine 日志 | `22:22:57 remote stems: ok 10570476 frames at 44100 Hz in 10323 blocks (max 1024), 394.1s`。没有 `i/o timeout`，没有 Stems Processor Error |
| 帧数核对 | 10570476 / 44100 = 239.7 s，与曲长 239 s 一致 |
| Engine 块大小 | maxBlockSize = 1024，平均每块 1024 帧 |
| 速度 | 394.1 s 处理 239.7 s 音频，0.61× 实时；每块 38.2 ms（probe 只有 27.5 ms，Engine 侧每块大约多 10 ms） |
| relay | `15:22:57 UTC POST /v1/engine 200 6m33.771s` |
| 收尾 | `.stems` 22:22:56 写出，客户端 22:22:57 成功退出，**没有 120 s 空等** |
| 产物 | `Engine Library/Stems/3117 157073fc-71ee-4995-9980-777ebb41b3ad.stems`，19310922 B，sha256 `e3b07d74…40b4` |

结论：

- 真实 Engine 会话的收尾问题已修好。
- 速度仍受逐块往返限制：0.61× 实时，和 11-03 旧会话的 0.60× 相同，只是不再多等 2 分钟。
- 每块耗时接近「往返 + Engine 侧开销」，所以 Engine 很可能是一块一块等回复的 lockstep 方式。这是推断，没有直接证明。
- **四轨能不能加载、静音和独奏是否生效（A3），要用户加载这首、双按 HOT CUE 后靠听感确认，本项没有覆盖。**
- 另：`1 157073fc….stems`（21.9 MB，21:53）是 22:11 替换客户端之前、用旧客户端生成的，来源和有效性不明，没动它。

### 5.3 A3 用户反馈与只读核查（2026-11-03 22:23–22:30 +07）

- 用户反馈：加载 3117 后，双按 HOT CUE 没有反应（原厂操作，JP07 secondary=STEMS）。**A3 未通过。**
- 22:23:40 Engine 日志 `OfflineAnalyzer: Analyzed "Nujabes - A day by atmosphere supreme.mp3"`，晚于 `.stems` 写出的 22:22:56。所以加载发生在分轨完成之后，不是 deck 还缓存着分轨前的版本。
- 22:28:02 `/proc/<Engine>/fd`：开着 3117 的源 mp3（2 个）和它在 `/data/inMusicTempFiles` 的临时副本，**没有任何 `.stems` 文件**。作对照，渲染期间能看到 Engine 开着 `/tmp/EngineDJ/Stems/3117 ….mp4`。日志里没有 stems 解码或解析错误。
- 推断（**不是运行时值**）：曲目按普通音频加载，没有走 stems 解码路径。这和 feature 门（与 `hasStemsPreRenderedPlayback` 同一接口）在加载时为 false 相符，也可能是 Engine 惰性加载。另有一条待 REA 核验的线索：旧 unlock 脚本的 `stemsPreRenderedPlayback` 改动（file `0xF62B10/0xF62ED0`）可能改在了别的机型路径上。
- 最小诊断方案已交主控（只读 QML 采集 Stemmed/Pads/View/feature + 一次不在播放时的重启 + 用户加载一次、双按一次）。需要和 IN Opus 的原生列表补丁合并进同一次维护，我不改共享文件。

## 6. 下一步设计（未实现）

### 6.1 缓存与在线曲预取

- 分轨子进程协议里**没有曲目身份**，Engine 只给 `-s -hv -pv` 参数和 PCM。如果只按 PCM 哈希建缓存键，必须先把整首传完才算得出来，做不到「只上传一次」。
- **前置条件（待证，未满足之前不实现「只上传一次」）**：必须证明当前 `RenderStemsJob` 和当前子进程之间存在唯一、强绑定的曲目身份。不能从 Engine 的 fd 里挑第一个或最新的：播放的 Deck、分析和分轨可能同时打开不同的源文件，绑错会把另一首歌的分轨交给用户。也不能只用开头一段（可能是静音）的指纹。绑定之后还要用整段的音频长度、边界和帧数校验。**没有这种绑定时，仍然整首上传 PCM，不假装做到了只上传一次。**
- 绑定成立之后，键 = 源文件 sha256 + 分离器版本（`worker.py` 的 sha）+ 采样率/声道。源文件一改哈希就变，分离器升级时版本字段让旧缓存失效。
  客户端在会话开始时带上这个键，后端命中就直接回放缓存的四轨，碟机不再上传 PCM（Engine 的块照样读掉）。没命中就照常上传，后端算完、**只有收到 `E=ok`** 才发布缓存。失败或取消不写缓存。
- 在线曲：clawbot 按服务曲目 ID 用用户已授权的账号下载同一份文件，整首离线分轨并预热缓存。它和碟机下载的文件逐字节相同时，源文件哈希才对得上。不同则退回 USB 路径，不猜对齐。
- 缓存体积：四轨 float32，7 分钟一首约 590 MB，至少要压成 int16 或 FLAC。这在实现前另行估算 clawbot 磁盘。

### 6.2 速度

如果 A1/A2 证实 Engine 是一块一块 lockstep 等回复，那么客户端可以先用缓冲里已有的帧回复（worker 本来就会先回 0 帧，Engine 已经接受过这种行为），上传和回收异步进行，done 之后再按 `maxBlockSize` 分块补完剩余帧。这需要实机验证 Engine 能容忍整首延迟的情况，**不先做**。

## 7. 未验证 / 风险

- HOT CUE 二次按键的前提（JP07 secondary 数组、Stemmed/feature 门）还没闭合，任何能力开关都**不做全局打开**。
- 120 s 空等具体卡在边缘的哪一层，还没在真实路径上做直连与域名的对照（A1）。新协议不依赖这个归因。
- 用户曲库 DB 尚未备份，本轮只读复制 `m.db`/`stm.db` 到本机 `/tmp` 查时长，没有写碟机上的 DB。
