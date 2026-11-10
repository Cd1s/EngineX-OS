# L1：原厂详细日志与网页下载验证（2026-11-10）

基线：`329b55dae4db8167659360811c96cd29031975e2`（5.3.0.4 候选）。只在隔离 ARM 实验环境运行；没有操作实机，没有修改共享固件或候选产物。量化证据见 [captures/verbose-logging-20261110.json](handbook-evidence/captures/verbose-logging-20261110.json)。

## 原厂机制：运行结果修正了手册里的字符串推断

选用原厂 Qt 日志过滤配置。在 **启动 Engine 之前**设置 `QT_LOGGING_CONF=/data/logging.ini`，打开时原子写入下列文件，关闭时删除：

```ini
[Rules]
*.debug=true
qt.*.debug=false
```

最后一条只关闭 Qt 内部调试，保留 Engine 调试及 Qt 警告/错误。仅 `*.debug=true` 的对照在 12 秒内产生 234,217 行、38,834,947 字节，绝大部分是 Qt 导入、布局和渲染跟踪；不适合问题排查。

原厂命令始终是：

```sh
/usr/Engine/Engine -d0 -loggerOptions "Type, Message, Thread, Category, SplitLongLines"
```

没有修改 `-d0` 或添加 `Function`、`Console`、`Colorize`；本任务没有验证这三个额外格式选项的语义。

**不能只放文件。** 不从父进程设置 `QT_LOGGING_CONF` 时，放入 `logging.ini` 得到 243 行，新增主要来自 `OfflineAnalyzer`；主进程分类没有增加。启动前设置变量后，才观察到大量主进程 Engine 分类。因此一代的既有 Engine drop-in、二代数据包的既有 Engine drop-in 都增加了该环境变量。文件不存在时默认过滤保持生效。

也实测了单独放置 `/data/LoggingCategories.ini`：纯规则 `*.debug=true` 得到 124 行，`[Rules]` 加规则得到 134 行；均未新增主进程分类。本实现不使用这条未经验证的文件通道，也不声称已证明它的厂商自定义格式。

## armhf：真实 5.3.0 Engine 的三次 12 秒对照

底包是官方 SC5000 5.3.0，解包到任务自己的目录后只读挂载。使用 `unshare -m -n -p -f --kill-child`、chroot、私有 `/data`/`run`/`tmp`、只读 CPU/显示设备信息、隔离的 IRQ 文件，按现有 `doc-shots.sh` 使用 EGLFS KMS + 软件 OpenGL；原厂 Engine 原生执行。整个实验放入 `MemoryMax=4G` 的独立临时单元，不重载宿主 systemd。

| 条件 | 行数 | 字节数 |
| --- | ---: | ---: |
| 启动环境已配置，日志文件不存在 | 116 | 18,091 |
| 原子写入上述日志文件，再启动 | 1,704 | 265,921 |
| 删除日志文件，再启动 | 125 | 19,585 |

打开后新增的真实日志分类包括 `air.planck.engine.sync`、`air.planck.streaming`、`air.planck.stems`、`air.planck.grpcserver`、`air.planck.librarian`、`air.planck.onelibrary`。原厂 `Category` 列最长显示 24 字符；证据 JSON 保留的其他长分类可能被原厂截断。行数随任务调度、缓存和设备缺失警告变化，不要求关闭前后完全相等。

**读取时机实证：** Engine PID 53 于 02:12:37.959 UTC 尝试读取 `logging.ini`（当时不存在）。02:12:45 写入文件、02:12:53 删除文件，现有 Engine/分析器没有重新打开配置，也没有给配置增加 inotify 监听；02:13:03 结束时新启动的辅助进程另行读取。结论限定为这次 25 秒跟踪：现有进程不热加载，必须重启 Engine；新进程可以读到当时的配置。

**真实设置接口联调：** 构建本分支 armhf `sc5000-suite`，在同一隔离 rootfs 运行 `stream-sync`，经 `/api/ui?verboseLog=1&restart=1` 打开、`verboseLog=0&restart=1` 关闭。验证配置内容、设置持久化、文件删除，以及两次 `systemctl restart --no-block engine.service` 调用。和现有门禁一样，systemctl 使用沙箱记录桩；实验控制器收到重启请求后停止该次 Engine 的独立进程组并重新启动。

| HTTP 联调阶段 | 新 Engine PID | 12 秒日志行数 |
| --- | ---: | ---: |
| 关闭 | 68 | 125 |
| 打开并重启 | 196 | 2,021 |
| 关闭并重启 | 322 | 114 |

这验证了设置 → 配置 → 原有重启接口 → 新 Engine 进程 → 日志变化；不替代实机 systemd、播放中断和音频恢复检查。

## AZ04：相同规则在真实 64 位 Engine 的早期日志中生效

用只读 AZ04 rootfs 的真实 Engine，Build ID `040335918db1144f0c3452d6f23088d3c3a94613`、SHA-256 `63dc843feb6738d463eb68a62c9f1bd2cd86dcb562fcaa77a4fef607fefdf0ed`。沿用现有 AZ04 bootstrap 的候选 hook，保留文件名 `libengine-ui-hook.so`；确实得到 `Engine 5.3.0-az04`、SQLite hook、14 个 DSP guard 的初始化证据。早先把库改名为 `hook.so` 会令其构造函数跳过初始化，已修正后重测，早先数据不作为最终 AZ04 对照。

| 条件 | 行数 | 字节数 |
| --- | ---: | ---: |
| 无日志文件 | 23 | 1,112 |
| 上述日志文件 | 167 | 9,448 |
| 删除日志文件 | 23 | 1,112 |

新增早期输出含 `air.planck.streaming`、`air.planck.engine.sync` 等分类注册消息。三次 Engine 都以 `engine-quit-reason=TestApp`、退出码 0 结束，未进入持续 UI/播放运行。trace 观察到 `/dev/tee0..9` 不存在，但该访问顺序**不能证明** TEE 是退出原因。仓库已有的 JP21 `sclive-aux-hardware-fixture.c` 说明二代缺少 SoC 身份信息可能进入 TestApp；本任务未为 AZ04 新增身份垫片，也未修改原厂检查。

因此能确认相同 Qt 配置解析在 AZ04 原厂启动中生效，不能据此声称 AZ04 的持续日志、热加载或播放已经验证。完整验证留给总控实机。Craft Anything 直接记录此次运行：`ev_5664b7d1fbac67cc91323563fbc4a2c68dec9ff3cda423faf7d21d20c228587a`。实验机的 Craft Anything 没有配置 AI backend，未额外安装或修改共享模型/凭据配置；结论来自原生运行与跟踪，不来自模型推断。

## journal 存储与边界

- 一代 EngineX 已有 `rootfs-overlay/etc/systemd/journald.conf.d/sc5000.conf`：`Storage=volatile`、`RuntimeMaxUse=16M`；离线 debloat 也写相同配置，不需要再加一份。
- 实测读取 SC LIVE 4 与 AZ04 原厂配置，已有 `RuntimeMaxUse=64M`。AZ04 `/var/log -> volatile/log`，`/var/volatile`、`/run` 按原厂 fstab 为 tmpfs，未预建 `/var/log/journal`，默认 journal 使用 `/run/log/journal`。
- 详细日志可能更快轮换记录，但已有容量上限。systemd 的上限按 journal 文件轮换执行，活跃文件可能令磁盘占用略超设定值；它不是整个进程的内存硬限。
- 网页接口 `GET /api/logs` 沿用控制台访问码/严格 SameSite cookie 校验。只读本次启动最近 30 分钟、最近 5,000 条，按新到旧输出；覆盖 `engine.service`、`stream-sync.service` 及二代的 `enginex-stream-sync.service`。
- 下载最多 2 MiB；journal 原始输入最多 4 MiB，单次进程最多 10 秒，同一服务器同时只允许一个下载。只输出完整、已脱敏的 journal 消息，不把被截断消息的尾片段交给浏览器，达到上限时附截断提示（计入 2 MiB）。
- 含 token、ARL、密码、cookie、Authorization/Bearer、签名凭据、API key 或本机访问码的整条 journal 消息（含同条内的多行/转义 JSON）被遮盖；stdout 拆成独立 entry 的缩进续行也保守遮盖。URL 用户信息、查询参数和片段被遮盖，兼容相对 URL、转义斜线；含百分号编码的整条消息直接隐藏，避免裸 `%`、重复编码或编码空格造成部分凭据泄露。原始 stderr 和 backend 错误不发给浏览器。未找到能复用的仓库通用日志脱敏函数，复用了既有访问码规范化函数。

## 验证与总控实机步骤

- 指定完整命令通过：`gofmt -l .` 无输出，`go vet ./...`、`go test -race ./...` 通过；overlay 18 项通过；packer 共 153 项，1 项按已有条件跳过（`test_install_uses_bundle_alsa_dir`，实验机已有 `/data`）。没有改动或删除宿主 `/data` 以强行消除跳过。
- 新增 Go 测试覆盖默认关闭、设置持久化、原子文件内容/删除、失败不重启、访问码、cookie、凭据与签名 URL 脱敏、多行消息、原始输入/下载上限、固定时间和 unit 范围、backend 错误不泄露。
- 原厂 armhf `journalctl` 已在隔离 rootfs 中实际接受接口的完整参数，退出码 0，空 journal 的 stdout/stderr 均为空；真实 systemd 服务日志内容仍需实机核对。
- 三语浏览器验证标签、取消不保存、确认请求带 `restart=1`、文本下载，全部通过。截图以沙箱返回的网页和脱敏状态数据渲染；网页截图无需 Engine 的 OpenGL。碟机屏幕 QML 没有改动。

总控在计划停播时，分别选一代机与 AZ04：

1. 安装包含本提交的候选版本，打开网页控制台并用屏幕上的访问码登录。
2. 默认应显示“详细日志”关闭；点击“下载日志”，确认是可打开的纯文本附件，包含 Engine 和同步服务，不含账号凭据。
3. 播放测试音轨，打开“详细日志”。取消提示应保持播放；确认提示应重启 Engine、中断播放，回到网页后开关保持打开。等待重新进入播放界面，复现待排查问题并下载日志。
4. 确认日志新增 Engine 调试分类，音频可恢复，设备内存稳定、journal 正常轮换；AZ04 尤其核对通过真实 SoC 身份检查后的应用层日志。
5. 关闭“详细日志”并确认重启，确认开关关闭、日志恢复常态；重新开机后仍关闭。需要回滚时先关闭开关，再回到基线提交/上一候选；不需要恢复或清空曲库。
