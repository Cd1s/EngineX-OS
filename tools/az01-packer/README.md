# AZ01 离线工具

Python 3 标准库实现容器解析；文件系统操作需要 `debugfs`、`e2fsck`（e2fsprogs），公钥检查需要 `ssh-keygen`，生成不可知随机密码哈希需要 `openssl`。不挂载文件系统、不运行固件程序、不刷机、不访问块设备。所有二进制输出必须在 git 工作树外，输出已存在时拒绝覆盖。

已实测的输入是 SC5000 5.0.4，SHA-256 为 `52e8526bbfd273e5185b5d8c2a9cbe32436c3acd1c77dcfde97e40a34c5b24d7`。实现只接受已核实的 **AZ01 wire version 1**、JP07 兼容性及 `splash → recoverysplash → rootfs → EOF` 布局；更新器内部将此类称为 v2。4.3.4 是 FIT/FDT，未知 wire version、签名格式及其他分区布局全部拒绝，不能用此工具重打包。

辅助研究入口：`inspect_fit.py <4.3.4.img> [--extract-rootfs <外部新文件>]` 仅解析、校验和提取旧 FIT；`probe_performance.py <Database2/m.db>` 用只读 SQLite URI 解析随包 Demo 的 PerformanceData，输出字段/未知尾部与 blob 哈希，不修改数据库。

```bash
set -o pipefail
python3 tools/az01-packer/az01_packer.py info \
  /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  | cut -c1-200 | head -40

python3 tools/az01-packer/az01_packer.py unpack \
  /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  --output /root/build/sc5000-lab/unpacked-5.0.4 \
  | cut -c1-200 | head -40

# 首先将官方原包存入远端备份库，并确认返回完整 backup id。
agent-backup put /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  --label sc5000-before-offline-customization | cut -c1-200 | head -40

# 使用你自己的公钥路径；不传入私钥。所有操作产生新文件。
python3 tools/az01-packer/az01_packer.py patch-rootfs \
  /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  --ssh-public-key /path/to/id_ed25519.pub \
  --output /root/build/sc5000-lab/custom-rootfs.ext2 \
  | cut -c1-200 | head -40

python3 tools/az01-packer/az01_packer.py repack \
  /root/build/sc5000-lab/fw/SC5000-5.0.4-Update.img \
  --rootfs /root/build/sc5000-lab/custom-rootfs.ext2 \
  --output /root/build/sc5000-lab/SC5000-custom-test.img \
  | cut -c1-200 | head -40
```

`unpack` 产生三个原始 `.payload`、解压后的 `rootfs.ext2` 和已校验的 `manifest.json`。`repack` 总是重新读取并验证官方原包；不信任外部 manifest。未修改 rootfs 的重打包逐字节返回官方原包；修改后保留所有非 rootfs 字节，只重算 rootfs 长度和 SHA-1，再解析验证输出。XZ 自带的校验由 liblzma 验证，限制单流、解压大小 2 GiB、字典内存 256 MiB。

重新压缩保留原 XZ integrity check（5.0.4 为 CRC32），采用 preset 6 / LZMA2 8 MiB 字典。block 数和压缩字节不要求等同官方；设备更新器接受性需要另行验证。标准库压缩是单线程，500 MiB rootfs 在此机上需要数分钟。

`patch-rootfs` 从原包提取新 rootfs，注入公钥、独立公钥 sshd 配置、准备服务与 `sc5000-custom.target`，不修改厂商 sshd 配置。根账号以新生成且不保留明文的随机 SHA-512 密码哈希解除账号锁定，SSH 的密码和交互认证均关闭。下层 `/usr/lib/systemd/system/multi-user.target.wants/` 启用自定义 target；准备服务等待 `/etc`、`/var`、`/data`，持久化目录为 `/data/sc5000-custom/`。公钥种子位于 `/usr/share/sc5000-custom/authorized_keys`，仅在持久化公钥文件不存在时复制；已有设备更换公钥需有意更新持久化文件。主机密钥在设备首次启动时生成，不打包共享主机私钥。

准备服务拒绝持久化目录/密钥中的符号链接；先在同分区私有临时目录复制公钥、生成主机密钥，再 rename 到持久化位置。生成失败不会发布半成品主机密钥；已有主机密钥会检查可读性，不自动更换设备身份。单元测试在 lab 重定位目录中实际运行此脚本，验证失败重试、身份保持和拒绝链接。

可以添加 `--overlay /path/to/tree` 注入普通文件，或者重复 `--service /path/to/sc5000-name.service` 注册服务。后者自动创建依赖准备服务的排序 drop-in 和 target wants 链接。overlay 禁止符号链接、硬链接、特殊文件及任何受保护路径；目标文件系统中的父目录也必须是真实目录，现有符号链接和多链接文件拒绝修改。自定义服务的业务行为由调用者负责审查。

安全边界：容器只能替换最后的 rootfs；`/boot/`、任何 `uboot*`/`u-boot*`、`spl`/`spl.*`、`env`/`*.env` 路径以及 `/usr/Engine/Firmware/` 都受保护。修改前后递归比较其内容、身份/权限/所有者以及父目录身份，且检查文件系统一致性。外部编辑过的 rootfs 也经过同一比较。工具不提供绕过选项。SHA-1 一致性不等于厂商真实性；官方原包的来源和 SHA-256 必须独立核对。离线重打包通过不代表实机启动或厂商更新器接受性已经验证。

保护快照还覆盖 inode flags、链接数、时间戳、ACL/扩展属性；长属性通过 `ea_get` 读取完整值计算 SHA-256，不依赖 `ea_list` 省略值后的显示。父目录允许因新增普通子项导致大小/时间变化，其身份、权限、flags、ACL仍须保持。

重包还必须保持官方 rootfs 的镜像长度、block/inode/group 几何、UUID、features、journal/descriptor/checksum身份。禁止对外部候选做扩容、换UUID或转换文件系统特性后重包；2 GiB上限只是资源限制，不是设备分区容量。官方5.0.4原始剩余空间约11.2 MiB，工具不自动扩容；较大自定义应用应另行部署到持久化数据分区，再通过服务启动。

```bash
python3 -m unittest discover -s tools/az01-packer -v 2>&1 | cut -c1-200 | head -40
```
