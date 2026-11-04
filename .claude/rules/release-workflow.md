# EngineX OS 发版规则（Release Checklist）

任何人或 AI agent 发布 EngineX OS（包括新版本，以及同一版本的修复重发）都按这份清单做，不跳步。`<VERSION>` 指 `5.0.4.x` 这样的版本号，`<OLD>` 指上一个版本号。

## 0. 先确定发哪种

- **同一版本修复重发**（维护者默认做法：版本号固定，例如一直是 5.0.4.1）：不改版本号，跳过第 1 步，第 5 步用 `--clobber` 覆盖同一个 Release，再把 tag 移到新提交。
- **新版本**：只有维护者明确要求才升版本号。按全部步骤做，新建 `v<VERSION>` tag 与 Release。

## 1. 必改文件（只在新版本时改）

| 文件 | 要改的地方 |
| --- | --- |
| `rootfs-overlay/etc/sc5000/version.txt` | 整个文件内容就是 `<VERSION>` |
| `README.md`、`README.zh-CN.md`、`README.zh-TW.md` | 第 3 行 hero 图的 `alt="EngineX OS <VERSION>"`；简介里的 `**EngineX OS <VERSION>**`；性能对比表表头里的 `` `EngineX OS <VERSION>` ``；「如何安装与升级」里全部 8 个机型的文件名和下载链接（三份 README 各 8 条，共 24 条） |
| `docs/assets/hero-banner*.svg` | 三张 banner 现在不写版本号。大版本换视觉或 banner 里出现版本号时才改，三语一起改 |
| `tools/az01-packer/README.md` | 示例命令里的版本号（可选，保持示例与现行版本一致） |

下载链接格式（文件名带版本号，所以每次升版本都要改）：

```
https://github.com/Cd1s/EngineX-OS/releases/latest/download/<PREFIX>-<VERSION>-EngineX-OS-<Model>-Update.img
```

改完检查没有漏掉的旧版本号（测试夹具里的版本号不用改）：

```sh
git grep -n "<OLD>" -- README.md README.zh-CN.md README.zh-TW.md rootfs-overlay docs/assets
```

`tests/`、`test_az01_packer.py` 里的版本号是测试数据，不要改。镜像里的 `/etc/sc5000/version.txt` 由 `build-release.sh` 的 `--set-version` 写入，第 1 步改的仓库文件用于实机热部署和保持一致。

## 2. 测试（只在编译机上跑，本机不编译）

```sh
gofmt -l . && go vet ./... && go test -race ./...
bash tests/test_rootfs_overlay.sh                 # 必须用 bash
python3 -m unittest discover -s tools/az01-packer
python3 -m unittest discover -s tools/stems
# C 单测：编译命令写在各文件头部
#   tools/ui-mod/test_dsp_smooth.c（另跑 ASan/UBSan 与 TSan 各一次）
#   tools/ui-mod/test_stems_feature.c（armhf + qemu-arm）
#   tools/edisksd-hook/test_edisksd_hook.c（--mounts 只在 unshare -m 里跑）
```

涉及 Factory Reset、`az01-update --erase-data`、空白 `/data` 首次启动的改动，只在编译机的 `unshare -m` + `qemu-arm` 沙箱里用解包出的 rootfs 测，**绝不在实机触发**。

## 3. 提交与构建

1. 改动先提交并推送到 `origin/main`，构建必须对应一个已推送的提交。
2. 用干净目录构建（`remote-build` 同步不会删除远端已删除的文件，旧目录会带上残留）：

```sh
remote-build --name sc5000-release-<短说明> --clean -- \
  'sh tools/az01-packer/build-release.sh <VERSION> <官方固件目录> none <fsck.hfsplus> <新输出目录>'
```

第 3 个参数在公开发布时**必须是 `none`**：公开镜像不能内置任何人的 SSH 公钥（#46）。只有维护者自用的私有镜像才传公钥文件，且私有镜像不能上传到 Release。

输出目录必须不存在、且不在任何 git 工作区里。产出 8 个 `*-Update.img` 和 `SHA256SUMS.txt`。

## 4. 校验镜像

- 三个镜像组各抽一个，`python3 tools/az01-packer/az01_packer.py unpack <img> --output <新目录>` 要输出 `"verified": true`。
- 在解包出的 `rootfs.ext2` 里用 `debugfs -R "cat <路径>"` 核对：`/etc/sc5000/version.txt` 是 `<VERSION>`，本次修改的文件确实在镜像里。
- 同一组的机型 sha256 应相同：

| 镜像组 | 机型（前缀，板号） |
| --- | --- |
| SC5000 | `SC5000` JP07、`SC5000M` JP08、`PRIMEGO` JP11 |
| SC6000 | `SC6000` JP13、`SC6000M` JP14 |
| PRIME 4 | `PRIME4` JC11、`PRIME2` JC16、`MIXSTREAMPRO` NH08 |

## 5. Tag 与 GitHub Release

在编译机上直接上传（那边 `gh` 已登录，不用把 1.5 GB 拉回本机）：

```sh
# 新版本
gh release create v<VERSION> --repo Cd1s/EngineX-OS --title "EngineX OS <VERSION>" \
  --notes-file notes.md --target <提交> *.img SHA256SUMS.txt
# 同一版本修复重发
gh release upload v<VERSION> --repo Cd1s/EngineX-OS *.img SHA256SUMS.txt --clobber
```

- Release Notes 必须有 8 个机型的 SHA-256 对照表（机型、板号、文件名、SHA-256），每次重发都要换成新值。
- tag 指向记录这次发布的提交：`git tag -f v<VERSION> <提交> && git push -f origin v<VERSION>`（同一版本重发时移动 tag）。
- 新版本要设为 latest，README 的 `releases/latest/download/...` 链接才会指向它。

## 6. 发布后核对

```sh
gh release view v<VERSION> --repo Cd1s/EngineX-OS --json assets -q '.assets[] | "\(.digest) \(.name)"'
```

- 每个资产的 digest 与 `SHA256SUMS.txt`、Release Notes 表格一致。
- 新版本时，抽查 README 里的下载链接：`curl -sIL <链接> | grep -i "^HTTP"` 最终是 200。
- 在 `docs/ISSUE-FIXES-*.md` 记录：日期、范围、原因、验证、构建提交、三组 sha256、回滚点（上一次构建的提交）、实机影响。

## 7. 隐私与版权

- 提交、Release Notes、日志里不出现私钥、Token、Deezer ARL、密码、维护者的私有域名和内网主机名。私有域名清单不写进仓库，发版前向维护者确认并 `git grep` 为 0 命中。
- 不提交官方固件、解包的 rootfs、原厂二进制（`.gitignore` 已拦截 `*.img`、`*.ext2`、`*.payload` 等）。

## 8. 实机（可选）

- 实机只做热部署和只读核对；部署前先备份被替换的文件。
- 测碟机端在线更新时，要把实机两个 `version.txt`（`/data/sc5000-custom/version.txt` 和 `/etc/sc5000/version.txt`）临时改成比 `<VERSION>` 旧的版本才会出现更新提示；`/etc` 那份用 bind mount 临时文件，重启即恢复。
