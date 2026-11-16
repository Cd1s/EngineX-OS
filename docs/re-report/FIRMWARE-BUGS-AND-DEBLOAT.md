# SC5000 Engine OS 5.0.4 默认固件静态扫描：原厂“屎山代码”与未修复 Bug 审计报告

> 扫描对象：官方 `SC5000-5.0.4-Update.img` 解包 rootfs (`/usr/Engine/Scripts/*`, `/lib/systemd/system/*`, `/usr/Engine/Engine` 内嵌 SQL 与字符串) 及社区已证实未修复缺陷 (`EOS-01` ~ `EOS-15`)。

---

## 一、从 5.0.4 默认固件内直接抓出的原厂“屎山代码”与严重缺陷

### 1. `/usr/Engine/Scripts/engine` 强行把千兆网口降速到 100Mbps
- **原厂代码（第 32 行）**：
  ```sh
  /usr/bin/az01-ethernet-speed 100
  ```
  而在第 102 行（`TestApp`）和第 109 行（`ControllerMode`）却写着：
  ```sh
  /usr/bin/az01-ethernet-speed 1000
  ```
- **后果**：正常打碟模式下，SC5000 的千兆以太网被原厂脚本硬生生锁死在 **100Mbps**，导致局域网曲库加载、Engine DJ 同步、Pro DJ Link 传输速度被砍掉 90%！
- **修复**：在 `rootfs-overlay` 及 `sc5000-debloat` 中将正常模式的网口速率恢复为 `1000`（支持自适应回退）。

### 2. `/usr/Engine/Scripts/setup-prerequisites.sh` 的灾难级中断亲和性（IRQ Affinity）分配
- **原厂代码**：
  ```sh
  # Setting GPU to CPU 1
  /usr/Engine/Scripts/set-irq-affinity.sh ffa30000 1
  # Setting Audio to CPU 3
  /usr/Engine/Scripts/set-irq-affinity.sh ffb20000 3
  # Setting Network to CPU 1
  /usr/Engine/Scripts/set-irq-affinity.sh eth0 1
  # Control surface is on USB (not UART) so setting USB to CPU 1:
  /usr/Engine/Scripts/set-irq-affinity.sh ehci_hcd 1
  /usr/Engine/Scripts/set-irq-affinity.sh ff540000 1
  ```
- **后果**：
  1. 原厂把 **Mali GPU (`ffa30000`)、以太网 (`eth0`)、USB 控制器 (`ehci_hcd` / `ff540000`，同时跑 U 盘读写和面板控制器)** 全部塞在同一颗核心 **`CPU 1`** 上！一旦边读取 U 盘大文件或网络同步，一边滑动 60fps 波形，GPU 和 USB 中断在 `CPU 1` 上激烈抢占，直接导致触盘卡顿与波形掉帧（对应 `EOS-04` / `EOS-05`）。
  2. 原厂把 **音频 I2S (`ffb20000`)** 放在了 `CPU 3`！如果不修正这一条，后台任务绑在 `CPU 3` 就会与实时音频中断相撞。
- **修复**：
  - 在 `sc5000-perf-tune`（设为 `After=engine.service`）及覆盖脚本中重排 4 核分工：
    - `CPU 0`：**独占实时音频 (`ffb20000`，`chrt -f -p 99`)**；
    - `CPU 1`：**独占 Mali GPU (`ffa30000`) + Qt6 渲染线程**；
    - `CPU 2`：**独占 USB 存储/控制面板 (`ehci_hcd`, `ff540000`) 与千兆网络 (`eth0`)**；
    - `CPU 3`：**专供后台低优守护进程 (`rb-bridge`, `stream-sync`, `engine-guardian`, `sshd`，配合 `SCHED_IDLE`)**。

### 3. `/usr/Engine/Scripts/encrypt-fs.sh` 每次启动 Engine 无脑 `rm -rf /data/downloads /data/stems`
- **原厂代码**：
  ```sh
  function encryptDirectory() {
    dir="/data/$1"
    rm -rf "$dir"
    sync
    mkdir -p "$dir"
    fscryptctl set_policy "$key" "$dir"
  ```
  且每次开机用 `/dev/urandom` 生成一次性随机密钥，加密失败还会调 `/usr/Engine/Reporter` 甚至触发 `az01-update --erase-data`（恢复出厂清空数据）！同时在 `#!/bin/sh` 下混用 `function` 和 `[[ ]]` 非 POSIX 语法。
- **修复**：在 `sc5000-debloat` 中移除自动触发 `--erase-data` 的危险分支，并将我们的流媒体常驻下载仓指向 `/media/sc5000-sd-cache/Downloads`（避免被原厂 `/data/downloads` 每次重启清空）。

### 4. `/usr/Engine/Scripts/engine.service` 缺失脚本、无限 Core Dump 写盘与死循环失效
- **原厂代码**：
  - `ExecStopPost=/usr/Engine/Scripts/cleanup`：该文件根本不存在（实际文件叫 `cleanup-JP07.sh`），每次停止服务都会报 `203/EXEC` 错误。
  - `LimitCORE=infinity` + `gzcp.sh`（`/bin/pixz -2 -p 4`）：一旦崩溃，调用 4 线程 `pixz` 往用户的演出 U 盘上压缩几十 MB 的 Core Dump，随后阻塞在 `/usr/Engine/Reporter --coreDump --wait`。
  - `engine` 脚本外层写了 `while [ 1 ]; do ... break; done`，末尾无条件 `break`，所谓的循环完全是摆设。
- **修复**：提供 `engine.service.d/override.conf` 设 `LimitCORE=0`，提供 `/usr/Engine/Scripts/cleanup` 软链指向 `cleanup-JP07.sh`，屏蔽 `Reporter` 阻塞上传。

### 5. `/usr/Engine/Scripts/setup-usb-gadget.sh` 语法错误与网络守护冲突
- **原厂代码**：第 404 行写了 `shift1`（少了一个空格的语法错误），注释里甚至把自家品牌拼成 `"Dinon DJay"`。
- **网络服务冲突**：`systemd-networkd` 与 `connman` 同时管理 `eth0` 导致双 DHCP 竞争；`systemd-resolved` 与 `connman` 冲突反复重启。
- **遥测与后台噪音**：`Engine` 二进制内嵌向 `inmusicbrands.com` (Logstash) 和 `PostHog` 发送遥测数据。

### 6. 原厂 SQLite `m.db` / `hm.db` 数据库层面的 5 个结构性缺陷
1. **`PlaylistEntity` 外键无前导索引**：仅有 `INDEX(nextEntityId, listId)`，没有以 `listId` 开头的单列或复合索引（除了唯一约束 `(listId, databaseUuid, trackId)`），在按 `(listId, nextEntityId)` 递归遍历或按 `listId` 删除时效率低下。
2. **`PlaylistEntity` 单向链表 (`nextEntityId`) 脆弱性（`EOS-01`）**：`trigger_before_delete_PlaylistEntity` 含有 `WHEN OLD.trackId > 0`，当无效行被删时链表直接断裂；且无插入触发器保護，一旦出现断链、多头或环，歌单直接丢歌或卡死。
3. **`PlaylistPath` 视图中的 `O(N^2)` 递归 CTE 子查询（`EOS-04`）**：`PlaylistPath` 在 `ROW_NUMBER() OVER (ORDER BY (SELECT COUNT(*) FROM ...), (SELECT position FROM ...))` 中对每一行重复执行递归 CTE，歌单多时浏览极慢。
4. **`trigger_PerformanceData_after_update_Track_timestamp` 遗留不存在的列名**：5.0.4 已将 `isAnalyzed` 移至 `Track` 表，但触发器仍写着 `AFTER UPDATE OF trackData, isAnalyzed, ... ON PerformanceData`。
5. **`hm.db` 历史库无限膨胀（`EOS-14`）与 `albumArtSourceHash` 封面回归（`EOS-09`）**：历史记录从不清理导致开机 Sync 慢达十余分钟；5.0 新增 `albumArtSourceHash` 在未完成迁移或大图时拖慢加载。
