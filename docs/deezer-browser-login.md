# Deezer 云端扫码登录

碟机生成 Deezer 二维码后，手机打开 `/m/deezer?t=<token>`，选择「启动云端登录窗口」。在云端浏览器画面中点击输入框，用下方输入栏填入邮箱、密码或验证码；支持点击、滑动、滚动按钮和 Enter / Tab / Backspace。登录后自动返回完成页面，碟机通过现有 relay 轮询取得 Deezer User ID 和 ARL。

- CDP 固定连接服务所在机器的 `http://127.0.0.1:9222/json/version`，不对公网暴露 CDP 地址。
- 最多一个无痕 browser context；其他 token 得到 HTTP 409。
- 视口为 390 × 680；每秒刷新 JPEG，截图请求不延长空闲期限。
- 五分钟没有实际操作、二维码到期、主动关闭、登录成功、初始化失败及服务退出时均销毁 context；`disposeOnDetach` 覆盖连接丢失及进程异常退出。
- 每次截图或操作检查 Deezer 域 Cookie。仅接受格式有效的 ARL，并通过 `deezer.getUserData` 验证正数 User ID；先清除浏览器上下文，再写入 relay 会话。
- 只有经碟机密码认证的 trusted 会话同步服务器全局 Deezer 配置。手机接口不返回 ARL；原有碟机轮询协议保持兼容。
- 下方保留直接粘贴分享链接、用户 ID 和 ARL 的方式。

## HTTP 接口

接口都使用同一个有效 Deezer `t` 参数；先打开手机页面，或通过现有 relay API 注册二维码。

- `GET /m/deezer/frame?t=<token>`：可直接启动，返回 `image/jpeg`；登录完成时返回 JSON `{"ok":true,"status":"done"}`。
- 页面轮询用 `GET /m/deezer/frame?t=<token>&poll=1`：已关闭或空闲超时时返回 HTTP 410，等待用户手动重启。
- `POST /m/deezer/act?t=<token>`：`Content-Type: application/json`，支持 `start`、`stop`、`click`（`x`,`y`）、`wheel`（`dy`）、`text`（`text`）、`key`（`key` 为 `Enter`、`Tab`、`Backspace`）。成功返回 `active`、`stopped` 或 `done` 状态。

不要把真实 token、输入文字或 ARL 放入日志及验收资料。

## 验证和构建（IN）

```sh
cd /root/build/sc5000
go test ./...
go test -race ./cmd/sc5000-relay
go vet ./cmd/sc5000-relay
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/sc5000-relay-amd64 ./cmd/sc5000-relay
```

新增测试使用模拟 CDP WebSocket 和 Deezer 身份 API，无需真实账号；覆盖隔离、并发、操作转发、Cookie 域和身份校验、trusted 同步、超时回收、失败清理、慢截图客户端及 Spotify 默认 HTTPS 回调和授权范围。

## 部署回滚

部署前的原二进制备份：

`20261102T174024Z-relay-host-before-deezer-browser-login-7597eeb45476-sc5000-relay`

原二进制 SHA-256：`7597eeb4547678c51372184a54ab79f116b4a521cb815243b7586f0c8ce6d2ec`。

在 relay-host 上恢复（先下载并校验成功，再替换）：

```sh
agent-backup get 20261102T174024Z-relay-host-before-deezer-browser-login-7597eeb45476-sc5000-relay --output /tmp/sc5000-relay-rollback
chmod 755 /tmp/sc5000-relay-rollback
chown root:root /tmp/sc5000-relay-rollback
mv /tmp/sc5000-relay-rollback /usr/local/bin/sc5000-relay
systemctl restart sc5000-deemix-relay.service
systemctl is-active sc5000-deemix-relay.service
curl -fsS http://127.0.0.1:6595/healthz
```

部署前的状态目录备份（保留现有二维码和配置）：

`20261102T174854Z-relay-host-before-deezer-browser-deploy-state-a36a51a27752-sc5000-relay.tar.gz`

Spotify 默认 Client ID `1c7c775b12b642b38e0884afcfd6169c` 只在其注册过的回调上可用（`relay.example.com` 是文档占位符）。中继按当前 `-public` 生成 `<public>/oauth/spotify/callback`，首次使用会探测 Spotify 是否接受这个回调：接受就走自动 HTTPS 回调，并按“Client ID + 回调地址”记住；不接受或探测无结论就退回粘贴 127.0.0.1 地址的流程。授权范围为 `user-library-read playlist-read-private playlist-read-collaborative`。

## 本次部署验收

- 线上二进制 SHA-256：`59aae060670dbe54c89570e9a05c8e229e85095d08e7092a32f10359181cdd38`。
- `sc5000-deemix-relay.service` active，`/healthz` 正常。
- 指定测试 token 原先属于已过期的 SoundCloud 会话；通过原有 `POST /api/relay/session` 将该测试 token 重新注册为 Deezer 后，公开 `/m/deezer` 返回 HTTP 200，`/m/deezer/frame` 返回 HTTP 200、`image/jpeg`，截图为 390 × 680。
- 真实登录页面会自动跳转至 `account.deezer.com/en-us/login/`。已看到邮箱和密码表单，已通过点击操作关闭 Cookie 提示。
- 测试窗口关闭后，CDP 检查确认无痕 context 为 0、Deezer 标签页为 0。
- Spotify 公开入口返回 HTTP 302；回调和三个 scopes 已检查。
- 独立复核：严DE（Claude），PASS，agent `bc249d8f-4dd8-4367-89a7-869642af0e68`；工作区已归档。
- 未提供真实账号凭据，因此真实账号登录及提取 ARL 的完整链路尚未人工验收；身份校验、提取、绑定、trusted 同步和清理均通过模拟 CDP/API 测试。
