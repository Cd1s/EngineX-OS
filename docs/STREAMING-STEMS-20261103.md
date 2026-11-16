# 流媒体曲目的远端分轨：来源支持矩阵与实施边界（2026-11-03）

用户新增需求：网易云、Spotify、Deezer 等在线曲也能用 Stems，由计算机（中继主机）下载、分析好后再传给碟机。
本文只按代码与部署证据写，不推测。代码位置引自只读审计。

## 1. 硬边界

- **不调用、迁移或扩展 Deezer 解密路径。** `internal/deezer`（gateway → `media.deezer.com/v1/get_url` → CDN → Blowfish `decryptStripe`）属于流加密解密，新功能一律不接。
- **Spotify 只提供目录、元数据和可选的 30 秒预览。** 官方没有下载完整音频的入口，并禁止 stream ripping（[Get Track](https://developer.spotify.com/documentation/web-api/reference/get-track)）。Spotify 只能用于歌单/元数据匹配，界面和缓存必须标出实际音源，**不称 Spotify 原音**。
- 只用用户已有的有效账号，以及官方允许取得的、未受保护的完整文件；不绕过付费或权限。凭证留在现有位置：碟机 `state/*.json`、relay `config.json`，不复制新的密钥文件，日志里不打印。
- 授权失效、来源不可用、匹配不唯一时，如实返回失败或「待选择」，**不悄悄换成别的歌**。
- `.stems` 只能由 Engine 自己编码加密。**不在远端伪造加密的 `.stems`**，也不把 4 个 WAV 冒充成原生可播的分轨。

## 2. 当前链路事实（只读核对，不改动）

用户纠正（2026-11-03）：下载都走 deemix，网易云也优先 deemix。碟机当前设置是 `netease_mode=deemix`，已核对；本轮不改设置，也不动 Deemix 服务和账号。

| 来源（界面上） | 实际下载器 | 音频来自 | 碟机上的文件 |
|---|---|---|---|
| Deezer | 碟机 `sc5000-deemixd`（127.0.0.1:6595） | Deezer 媒体 CDN，碟机本地解密（`internal/deezer`） | `<DeezerID>.flac/.mp3` |
| Spotify | 同上，用 Spotify 歌单和元数据去匹配 Deezer：先按 ISRC，再按「标题+艺人+时长 ±2s+版本词，且结果唯一」 | 匹配到的 Deezer 曲目；Deezer 失败时，如果有网易云登录，就回退到网易云 official | `<DeezerID>.*`，不保存 Spotify ID |
| 网易云（当前 `deemix` 模式） | 同上，先匹配 Deezer | 匹配到的 Deezer 曲目；失败时回退到网易云 official 直链 | `<DeezerID>.*` 或 `netease-<id>.*` |
| SoundCloud | 碟机 stream-sync 进程内 | 官方 `api.soundcloud.com`（可下载的原文件，或 mp3/aac 串流） | `soundcloud-<id>.*` |

- relay 的音频接口返回 410，远端目前不下载任何音频。
- 补充事实：Engine 链接 FFmpeg libavcodec 58.134.100。
  - 3117 这首 mp3 用 x86 上的 ffmpeg 7.1 解码，与 Engine 实际送来的总帧数一致（10570476），gapless 修剪也相同。
  - x86 上 ffmpeg 4.4 和 7.1 的解码结果逐字节相同（sha `8bc80c3e…`）。
  - armhf ffmpeg 4.4.2（qemu）解码结果不同（sha `b5e68e34…`）。
  - 所以想复用远端解码结果去对齐 Engine 的 PCM 不可靠。本项不再推进。

## 3. 新增功能能做什么（与上面的现有链路分开）

**范围**：用户自有文件，或官方允许取得、未受保护的完整音频，在中继主机上远端分轨，结果作为**独立的四轨文件**交回。

**不做**：
- 新功能不调用、迁移或扩展 `internal/deezer` 解密路径，所以 Deezer、Spotify 和网易云 deemix 这些经 Deezer 下载的音频，不由中继主机远端重新取音。
- 碟机上**已经下载好的文件**属于用户本地文件，可以上传一次做远端分轨，命中缓存后不重算。
- 不接入原厂 Stems 播放：不做原生缓存接入，不改许可或功能校验，也不伪造 `.stems`。

| 输入 | 新功能 | 说明 |
|---|---|---|
| 碟机/USB 上的文件（含已下载的流媒体文件） | 支持：上传一次 → 远端整首离线分轨 → 缓存 → 取回四轨 | 缓存键是文件 sha256，同一文件再提交直接命中 |
| 未受保护的直链（例如 SoundCloud 可下载原文件） | 暂不做：URL 拉取有 SSRF 风险，范围也更大 | 先用上传路径覆盖 |
| Deezer 解密得到的音频 | 远端不取音 | 只能用碟机上已有文件走上传路径 |

## 4. 原生 Stems 播放

按用户和主控的指示，本线不再推进原生 Stems 入口，也不做许可相关修改。远端结果是独立的四轨文件，**不等于原厂 Stems 模式可播**。

## 5. 实现（隔离 worktree `../sc5000-streaming-stems`，在 IN 构建测试，未部署）

- `sc5000-stemsd` 新增离线任务：
  - `POST /v1/jobs`：上传文件，可带 `X-Stems-Sha256` 期望值，不符就拒绝。
  - `GET /v1/jobs/{id}`：查询状态 `queued / running / done / failed / canceled` 和进度。
  - `DELETE /v1/jobs/{id}`：取消。
  - `GET /v1/jobs/{id}/{vocals|bass|drums|melody}.flac`：取回四轨。
- `tools/stems/separate_file.py`：ffmpeg 解码 → 复用 `worker.Separator` 流式计算（内存有界）→ 四路 24-bit FLAC，并核对输入、输出帧数相等。
- 缓存：键 = 文件 sha256 + 分离器版本（`worker.py` 与 `separate_file.py` 的 sha）。只有校验通过的结果才原子发布；失败或取消的结果不写入缓存。
- relay 用控制台密码鉴权，把这些路由转发到 stemsd。
- 验收：合成素材或自有素材，测首算、命中、取消、坏输入、sha 不符被拒。

### 5.1 IN 实测（2026-11-03 15:47–15:55 UTC，隔离 worktree，**未部署**）

产物 sha：

| 产物 | sha |
|---|---|
| `sc5000-stemsd` | `6909443b…` |
| `sc5000-relay` | `f2e13c8c…` |
| `separate_file.py` | `a288f864…` |
| `worker.py` | `56e5119d…`（未改） |

IN 和中继主机的 ffmpeg（7.1.5）、numpy（2.2.4）、scipy（1.15.3）版本相同。

| 项 | 结果 |
|---|---|
| 首算 3117（用户自有 mp3，239.7 s） | 17.6 s，约 14× 实时（IN 16 核；中继主机 8 核待测） |
| 四轨输出 | 4 个立体声 24-bit FLAC，每轨 10570476 帧，与源一致；合计 112.7 MB（vocals 22.1 / bass 11.5 / drums 39.7 / melody 39.4 MB） |
| 同一文件再提交 | 200 `cached:true`，0.023 s，不运行分离器 |
| 按 sha 查询（不上传） | `GET /v1/stems/{sha}` 200，1 ms |
| 取回四轨（回环） | 0.14 s |
| 峰值内存（分离器 + 5 个 ffmpeg） | 4 分钟曲 337 MB，15 分钟曲 348 MB，不随曲长增长 |
| sha 不符 | 400，不进队列 |
| 坏输入（随机字节） | failed（`separator failed: exit status 1`），不写缓存 |
| 取消（2 分钟文件，进度 28% 时） | canceled，2.06 s 内结束，无残留进程和临时文件，不写缓存 |

测试：
- Go 单元测试（IN，`-race`），`internal/stems` 和 relay 的 Stems 用例均通过，覆盖：
  - 首算后缓存命中、sha 不符、超出大小限制（413）、空上传、缺 token；
  - 分离器失败或少写一轨时判失败，且失败结果不缓存；
  - 取消运行中和排队中的任务；
  - 分离器版本变化后缓存失效，超出容量时淘汰旧结果；
  - 经 relay 用控制台密码访问可用，不带密码返回 401。
- Python `test_separate_file`（真实 ffmpeg）：左右声道内容不同的立体声、单声道、坏输入；四轨相加还原原曲（误差 < 1e-4），总帧数一致。`test_worker` 照旧通过。

**未做与限制**：
- 未部署到中继主机。部署需要改 unit：加 `StateDirectory=sc5000-stems` 和 `-jobs-dir %S/sc5000-stems`，`TasksMax` 提到 64（一次运行会起 5 个 ffmpeg 加 FFT 线程）。部署前先备份。
- 碟机侧未接入：下载完成后谁来上传、四轨放在哪里、界面怎么呈现，都需要改共享的 `pkg/webui/jobs.go` 或 UI，方案先交主控。
- URL 拉取暂不做。
- 结果是独立的四轨文件，**不是原厂 Stems 模式**。
