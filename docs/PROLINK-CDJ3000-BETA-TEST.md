# PRIME 4 ↔ CDJ-3000 Pro DJ Link beta test / 互联测试手册

Issues: [#59](https://github.com/Cd1s/EngineX-OS/issues/59) (read CDJ media), [#63](https://github.com/Cd1s/EngineX-OS/issues/63) (export to CDJ).
The link has so far only been tested against the virtual CDJ-3000 in [OpenProLink](https://github.com/Cd1s/OpenProLink); this is the first real-hardware run.
目前只和 OpenProLink 里的虚拟 CDJ-3000 联调过，这是第一次真机测试。

Each test lists **what to do** and **what you should see**. Write `PASS`, `FAIL` or `N/A` next to every step in the report template at the end. A photo of the CDJ screen helps a lot when something fails.
每一项都写了**怎么做**和**应该看到什么**。在文末模板里逐条填 `PASS` / `FAIL` / `N/A`；失败时拍一张 CDJ 屏幕最有用。

---

## 0. Prerequisites / 准备工作

**Hardware / 硬件**

- PRIME 4 running EngineX OS (`Settings → About` shows `EngineX OS 5.3.0.x`).
- CDJ-3000 on current Pioneer firmware, with a USB stick exported from rekordbox.
- One USB stick or SD card in the PRIME 4 that holds either an Engine DJ library or a rekordbox export (FAT32/exFAT, writable).
- A network switch (any unmanaged gigabit switch) **or** one Ethernet cable directly between the two units. Do not use Wi-Fi for this test.
- 已刷 EngineX OS 的 PRIME 4（`设置 → 关于` 显示 `EngineX OS 5.3.0.x`）；装有最新官方固件的 CDJ-3000，插一个 rekordbox 导出的 U 盘。
- PRIME 4 上插一个 U 盘或 SD 卡，里面有 Engine DJ 曲库或 rekordbox 导出（FAT32/exFAT，可写）。
- 一台普通千兆交换机，**或**一根网线直连两台设备。本测试不要用 Wi-Fi。

**Network / 网络**

- Both units must be in the same subnet: either both get an address from the same router's DHCP, or both have no DHCP and fall back to `169.254.x.x` (link-local). A direct cable gives link-local on both, which is fine.
- Plug the cable into the PRIME 4's **LINK / Ethernet** port and the CDJ-3000's **LINK** port.
- 两台必须在同一网段：要么都从同一个路由器 DHCP 拿地址，要么都没有 DHCP、自动落到 `169.254.x.x`（直连网线就是这种情况，没问题）。网线插 PRIME 4 的网口和 CDJ-3000 的 **LINK** 口。

**Settings on the PRIME 4 / PRIME 4 上的设置**

`Settings → Device` (the EngineX rows at the bottom) / `设置 → 设备`（底部的 EngineX 选项）：

| Setting / 设置 | Value | Used by |
|---|---|---|
| `Pro DJ Link Library` / `Pro DJ Link 曲库` | **On** | Test 2 (#59) |
| `Pro DJ Link Export to CDJ (Beta)` | **On** | Tests 1 and 3 (#63) |

**Player numbers / 机号**

- On the CDJ-3000 set `Player No.` to **3** (or 4) in its `UTILITY` menu.
- The PRIME 4's export picks the highest free number from 4 down to 1 by itself: with the CDJ on 3 it becomes **Player 4**; with the CDJ on 4 it becomes 3. The PRIME 4's own four decks are not Pro DJ Link players, so nothing else needs a number.
- Never leave the CDJ on `AUTO` for this test: two units racing for the same number is the first thing we want to rule out.
- CDJ-3000 在 `UTILITY` 里把 `Player No.` 设成 **3**（或 4），不要用 `AUTO`。PRIME 4 的导出会自动从 4 往下挑一个没人用的机号：CDJ 是 3 时它就是 **4 号机**。PRIME 4 自身的四个轨道不是 Pro DJ Link 机位，不需要另外编号。

**Before you start / 开始前**

1. Power on both units, connect the cable, wait 30 seconds.
2. Run the self-check (section 4). Every line should be `PASS`; a `WARN` for an unused feature is fine. If you see `FAIL`, fix that first or send the report as is.
3. 两台开机、接好网线、等 30 秒，先跑一次第 4 节的一键自检。应该全是 `PASS`（没用到的功能出现 `WARN` 没关系）；有 `FAIL` 先按提示处理，处理不了就直接把报告发来。

---

## Test 1. Discovery & tempo / beat sync / 设备发现与 BPM、拍子同步

| # | Do / 操作 | Expect / 预期 |
|---|---|---|
| 1.1 | Open the self-check report. / 打开自检报告。 | Under *Pro DJ Link devices* the CDJ-3000 is listed with its number and a `Seen` age under 2 s. / *Pro DJ Link devices* 里有 CDJ-3000，`Seen` 小于 2 秒。 |
| 1.2 | On the CDJ press `LINK`. / CDJ 上按 `LINK`。 | The PRIME 4 appears as a player (number 4). / 列表里出现 PRIME 4（4 号机）。 |
| 1.3 | Load and play a track on PRIME 4 deck 1, press its `SYNC` / tempo master so it is master. / PRIME 4 一号轨放歌并设为 Master。 | The CDJ shows the PRIME 4 as tempo **MASTER** and its BPM. / CDJ 显示 PRIME 4 是 **MASTER**，并显示其 BPM。 |
| 1.4 | On the CDJ load a track and press `BEAT SYNC`. / CDJ 放歌并按 `BEAT SYNC`。 | The CDJ's BPM matches the PRIME 4 within 0.1 and the beats stay in phase for 2 minutes (listen with both channels up). / CDJ 的 BPM 与 PRIME 4 相差不超过 0.1，两路一起听 2 分钟拍子不跑。 |
| 1.5 | Move the PRIME 4 pitch fader by about +3 %. / PRIME 4 推速 +3% 左右。 | The synced CDJ follows within one beat. / CDJ 一拍之内跟上。 |
| 1.6 | Press `MASTER` on the CDJ. / CDJ 上按 `MASTER`。 | Master moves to the CDJ; the report shows `Master = yes` on the CDJ's row only. Only one master at any time. / Master 交给 CDJ；报告里只有 CDJ 一行 `Master = yes`，任何时刻只有一个 Master。 |
| 1.7 | Hand master back by making a PRIME 4 deck master again. / 再把 Master 交回 PRIME 4。 | The CDJ shows the PRIME 4 as master again; the CDJ, if synced, follows it. / CDJ 再次显示 PRIME 4 是 Master。 |
| 1.8 | On-Air: only with a Pioneer DJM on the same link. / 只有接了先锋 DJM 时才测。 | Raising the CDJ's channel lights its On-Air ring; otherwise write `N/A`. / 推起 CDJ 那一路，On-Air 灯亮；没有 DJM 写 `N/A`。 |

---

## Test 2. Browsing CDJ-3000 media on the PRIME 4 & shadow decks / 在 PRIME 4 上读取 CDJ 曲库与影子轨道

How it works: with `Pro DJ Link Library` on, the PRIME 4 copies the CDJ's rekordbox library (tracks, waveforms, cues) over the network onto the PRIME 4's own drive, at most once every 10 minutes per CDJ slot, keeping 2 GB free on the drive. The copy shows up **after that drive is mounted again**, as a playlist named like `CDJ-3000 3 USB`. The drive must already hold a rekordbox export (`PIONEER/rekordbox/export.pdb`).

原理：`Pro DJ Link 曲库` 打开后，PRIME 4 每 10 分钟最多一次，把 CDJ 上 U 盘的 rekordbox 曲库（曲目、波形、Cue）通过网络复制到 PRIME 4 自己的盘上（盘上至少留 2 GB）。复制完成后要**把这个盘重新插拔一次**才会出现，歌单名类似 `CDJ-3000 3 USB`。这个盘上必须已经有 rekordbox 导出（`PIONEER/rekordbox/export.pdb`）。

| # | Do / 操作 | Expect / 预期 |
|---|---|---|
| 2.1 | Self-check report, CDJ row, `NFS` column. / 自检报告 CDJ 那一行的 `NFS` 列。 | `port N` (the CDJ's media server answered). An `error:` here means the PRIME 4 cannot read the CDJ at all. / 显示 `port N`；显示 `error:` 说明根本读不到 CDJ。 |
| 2.2 | Insert the rekordbox USB in the CDJ, wait 2–3 minutes (large libraries take longer). / CDJ 插 rekordbox U 盘，等 2–3 分钟（曲库大要更久）。 | – |
| 2.3 | Eject and re-insert the PRIME 4's drive. / PRIME 4 的盘弹出再插回。 | A `CDJ-3000 3 USB` playlist appears with the CDJ's tracks. / 出现 `CDJ-3000 3 USB` 歌单。 |
| 2.4 | Load three of those tracks. / 加载其中三首。 | Waveform and beat grid appear without re-analysis; hot cues and memory cues are where they are on the CDJ. / 不重新分析就有波形和拍线；Hot Cue、Memory Cue 位置与 CDJ 上一致。 |
| 2.5 | Shadow decks (4-deck mode). / 影子轨道（四轨模式）。 | This feature is for SC5000 / SC6000 only; on the PRIME 4 the `4-Deck Waveform` row is hidden. Write `N/A` unless you also have an SC5000/SC6000 on EngineX. / 只在 SC5000 / SC6000 上有，PRIME 4 不显示该选项，写 `N/A`（除非你另有刷了 EngineX 的 SC5000/SC6000）。 |

---

## Test 3. Exporting the PRIME 4's library to the CDJ-3000 / 把 PRIME 4 的曲库导出给 CDJ

| # | Do / 操作 | Expect / 预期 |
|---|---|---|
| 3.1 | Self-check: `Export running` is `PASS`, `udp 111` and `tcp 12523` are `PASS`. / 自检里 `Export running`、`udp 111`、`tcp 12523` 都是 `PASS`。 | – |
| 3.2 | On the CDJ press `LINK`, choose the PRIME 4 (player 4), then its `USB` / `SD`. / CDJ 按 `LINK`，选 PRIME 4（4 号机）的 `USB` / `SD`。 | The PRIME 4's playlists and tracks are listed. / 列出 PRIME 4 的歌单和曲目。 |
| 3.3 | Load three tracks on the CDJ. / CDJ 加载三首。 | Title, artist, BPM, key and artwork shown; the track plays without dropouts. / 显示曲名、艺人、BPM、调性和封面，播放不断音。 |
| 3.4 | Look at the waveform and the cue list. / 看波形和 Cue 列表。 | Full waveform, beat grid matches the beat, hot cues A–H match the PRIME 4. / 有完整波形，拍线对拍，Hot Cue A–H 与 PRIME 4 一致。 |
| 3.5 | Play a track to the end and seek around. / 播完一首、来回拖动。 | No `Unable to read` / `Disc error` message. / 没有读取错误提示。 |
| 3.6 | Eject the PRIME 4's drive while the CDJ is browsing it. / CDJ 正在浏览时弹出 PRIME 4 的盘。 | The CDJ shows the media as gone within a few seconds and does not freeze. / 几秒内 CDJ 显示介质已移除，不死机。 |

---

## 4. One-click diagnostic report / 一键收集诊断报告

**From a laptop or phone / 用电脑或手机**

1. Find the PRIME 4's address and access code: `Settings → About` shows `http://<PRIME4-IP>:8080` and the access code.
2. Open `http://<PRIME4-IP>:8080` in a browser on the same network and log in with the access code.
3. In the same browser open `http://<PRIME4-IP>:8080/api/prolink/diag?format=text`, select all, copy, paste into the GitHub issue.
4. `设置 → 关于` 里有控制台地址和访问码。在同一网络的浏览器打开 `http://<PRIME4-IP>:8080`，输入访问码登录；再在同一浏览器打开 `http://<PRIME4-IP>:8080/api/prolink/diag?format=text`，全选复制，粘贴到 GitHub Issue。

With curl / 用 curl：

```sh
curl -s -H "Authorization: Bearer <ACCESS-CODE>" "http://<PRIME4-IP>:8080/api/prolink/diag?format=text"
```

The page needs the console login because it lists the devices on your network. **Do not paste the access code itself into the issue.** Without `?format=text` the same page returns JSON.
需要登录是因为它会列出网络里的设备。**不要把访问码贴进 Issue。** 去掉 `?format=text` 返回 JSON。

**On the deck over SSH / 在碟机上（SSH）**

```sh
sc5000-prolink-diag
```

It checks the link speed, `rb-bridge` and `stream-sync`, the mounted drives and their `export.pdb`, waits 5 seconds and checks that every Pro DJ Link device sent packets in that window, then prints the full report above. The exit code is 1 when any line is `FAIL`.
检查网口速率、`rb-bridge` 与 `stream-sync`、挂载的盘和 `export.pdb`，等 5 秒确认每台设备在这段时间内都发过包，最后附上完整报告。有 `FAIL` 时退出码为 1。

The report contains the EngineX version, model and board, the two Pro DJ Link settings, IP addresses and netmasks, which Pro DJ Link sockets are open, and the Pro DJ Link devices with their numbers, names, BPM and play/master/sync state. It never contains streaming logins, cookies, tokens, Deezer ARL, Wi-Fi passwords, serial numbers or MAC addresses.
报告里有版本、机型板号、两个 Pro DJ Link 开关、IP 与子网掩码、端口监听情况和网络里各设备的机号、名称、BPM、播放/Master/同步状态；**不含**流媒体登录、Cookie、Token、Deezer ARL、Wi-Fi 密码、序列号和 MAC 地址。

**Common FAILs / 常见 FAIL**

| Line | Meaning / 含义 | Fix / 处理 |
|---|---|---|
| `Peer N … outside every local subnet` | The CDJ got a DHCP address, the PRIME 4 did not (or the other way round). / 一台有 DHCP、一台没有。 | Put both on the same router, or both on a bare switch / direct cable, then power-cycle both. / 都接同一个路由器，或都只接交换机/直连，然后两台重启。 |
| `Player numbers … used twice` | The CDJ and the export use the same number. / 机号冲突。 | Set the CDJ to 3. / CDJ 设成 3。 |
| `Export running … not running` | No drive with a library is mounted. / 没插有曲库的盘。 | Insert the USB/SD and wait 10 s. / 插盘等 10 秒。 |
| `udp 50000 … nothing listens` | `rb-bridge` is not running. / `rb-bridge` 没在运行。 | Restart the PRIME 4. / 重启 PRIME 4。 |
| `Peer N … NFS error` | The CDJ's media server does not answer. / CDJ 的媒体服务没响应。 | Check that a USB stick is in the CDJ. / 确认 CDJ 插了 U 盘。 |

---

## 5. Report template / 测试报告模板

Paste this into the GitHub issue (#59 or #63), fill each line, then paste the diagnostic report under it.
复制到 GitHub Issue（#59 或 #63），逐条填写，下面再贴诊断报告。

```text
PRIME 4 EngineX version:
CDJ-3000 firmware:
Connection: switch / direct cable / router (DHCP)
CDJ player number:

1.1 discovery on PRIME 4:      PASS / FAIL / N/A
1.2 PRIME 4 in CDJ LINK list:
1.3 CDJ sees PRIME 4 master:
1.4 CDJ BEAT SYNC to PRIME 4:
1.5 pitch follow:
1.6 master to CDJ:
1.7 master back to PRIME 4:
1.8 On-Air (DJM only):
2.1 CDJ NFS port:
2.3 CDJ playlist on PRIME 4:
2.4 waveform / cues correct:
2.5 shadow decks:              N/A on PRIME 4
3.1 export running:
3.2 PRIME 4 browsable on CDJ:
3.3 metadata / artwork / play:
3.4 waveform / hot cues:
3.5 no read errors:
3.6 eject handled:

Notes (what you saw when something failed):

<paste the diagnostic report here>
```
