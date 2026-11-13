# Package ext4: tests and oracles (#255 work package C)

All commands run on an approved ARM build host (Oracle-Batam-arm or
Oracle-Bogota-arm) as root, inside a private mount namespace:

```sh
. /etc/profile.d/buildtools.sh
unshare -m sh -c 'mount --make-rprivate / && mount -t tmpfs tmpfs /data &&
  EXT4_KERNEL_MOUNT=1 \
  EXT4_STOCK_ROOTFS=/root/build/sc5000-lab/sclive4-rea61-followup/rootfs \
  go test -race -count=1 ./internal/installer/ext4/...'
go test -run XXX -fuzz FuzzHostileImage -fuzztime 120s ./internal/installer/ext4/
```

| Test | Oracle | What it proves |
| --- | --- | --- |
| `ext4_test.go` | native reader/checker | create, inject, verify, repeated injection frees what it replaces, capacity preflight leaves the image unchanged, symlink and `..` paths refused, deep (fragmented) extent trees, original backup never written, refused backups leave no output, dirty journal refused, encrypted directories never modified |
| `TestOracleFreshImages` | host `e2fsck -fn`, `dumpe2fs`, `debugfs dump -p` | 64 MiB (AZ04 `data`), odd 300 MiB, 2.25 GiB and 4.5 GiB fresh images are clean with exactly the planned feature set; injected files and modes read back through e2fsprogs |
| `TestOracleBackupProfiles` | host `mke2fs`/`debugfs`/`e2fsck -fn` | five backup profiles (stock 1.47 with orphan_file + metadata_csum_seed + resize_inode, legacy uninit_bg, 32-bit metadata_csum, 1 KiB blocks without flex_bg, no checksums), each with `/enginex` and with a real legacy `/sc5000-custom`; an htree directory that must be modified, xattr, hard link, symlink and user files survive; the backup's hash never changes; a second injection over the result is clean |
| `TestOracleStockDeckTools` | the deck's own armhf `mkfs.ext4`/`fsck.ext4` 1.47.0 run natively | a `/data` made by the deck's `az0x-data-mkfs` command is injectable; the deck's boot-time `fsck.ext4 -fn` and `-fy` exit 0 on fresh and injected images |
| `TestKernel*` | Linux kernel loop mount | the kernel reads every injected file and link; over a kernel-written backup with fscrypt v2 `downloads/` and `stems/`, indexed directories, xattrs and hard links, injection keeps every encrypted inode byte-identical, the files still decrypt with the original key, and the deck can keep writing afterwards |
| `review_test.go` | bounded-time guards, image hashes | hostile block maps (fan-out triple-indirect blocks, blocks past i_size, huge directory or journal sizes) are refused in bounded time and memory; shared mapping blocks stop the checker early; the capacity preflight replays Apply's order so ErrNoSpace leaves the image unchanged; parent/child path conflicts and large files without large_file are refused before any write; i_blocks stays right when a directory's tree shrinks |
| `review3_test.go` | reviewer reproductions, image hashes, e2fsck when installed | a directory whose growth needs a deeper extent tree is charged exactly (refused with 2 free blocks, applied with 3); a block-mapped directory that would have to grow is refused before any write while its slack is still used; a damaged tree shared by many inodes is walked once; a corrupt journal is refused before an output copy exists |
| `FuzzHostileImage` | none (must not panic or hang) | damaged metadata in Probe, Check, Verify and Apply; damaged inodes get a valid checksum so block-mapped (indirect) paths are reached |

Activation sandbox: `internal/ext4inject` is a test-only CLI with the
`mkfs.ext4` and `host-install-userdata.py` command shapes. Run the unmodified
`tools/az01-packer/test_sclive4_bootstrap.sh` from a scratch worktree whose
`host-install-userdata.py` is replaced by a shim that calls
`ext4inject inject` and then requires `e2fsck -fn` exit 0, with a
`mkfs.ext4` shim that calls `ext4inject mkfs` first on `PATH`. Run it
for SC LIVE 4, `AZ04=1` and `JC11S_ROOTFS=<unpacked official PRIME4PLUS
rootfs>`. The scratch worktree is never committed.

Not covered without hardware: flashing an image produced here to a real
deck, its first boot and activation, and the deck's real eMMC/partition
geometry.
