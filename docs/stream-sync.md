# Stream Sync（SC5000 / Engine OS 5.0.4）

这是独立、可选的触控控制台，不改原厂 Deck、按键、Cue 或默认歌单。
Go 服务完成歌单浏览、跨平台匹配、本地下载请求、WAV/FLAC/MP3 解码、
BPM / Beatgrid / Camelot 调性 / RGB 波形分析及 SD 卡 SQLite 事务入库。
不包含任何 DRM 解密代码，也不实现 Deemix 本身。

## 构建与测试

```sh
go test ./... -v
go test -race ./...
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o bin/stream-sync-armv7 ./cmd/stream-sync
file bin/stream-sync-armv7
```

所有依赖均支持纯 Go，包括 `modernc.org/sqlite`、`mewkiz/flac`、`go-mp3`
和二维码生成器。测试自行合成音频，不依赖 ffmpeg、网络账号或设备。
SQLite 测试使用本工作树捕获的完整原厂 SQL，包含触发器和链表约束；
只删除 SQLite 自动维护、不能显式创建的 `sqlite_sequence` DDL。

## 设备启动

先让 Engine 在常驻 SD 卡建立原生曲库，备份 **整个 Engine Library**
（在线备份应使用 SQLite backup API 或一致性快照，不能只复制 WAL 模式下的 m.db）。
程序只打开已有、UUID 非空、schema 为 **3.0.2** 的 m.db，拒绝未知版本，
不创建替代库、不迁移设备 schema、不更改已有 journal_mode。

```sh
./stream-sync-armv7 \
  --database '/media/sc5000-sd-cache/Engine Library/Database2/m.db' \
  --downloads /media/sc5000-sd-cache/Music/Stream \
  --deemix-url http://127.0.0.1:6595 \
  --deemix-pid <本地下载进程PID> --strict-cpu \
  --spotify-client-id <注册的公开PKCE客户端ID> \
  --spotify-redirect-url https://<已注册的设备回调地址>/oauth/spotify/callback
```

`--listen :8080`、`--public-url http://sc5000.lan:8080` 为默认值。
域名解析由现有 LAN DNS/mDNS 提供，本程序不修改 DNS。
启动终端显示随机控制台访问码；设备和手机在首页输入同一个码即可操作。
也可显式设置 `--access-token`，应通过受限启动环境管理，避免共享 shell 历史。
控制台 Cookie 为 HttpOnly / SameSite=Strict，HTTPS 下为 Secure；跨 Origin 写请求被拒绝。
纯 HTTP LAN 访问没有传输加密，仅用于可信网络；需要时在已有反代上终止 HTTPS。

所有账号凭据均留在服务端，不在 sessions/jobs 响应或错误中回显。
主人 Spotify 默认读写 `~/.config/spotify/token.json`；支持 `access_token`、
`refresh_token`、`expiry`（RFC3339）、`expires_at`（Unix 秒）和 `client_id`。
过期 token 用公开客户端 ID 刷新，PKCE 不要求 client_secret。
主人网易云 cookie 存在 `~/.config/stream-sync/netease-owner.json`。
凭据文件用 0600 临时文件原子替换。

每个平台可以有一个主人会话及多个独立访客会话（共最多 16 个）。
访客从不加载主人的 token/cookie，从不持久化登录信息，8 小时到期或
点击“结束账号会话”后不可再浏览/提交。已经排队的曲目可继续入库；
下载文件和 `[Guest / 临时加歌]` 列表保留在 SD，登出不会删除音乐。
主人新下载进入 `[Stream / 临时下载]`，不改变原歌单。凭据失效时可随时
点击“重新扫码登录”，不必删除主人凭据文件。

### 网易云

需在机内另外运行 NeteaseCloudMusicApi 兼容服务，默认 `127.0.0.1:3000`，
可用 `--netease-url` 更改。使用 `/login/qr/key`、`/login/qr/create`、
`/login/qr/check`、`/login/status`、`/user/playlist`、`/playlist/detail`
和批量 `/song/detail`。cookie 放在 POST body，避免访问日志 URL 泄漏。
使用完整 trackIds 分批读取，避免 `/playlist/detail` 的简略 tracks 截断。
控制台显示二维码，每 2.5 秒轮询扫码状态，也可手动检查；过期后可重新登录。

### Spotify

二维码包含普通 OAuth 授权 URL；不是 Spotify Device Code 协议。
state / verifier / redirect / 10 分钟有效期绑定到具体会话，回调 state 单次消费。
手机扫码要求**手机可访问且 Spotify 允许的已注册 HTTPS 回调**；
Spotify 不接受普通 `http://sc5000.lan` 作为远程 OAuth redirect。
需要已有 HTTPS 反代把 `/oauth/spotify/callback` 转发到设备，或预先载入
主人 token；设备本地浏览器可使用 Spotify 允许的 loopback callback。
权限为 `playlist-read-private playlist-read-collaborative`，应用开发模式
允许的账号、Spotify API 配额及可用范围仍由 Spotify 平台控制。
分页只在配置的 API origin 上构造请求，不跟随 API 返回的任意 next URL。

### Deezer

读取公开用户歌单（在控制台填公开 user ID）或直接打开公开 playlist ID。
公开目录接口不需要 cookie。搜索支持 ISRC endpoint 和艺人/标题查询。
本实现不承诺读取 Deezer 私人歌单，也不把 ARL 交给下载器。

## 音频来源（按顺序尝试）

音频全部下载到碟机本机，云端服务只传登录凭证，不经手音频。

1. **碟机本机 Deemix**（默认 `127.0.0.1:6595`）：下文的 `queue` / `directory` 协议。网易云、
   Spotify、Deezer 曲目都先走这里，不占网易云的月度下载额度。
2. **网易云直连**：第一步失败时，用碟机已登录的网易云账号取官方音频地址（`lossless` →
   `exhigh` → `standard`），试听片段一律不算。其他平台的曲目先按标题、版本、共同艺人和
   ≤5 秒时长匹配网易云曲目。访客只用自己的登录，不碰主人的 cookie。

本仓库不实现音频解密。解密只发生在外部下载器里。

网易云文件存为 `netease-<id>.flac`，经 `Inspect` 校验后才可用，重启后直接复用。没有 SD
库时落在内部存储，总量上限 3 GiB，超出后淘汰最旧的。

MP3 回退默认开启，标签写实际码率（如 `[128k]`），`--allow-320k=false` 可关闭。

### 配置

- `state/deemix-config.json`：`relayUrl`（默认 `https://denon.cheek.day`）和 `deemixPassword`。
  密码没有默认值，也不进仓库，只存在这个 0600 文件里。
- 控制台首页、`/m/deezer`、`GET/POST /api/relay`（碟机本机为 `/v1/sc5000/relay`）和
  `POST /api/ui?relayUrl=&deemixPassword=` 都可修改，保存后立即生效。
  密码只能写入，读取时只返回 `deemixPasswordSet`。地址必须是 https，或内网 IP 的 http。
- 中继请求带 `X-Deemix-Password` 和 `Authorization: Bearer`，不跟随重定向。

### 不自动写入插着的库

`--database` 不存在时**不会**扫描 `/media/*`。Engine 一直开着插入介质上的
`m.db`/`hm.db`，运行中从外部写入曾导致库损坏后被 Engine 删除（见提交 b63795b）。
没有配置库时曲目照常播放，只是不入库。

## 云端扫码登录

Spotify 和 Deezer 的二维码指向 `--relay-url`（默认 `https://denon.cheek.day`），手机不用连局域网：

- 碟机 `POST /api/relay/session`，body 为 `{"token","provider","spotifyClientId"}`，3 秒超时。失败只记日志。
- 二维码内容是 `<relay>/m/<provider>?t=<token>`。
- 碟机轮询 `GET /api/relay/session?t=<token>`，返回 `status`（`scanned` / `done`），
  `done` 时带 `spotifyToken` 或 `deezer: {userId, arl}`，碟机存入 state 目录。

`--relay-url=` 留空时回退到局域网页面 `http://<碟机>:8080/m/<provider>?t=<token>`，
页面接受短链、资料页、用户 ID、`arl` Cookie、中继地址和密码。`arl` 向 Deezer 确认账号后，
随资料一起存为 `deezer-owner.json`（0600）；stream-sync 自身不用它下载，供本机 Deemix 读取。

## 本地 Deemix 适配协议

Deemix 各版本没有统一的 HTTP 队列协议，部分只提供 Socket.IO。
**默认 queue 模式需要已有服务/桥接器实现以下 REST 契约**，并非声称
任意原版 Deemix 的 6595 端口都直接提供这些接口。

```http
POST /api/queue
Content-Type: application/json

{"url":"https://www.deezer.com/track/123","bitrate":9,"quality":"FLAC","allowLossy":false}
```

响应（HTTP 2xx）：

```json
{"id":"job-123","state":"queued","progress":0}
```

`GET /api/queue/job-123`：

```json
{"id":"job-123","state":"completed","progress":1,"path":"123.flac"}
```

progress 范围是 0..1。支持 queued/downloading/completed/complete/done；
error/failed/cancelled/CantStream 会明确失败。path 可以是相对于 downloads
的路径或该树内的绝对路径；现存符号链接必须也落在同一树内。
队列 ID 不能包含路径/查询分隔符。API 路径可用 `--deemix-queue` 更改。
`--deemix-socket` 支持 Unix socket 上的同一 HTTP 协议。

没有状态 API 时可选择 `--deemix-mode directory`：

- POST `/api/download`，JSON body 为 `url`、`bitrate:9`、`allowLossy`；
  需要版本对应的本地服务接受此提交格式，2xx 表示已接受。
- 下载器输出模板必须为 **Deezer 数字 ID + 正确扩展名**，例如
  `downloads/123.flac`（不是任意子目录或艺人/标题命名）。
- 下载器先写临时文件，完成后原子发布最终名称。客户端只观察指定 ID
  的文件，连续两轮大小/mtime 稳定后验证头部；绝不选择目录中“最新的一首”。
- 后续完整 FLAC 解码还会验证 STREAMINFO 样本数及每帧 CRC，部分下载不入库。
  不满足此提交/命名/原子发布契约的下载器需由外部 HTTP 适配器接入 queue 模式。

Spotify 优先用 ISRC，兼顾版本标记和已知时长；相互矛盾的 ISRC 不按
名字强行匹配。没有可用 ISRC 时，清洗 Explicit、括号中文译名后要求
标题、完整艺人列表一致、两边时长非零且误差 ≤2000ms。Live 可用于清洗
搜索，但仍保留录音版本约束；Club/Radio/Extended/Remix 等版本必须相同。
无法区分多个候选时拒绝下载。Deezer 原生曲目直接使用其目录 ID。

无损不可用时默认接受 MP3 回退，`--allow-320k=false` 可关闭；每一个 MP3
音频帧都必须为 MPEG1 Layer III 320k，低码率/VBR 不会伪装成 `[320k]`。
检查实际文件头，不相信扩展名或 API 音质字段。Comment 标注
`[FLAC] <位深>/<采样率>` 或 `[320k] MP3 ...; FLAC unavailable`。
CantStream / 低码率 / 未授权降级均在 UI 失败状态中明确显示。

## 资源与入库边界

下载/分析串行运行，等待队列最多 16 个，历史最多 100 个；每任务默认
30 分钟超时。DSP、解码、入库位于锁定的独立 Linux OS 线程；CPU3、
SCHED_IDLE、nice 19、IOPRIO_CLASS_IDLE 通过 x/sys/unix 系统调用设置。
Go 进程启动时也将所有现有线程应用同一护栏，GOMAXPROCS=1，避免 GC
和辅助 HTTP 线程任意占用 CPU0-2。工作线程结束即销毁，不泄漏调度状态。
`--deemix-pid` 把下载服务当前所有线程也应用护栏；新线程从其父线程继承。
下载器已有 cgroup/服务 CPUAffinity 设置时，仍须核对所有子进程/线程。
本程序不能仅凭 HTTP 请求把另一个未配置的服务限制在 CPU3。

缺少 CPU3/无权限会记录并展示降级，尽量落在允许的最后一个非 CPU0
核心。设备上应加 `--strict-cpu`，亲和性不符时拒绝执行任务。
`--throttle-work 40ms --throttle-sleep 10ms` 为默认协作节流；解码和
DSP 都周期检查取消。单首单声道 PCM 最多 32 Mi 样本（128 MiB，44.1kHz
约 12.7 分钟）；超过上限明确拒绝，避免整机 OOM。内存还需预留解码帧、
FFT、波形、数据库与 Go runtime，不能把 128 MiB 当作整个进程的硬上限。
长混音应由其他已验证的导库路径处理。

数据库写入使用 immediate SQLite 事务、外键、FULL 同步与 1.5 秒锁等待。
Track、PerformanceData、Playlist、PlaylistEntity 同步提交；忙/失败时
整笔回滚，下载音频保留。只接受同一 SD 卷内音频并写相对路径。
原厂触发器负责新 Playlist 的 nextListId 拼接，客户端维护成员 nextEntityId
链。重复导入不重复建曲目/成员；已有分析只补 Comment/歌单，保持其
PerformanceData；未分析但已锁定拍线的条目保留已有 beatData/BPM，
补齐波形与文件元数据。从不创建、修改或覆盖 Hot Cue/Loop。

### 已确认格式与仍需实机验收的部分

按报告的真实布局生成 qCompress（BE 长度 + zlib）：44-byte trackData、
3099-byte RGB Overview、两组混合字节序 Beatgrid（24-byte marker）。
默认/调整拍线都按样本数写入；未知保留字段及未确认的尾部取零。
Track.key 使用既有分析器的五度圈枚举，不把 trackData 候选字段读取结果
反向覆盖显示 Key。

报告明确指出 **trackData 枚举/三个 double、Overview 颜色通道顺序和末
bin 意义尚未通过像素输出验证**；本实现的枚举与低/中/高→RGB 属推断。
高分辨率三频峰值/RMS 波形由分析器生成并在 Web waveform API 中返回；
真实 5.0.4 schema **没有 highResolutionWaveFormData 列**，没有捏造
同名列或声称已确认原厂高分辨率 sidecar 格式。入库提供的是已知结构
的 RGB overview 与 beatData；原厂高分辨率显示路径还需设备验证。

SQLite 提交不等于 Engine 已刷新其内存中的歌单视图，本程序没有向 Qt
私有 IPC 发送未验证消息。设备可能需要重新进入歌单或由原厂重新分析。
没有连接 SC5000、刷机或在真实 Deck 1 播放时测量 XRUN/Qt 帧时间；
**不能将本机测试通过等同于“0 爆音、0 掉帧、立即刷新及原厂高分辨率
波形已验收”**。部署验收需核对 CPU0-2 的 Engine/ALSA/Qt 分配、CPU3
下载进程树、双 Deck 音频、触摸帧时间、RGB 显示/拍线、Key 和重新挂盘。

## 碟机上的三个独立来源

Source 和 Settings → Services → Streaming 显示网易云音乐、Spotify、Deezer 三项（`tools/ui-mod`）。
它们共用 Engine 唯一的 TIDAL 客户端，点哪项，网关的 `scope` 就切到哪项，只返回该服务的歌单和搜索结果；
未登录时点击来源会在屏幕上弹出二维码：网易云用其 App 直接扫码（官方 unikey 流程，不再需要 NeteaseCloudMusicApi）；
Spotify 和 Deezer 没有同类流程，二维码打开本机的一次性手机页（`/m/<provider>`）。Spotify 需要自行注册应用的 Client ID，
Deezer 连接的是公开资料页。Deezer 公开榜单无需登录；网易云搜索需要登录。

## 碟机原生 TIDAL 界面

`tools/az01-packer/unlock_jp07_features.py` 把 Engine 内置 TidalAPI 的
`https://api.tidal.com/v1/` 与 `https://auth.tidal.com/v1/` 改写为
`http://127.0.0.1:8080/v1/` 与 `http://127.0.0.1:8080/au//`。Stream Sync
在同一端口应答（`pkg/webui/tidal_gateway.go`，双斜杠自动折叠）：

- 设备码/令牌登录直接成功，订阅为 `PREMIUM_PLUS` / `LOSSLESS`；
- 歌单 = 已登录的 `[网易云]`、`[Spotify]`、`[Deezer]` 歌单 + Deezer 精选；
  尚未登录网易云/Spotify 时首项提示访问 `sc5000.lan:8080` 扫码；
  重启后自动恢复已保存的 owner 登录；
- 搜索并发查询 Deezer / 网易云 / Spotify 的曲目与歌单；
- 每首曲目分配进程内稳定的正整数 trackId，播放信息返回
  `application/vnd.tidal.bts` 清单，指向 `/v1/stream/<id>.flac`；
- 首次播放时经 Deemix 下载（同一曲目并发请求共用一次下载），直接以
  `http.ServeFile`（支持 Range）返回，同时排队 CPU3 分析并导入
  `[Stream / 临时下载]`（访客会话为 `[Guest / 临时加歌]`）。

网关没有控制台访问码，因此只接受回环地址请求；`/api/` 仍需访问码。
Engine 解析 TIDAL JSON 的字段集合未在实机抓包验证，以上为按公开
TidalAPI 格式的实现，需在 SC5000 上验收浏览、搜索与播放。

## 本次验证记录

在 IN 工作树完成 `go test ./... -v`、`go test -race ./...`、`go vet ./...`
及内嵌 JavaScript 语法检查。Go 1.27.1 交叉构建记录包含
`GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0`；file 确认为
ELF 32-bit ARM EABI5、statically linked。二进制验证后清理，不提交产物。
查IN（Gemini）对 GPT 实现做了只读复核；报告的问题已修复并补回归，
复核结论为无新增 P1/P2。此记录不改变上文的外部 API 契约与实机验收边界。
