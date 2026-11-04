#!/usr/bin/env python3
"""Offline AZ01 wire-v1 packer. No mounts, device writes, flashing or MCU updates.

Only the inspected unsigned AZ01 layout is writable. FIT, unknown versions,
unknown records, trailing data and signed layouts fail closed.
"""
import argparse
import base64
import hashlib
import json
import lzma
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import shutil
import stat
import struct
import subprocess
import tempfile


class SafetyError(ValueError):
    pass


def require(ok, message):
    if not ok:
        raise SafetyError(message)


def safe_path(path):
    require(isinstance(path, str) and path.startswith('/') and path != '/',
            'expected an absolute non-root path')
    require(not any(c in path for c in '\\"\n\r\x00') and
            all(ord(c) >= 32 for c in path), 'unsafe path characters')
    require(all(p not in ('', '.', '..') for p in path.split('/')[1:]),
            'non-canonical path')
    return path


def protected(path):
    p = path.lower()
    return (p == '/boot' or p.startswith('/boot/') or
            p == '/usr/engine/firmware' or p.startswith('/usr/engine/firmware/') or
            any(re.fullmatch(r'(?:u-?boot.*|spl(?:[._-].*)?|env(?:[._-].*)?|.*\.env|'
                             r'idbloader(?:[._-].*)?|mlo|trust\.(?:img|bin))', part)
                for part in p.split('/')))


def writable(path):
    safe_path(path)
    require(not protected(path), 'protected bootloader/MCU path: ' + path)


class Reader:
    def __init__(self, data, pos=0):
        self.data, self.pos = data, pos

    def take(self, n):
        require(0 <= n <= len(self.data) - self.pos, 'truncated container')
        value = self.data[self.pos:self.pos+n]
        self.pos += n
        return value

    def u32(self):
        return struct.unpack('<I', self.take(4))[0]

    def string(self):
        n = self.u32()
        require(n <= 4096, 'oversized string')
        value = self.take(n)
        require(self.take(1) == b'\0', 'missing string terminator')
        require(not any(self.take((-(n+1)) % 4)), 'nonzero string padding')
        try:
            return value.decode('ascii')
        except UnicodeDecodeError as exc:
            raise SafetyError('non-ASCII container string') from exc


# Device-tree compatible strings of the 5.0.4 AZ01 wire-v1 images and the products
# they name (#32). A container keeps its own list: a package is only ever built
# from the official image of the same model group, never retargeted.
MODELS = {
    'inmusic,jp07': ('JP07', 'SC5000'),
    'inmusic,jp08': ('JP08', 'SC5000M'),
    'inmusic,jp11': ('JP11', 'PRIME GO'),
    'inmusic,jp13': ('JP13', 'SC6000'),
    'inmusic,jp14': ('JP14', 'SC6000M'),
    'inmusic,jc11': ('JC11', 'PRIME 4'),
    'inmusic,jc16': ('JC16', 'PRIME 2'),
    'inmusic,nh08': ('NH08', 'MIXSTREAM PRO'),
}
# Official 5.0.4 images come in three AZ01 groups, byte-identical within a group
# and all carrying the same Engine build (af073865...): SC5000/SC5000M/PRIME GO,
# SC6000/SC6000M, PRIME 4/PRIME 2/MIXSTREAM PRO. SC LIVE 2 is AZ0x (signed).


def models_of(compatible):
    return [dict(code=MODELS[c][0], name=MODELS[c][1]) for c in compatible if c in MODELS]


class Image:
    EOF = b'EOF\0' + struct.pack('<IQ', 16, 0)

    def __init__(self, data):
        self.data = data
        r = Reader(data)
        magic = r.take(4)
        # SC LIVE 2 ships AZ0x, a signed container (key record "denon-1"). A
        # modified image would fail the deck's signature check (#32).
        require(magic != b'AZ0x', 'AZ0x (SC LIVE 2) is a signed container; this tool cannot sign a modified image')
        require(magic == b'AZ01', 'not AZ01 (FIT/4.x is read-only research only)')
        self.version, self.table = r.u32(), r.u32()
        require(self.version == 1, 'unsupported AZ01 wire version; refuse signed/unknown layout')
        self.build = r.string()
        count = r.u32()
        require(1 <= count <= 32, 'invalid compatibility count')
        self.compatible = [r.string() for _ in range(count)]
        count = r.u32()
        require(1 <= count <= 32, 'invalid USB device count')
        self.usb_devices = [r.u32() for _ in range(count)]
        self.description = r.string()
        require(r.pos == self.table, 'unrecognized header extensions')
        self.models = models_of(self.compatible)
        require(self.models, 'no supported model in compatible list: ' + ', '.join(self.compatible))
        self.parts = []
        for i, name in enumerate(('splash', 'recoverysplash', 'rootfs')):
            entry = r.pos
            require(r.take(8) == (b'PARTP\0\0\0' if i == 0 else
                                    b'PARTX\0\0\0' if i == 1 else b'PARTL\0\0\0'),
                    'unexpected partition record/order (bootloader partitions forbidden)')
            size = struct.unpack('<Q', r.take(8))[0]
            actual_name = r.string()
            require(actual_name == name, 'unexpected/protected partition: ' + actual_name)
            comp = r.string()
            require(comp == ('xz' if name == 'rootfs' else 'none'),
                    'unsupported partition compression')
            require(r.u32() == 1 and r.string() == 'sha1' and r.u32() == 20,
                    'unsupported hash/signature layout')
            digest_at = r.pos
            digest = r.take(20)
            offset = r.pos
            payload = r.take(size)
            require(hashlib.sha1(payload).digest() == digest, 'SHA-1 mismatch: ' + name)
            self.parts.append(dict(name=name, compression=comp, entry=entry,
                                   offset=offset, size=size, digest_at=digest_at,
                                   sha1=digest.hex()))
        self.end = r.pos
        # SC6000 and PRIME 4 images pad the last record with zeros to 8 bytes
        # before EOF; SC5000 images happen to end aligned (#32).
        pad = (-r.pos) % 8
        if pad and data[r.pos:r.pos+pad] == bytes(pad) and data[r.pos+pad:r.pos+pad+4] == b'EOF\0':
            r.take(pad)
        self.aligned = r.pos % 8 == 0
        require(r.take(16) == self.EOF and r.pos == len(data),
                'unexpected EOF/trailer/signature')

    def payload(self, name='rootfs'):
        p = next(p for p in self.parts if p['name'] == name)
        return self.data[p['offset']:p['offset']+p['size']]

    def info(self):
        return dict(magic='AZ01', wire_version=self.version, build=self.build,
                    compatible=self.compatible, models=self.models, usb_devices=self.usb_devices,
                    description=self.description,
                    table_offset=self.table, file_size=len(self.data),
                    sha256=hashlib.sha256(self.data).hexdigest(),
                    partitions=self.parts, eof_offset=self.end, verified=True)

    def replace_rootfs(self, payload):
        p = self.parts[-1]
        result = bytearray(self.data[:p['offset']])
        struct.pack_into('<Q', result, p['entry']+8, len(payload))
        result[p['digest_at']:p['digest_at']+20] = hashlib.sha1(payload).digest()
        result.extend(payload)
        if self.aligned:
            result.extend(bytes((-len(result)) % 8))
        result.extend(self.EOF)
        Image(bytes(result))
        return bytes(result)


MAX_ROOTFS = 2 * 1024**3


def compress_rootfs(data, original_payload):
    require(original_payload[:6] == b'\xfd7zXZ\0' and len(original_payload) >= 12,
            'original rootfs is not XZ')
    check = original_payload[7] & 15
    require(check in (lzma.CHECK_CRC32, lzma.CHECK_CRC64, lzma.CHECK_SHA256) and
            lzma.is_check_supported(check), 'unsupported original XZ integrity check')
    # The inspected 5.0.4 stream uses LZMA2 with an 8 MiB dictionary (preset 6)
    # and CRC32, unlike 4.3.4's CRC64. Preserve the actual integrity check.
    return lzma.compress(data, format=lzma.FORMAT_XZ, check=check, preset=6)


def inflate(payload, output):
    """Bounded single XZ stream; no concatenated streams or decompression bombs."""
    dec = lzma.LZMADecompressor(format=lzma.FORMAT_XZ, memlimit=256*1024**2)
    size = 0
    for start in range(0, len(payload), 1024**2):
        chunk = payload[start:start+1024**2]
        input_end = start + len(chunk)
        while True:
            block = dec.decompress(chunk, max_length=1024**2)
            size += len(block)
            require(size <= MAX_ROOTFS, 'rootfs exceeds 2 GiB safety limit')
            output.write(block)
            if dec.eof:
                require(not dec.unused_data and input_end == len(payload),
                        'trailing/concatenated XZ streams')
                return
            if dec.needs_input:
                break
            chunk = b''
    raise SafetyError('incomplete XZ stream')


class ExtFS:
    """Offline e2fsprogs access. Never mount or follow guest symlinks."""
    def __init__(self, path):
        self.path = Path(path).resolve()
        require(not any(c in str(self.path) for c in '\\"\n\r') and
                all(ord(c) >= 32 for c in str(self.path)), 'unsafe host image path')
        require(self.path.is_file(), 'rootfs must be a regular image file')
        require(2048 <= self.path.stat().st_size <= MAX_ROOTFS,
                'rootfs image outside 2 KiB..2 GiB safety bounds')
        with self.path.open('rb') as f:
            f.seek(1080)
            require(f.read(2) == b'\x53\xef', 'rootfs is not ext2/3/4')
        require(shutil.which('debugfs'), 'debugfs is required (e2fsprogs)')

    def cmd(self, command, write=False):
        argv = ['debugfs'] + (['-w'] if write else []) + ['-R', command, str(self.path)]
        p = subprocess.run(argv, capture_output=True, text=True)
        # debugfs often returns 0 even when its command failed.
        errors = '\n'.join(p.stderr.splitlines()[1:]).strip()
        require(p.returncode == 0 and not errors,
                f'debugfs command failed: {errors or p.stdout[-1000:]}')
        return p.stdout

    def geometry(self):
        # Firmware boot arguments and partition capacity belong to the stock
        # image. File-level MCU equality alone must not authorize resize/UUID or
        # feature changes to an externally edited filesystem.
        with self.path.open('rb') as f:
            f.seek(1024)
            sb = f.read(1024)
        fields = {
            'inode_count': (0, 4), 'block_count_lo': (4, 4),
            'first_data_block': (20, 4), 'block_and_cluster_sizes': (24, 8),
            'group_geometry': (32, 12), 'revision': (76, 4),
            'first_inode_and_inode_size': (84, 6),
            'features': (92, 12), 'uuid': (104, 16),
            'journal_identity': (208, 24), 'descriptor_size': (254, 2),
            'block_count_hi': (336, 4), 'flex_group_and_checksum_type': (372, 2),
            'checksum_seed': (624, 4),
        }
        return dict(image_size=self.path.stat().st_size,
                    **{name: sb[offset:offset+size].hex()
                       for name, (offset, size) in fields.items()})

    def entries(self, path):
        if path != '/':
            safe_path(path)
        result = {}
        for line in self.cmd(f'ls -p "{path}"').splitlines():
            if not line.strip():
                continue
            fields = line.split('/')
            require(len(fields) == 8 and fields[0] == '', 'unrecognized directory listing')
            ino, mode, uid, gid, name, size = fields[1:7]
            if name in ('.', '..') or ino == '0':
                continue
            full = path.rstrip('/') + '/' + name
            # systemd ships literal backslashes in names such as
            # secure\\x2dmedia.mount. Listing those is harmless; writes still use
            # safe_path and refuse such names, as do protected reads.
            require(all(ord(c) >= 32 for c in name) and '"' not in name,
                    'unsupported directory entry characters')
            require(name not in result, 'duplicate directory entry')
            result[name] = (int(ino), int(mode, 8), int(uid), int(gid), size)
        return result

    def walk(self, path='/'):
        for name, meta in self.entries(path).items():
            full = path.rstrip('/') + '/' + name
            yield full, meta
            if stat.S_ISDIR(meta[1]):
                yield from self.walk(full)

    def read(self, path):
        safe_path(path)
        with tempfile.TemporaryDirectory(prefix='az01-read-', dir=self.path.parent) as tmp:
            dest = Path(tmp)/'data'
            self.cmd(f'dump "{path}" "{dest}"')
            require(dest.is_file(), 'could not read ' + path)
            return dest.read_bytes()

    def attributes(self, path):
        # ea_list omits values of long attributes. Read every complete value;
        # comparing names/sizes alone would miss same-length ACL/xattr changes.
        result = {}
        for line in self.cmd(f'ea_list "{path}"').splitlines():
            if not line.strip() or line == 'Extended attributes:':
                continue
            match = re.fullmatch(r'\s+(.+?) \((\d+)\)(?: =.*)?', line)
            require(match is not None, 'unrecognized extended attribute listing')
            name, size = match[1], int(match[2])
            require(size <= 65536 and not any(c in name for c in '\\"\n\r') and
                    all(ord(c) >= 32 for c in name), 'unsupported extended attribute')
            with tempfile.TemporaryDirectory(prefix='az01-ea-', dir=self.path.parent) as tmp:
                dest = Path(tmp)/'value'
                self.cmd(f'ea_get -f "{dest}" "{path}" "{name}"')
                data = dest.read_bytes()
                require(len(data) == size, 'extended attribute size mismatch')
                result[name] = hashlib.sha256(data).hexdigest()
        return result

    def snapshot(self):
        """Content plus identity/metadata for boot, MCU, env and their ancestors."""
        entries = dict(self.walk())
        selected = {p for p in entries if protected(p)}
        for p in list(selected):
            selected.update(str(a) for a in PurePosixPath(p).parents if str(a) != '/')
        result = {}
        for p in sorted(selected):
            meta = entries[p]
            # Directory size/link count may grow when adding an unrelated child.
            identity = meta[:4] if stat.S_ISDIR(meta[1]) else meta
            content = None
            inode_stat = self.cmd(f'stat "{p}"')
            if stat.S_ISREG(meta[1]):
                content = hashlib.sha256(self.read(p)).hexdigest()
            elif stat.S_ISLNK(meta[1]):
                content = inode_stat
            # Protected inodes must also retain flags, link count, times, ACLs
            # and xattrs. Ancestor directory sizes/times/extents may legitimately
            # change when an unrelated child is added; retain their flags/ACLs.
            if protected(p):
                metadata = inode_stat
            else:
                metadata = '\n'.join(re.findall(r'Flags: [^\n]+|File ACL: [^\n]+', inode_stat))
            attrs = self.attributes(p)
            result[p] = (identity, content, metadata, attrs)
        return result

    def ensure_parents(self, path):
        current = '/'
        for name in PurePosixPath(path).parts[1:-1]:
            entries = self.entries(current)
            current = current.rstrip('/') + '/' + name
            writable(current)
            if name not in entries:
                self.cmd(f'mkdir "{current}"', True)
            else:
                require(stat.S_ISDIR(entries[name][1]), 'symlink/non-directory parent: '+current)

    def write(self, path, data, mode=0o644):
        writable(path)
        self.ensure_parents(path)
        parent, name = str(PurePosixPath(path).parent), PurePosixPath(path).name
        existing = self.entries(parent).get(name)
        if existing:
            require(stat.S_ISREG(existing[1]), 'refuse replacing symlink/non-file: '+path)
            s = self.cmd(f'stat "{path}"')
            require(re.search(r'Links:\s+1\b', s), 'refuse modifying hardlinked file: '+path)
            self.cmd(f'rm "{path}"', True)
        with tempfile.TemporaryDirectory(prefix='az01-write-', dir=self.path.parent) as tmp:
            src = Path(tmp)/'data'
            src.write_bytes(data)
            self.cmd(f'write "{src}" "{path}"', True)
        self.cmd(f'set_inode_field "{path}" mode 0{stat.S_IFREG | mode:o}', True)
        self.cmd(f'set_inode_field "{path}" uid 0', True)
        self.cmd(f'set_inode_field "{path}" gid 0', True)
        require(self.read(path) == data, 'write verification failed: '+path)

    def validate_link(self, path, target, overlay=None):
        """Resolve guest links component by component, without following host links."""
        writable(path)
        overlay = dict(overlay or {})
        overlay[path] = target
        parents = {'/': None}
        resolved = []
        pending = target.split('/') if isinstance(target, str) else []
        if isinstance(target, str) and not target.startswith('/'):
            pending = list(PurePosixPath(path).parts[1:-1]) + pending
        hops = 0
        while True:
            require(isinstance(target, str) and target and
                    not any(c in target for c in '\\\"\n\r\x00') and
                    all(ord(c) >= 32 for c in target), 'unsafe symlink target')
            if target.startswith('/'):
                resolved = []
            while pending:
                name = pending.pop(0)
                if name in ('', '.'):
                    continue
                if name == '..':
                    require(resolved, 'symlink escapes guest root: '+path)
                    resolved.pop()
                    continue
                current = '/'+'/'.join([*resolved, name])
                writable(current)
                parent = '/'+'/'.join(resolved)
                if parents[parent] is None:
                    parents[parent] = self.entries(parent)
                meta = parents[parent].get(name)
                if current in overlay:
                    link = overlay[current]
                else:
                    link = None
                    if meta and stat.S_ISLNK(meta[1]):
                        info = self.cmd(f'stat "{current}"')
                        fast = re.search(r'^Fast link dest: "(.*)"$', info, re.M)
                        if fast:
                            link = fast[1]
                        else:
                            try:
                                link = self.read(current).decode('utf-8')
                            except UnicodeDecodeError as exc:
                                raise SafetyError('invalid guest symlink encoding') from exc
                if link is not None:
                    hops += 1
                    require(hops <= 40, 'symlink loop or excessive chain: '+path)
                    target = link
                    pending = link.split('/') + pending
                    break
                parents[current] = None if meta and stat.S_ISDIR(meta[1]) else {}
                resolved.append(name)
            else:
                return '/'+'/'.join(resolved)

    def link(self, path, target, overlay=None):
        self.validate_link(path, target, overlay)
        self.ensure_parents(path)
        parent, name = str(PurePosixPath(path).parent), PurePosixPath(path).name
        existing = self.entries(parent).get(name)
        if existing:
            require(not stat.S_ISDIR(existing[1]), 'refuse replacing directory: '+path)
            self.cmd(f'rm "{path}"', True)
        self.cmd(f'symlink "{path}" "{target}"', True)


PREPARE = b'''#!/bin/sh
set -eu
umask 077
for path in /data/sc5000-custom /data/sc5000-custom/authorized_keys /data/sc5000-custom/ssh_host_ed25519_key /data/sc5000-custom/ssh_host_ed25519_key.pub; do
    [ ! -L "$path" ] || { echo "Refusing persistent SSH symlink: $path" >&2; exit 1; }
done
mkdir -p /data/sc5000-custom
chmod 700 /data/sc5000-custom
work=$(mktemp -d /data/sc5000-custom/.prepare.XXXXXX)
trap 'rm -rf "$work"' 0
if [ ! -e /data/sc5000-custom/authorized_keys ]; then
    cp /usr/share/sc5000-custom/authorized_keys "$work/authorized_keys"
    chmod 600 "$work/authorized_keys"
    mv "$work/authorized_keys" /data/sc5000-custom/authorized_keys
fi
[ -f /data/sc5000-custom/authorized_keys ]
chmod 600 /data/sc5000-custom/authorized_keys
if [ ! -e /data/sc5000-custom/ssh_host_ed25519_key ]; then
    /usr/bin/ssh-keygen -q -t ed25519 -N '' -f "$work/ssh_host_ed25519_key"
    mv "$work/ssh_host_ed25519_key" /data/sc5000-custom/ssh_host_ed25519_key
    mv "$work/ssh_host_ed25519_key.pub" /data/sc5000-custom/ssh_host_ed25519_key.pub
fi
[ -f /data/sc5000-custom/ssh_host_ed25519_key ]
/usr/bin/ssh-keygen -y -P '' -f /data/sc5000-custom/ssh_host_ed25519_key >/dev/null

# A firmware update rewrites / but never /data: install the image's UI packs
# once per release, before Engine starts (engine.service orders after this unit).
install_ui() {
    archive=${SC5000_UI_ARCHIVE:-/usr/share/sc5000-custom/ui-packs.tar.xz}
    ui=${SC5000_UI_DIR:-/data/sc5000-custom/ui}
    [ -f "$archive" ] || return 0
    version=$(cat /etc/sc5000/version.txt 2>/dev/null || echo unknown)
    # Releases keep their version (5.0.4.1) across rebuilds: also key on the archive.
    id=$(sha256sum < "$archive" | cut -d' ' -f1)
    if [ -d "$ui/packs" ] && [ "$(cat "$ui/.pack-version" 2>/dev/null)" = "$version" ] &&
        [ "$(cat "$ui/.pack-id" 2>/dev/null)" = "$id" ]; then
        return 0
    fi
    for path in "$ui" "$ui/packs" "$ui/icons"; do
        [ ! -L "$path" ] || { echo "Refusing UI symlink: $path" >&2; return 1; }
    done
    umask 022
    mkdir -p "$ui"
    stage=$(mktemp -d "$ui/.install.XXXXXX")
    if ! xzcat "$archive" | tar -C "$stage" -xf - || [ ! -d "$stage/packs" ]; then
        rm -rf "$stage"
        return 1
    fi
    for name in packs icons; do
        [ -d "$stage/$name" ] || continue
        rm -rf "$ui/$name.replaced"
        [ ! -e "$ui/$name" ] || mv "$ui/$name" "$ui/$name.replaced"
        mv "$stage/$name" "$ui/$name"
        rm -rf "$ui/$name.replaced"
    done
    rm -rf "$stage"
    # New packs deserve a fresh try, even after the old ones hit safe mode.
    rm -f "$ui/safe-mode" "$ui/boot-count" "$ui/boot-token"
    printf '%s\n' "$version" > "$ui/.pack-version"
    printf '%s\n' "$id" > "$ui/.pack-id"
    printf '%s\n' "$version" > "${SC5000_VERSION_FILE:-/data/sc5000-custom/version.txt}"
}
install_ui || echo "sc5000-custom-prepare: UI packs not installed" >&2
'''

# Engine 4.3.4 left copies of these in the /etc overlay upper layer; they
# shadow the 5.0.4 lower /etc (telemetry hosts blocking connman's online check,
# stale connman/edisksd settings). Removing upper entries while /etc is mounted
# is not seen until reboot, so this runs once per epoch before etc.mount.
ETC_RESET_TEMPLATE = '''#!/bin/sh
set -eu
epoch=@EPOCH@
upper=/data/system/etc/overlay
marker=/data/sc5000-custom/etc-overlay-epoch
for path in /data/system /data/system/etc "$upper" /data/sc5000-custom "$marker"; do
    [ ! -L "$path" ] || { echo "Refusing /etc overlay symlink: $path" >&2; exit 1; }
done
if [ -f "$marker" ] && [ "$(cat "$marker")" = "$epoch" ]; then
    exit 0
fi
if [ -d "$upper" ]; then
    for name in hosts resolv.conf connman dnsmasq.d edisksd@MANAGED@; do
        rm -rf -- "$upper/$name"
    done
fi
mkdir -p /data/sc5000-custom
chmod 700 /data/sc5000-custom
printf '%s\n' "$epoch" > "$marker.tmp"
mv "$marker.tmp" "$marker"
'''


def etc_reset(epoch='engine-5.0.4', managed=()):
    """The reset script for one build. A new epoch (version + /etc content of the
    overlay) also drops upper-layer copies of every /etc file the image ships, so
    a live-deployed guard, hook or drop-in cannot shadow the new image (#33)."""
    require(re.fullmatch(r'[A-Za-z0-9._-]{1,64}', epoch), 'invalid epoch')
    for name in managed:
        require(re.fullmatch(r'[A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*', name) and '..' not in name.split('/'),
                'unsafe /etc path for reset: ' + name)
    return (ETC_RESET_TEMPLATE.replace('@EPOCH@', epoch)
            .replace('@MANAGED@', ''.join(' ' + n for n in sorted(managed))).encode())


ETC_RESET = etc_reset()

SSHD_CONFIG = b'''Port 22
HostKey /data/sc5000-custom/ssh_host_ed25519_key
AuthorizedKeysFile /data/sc5000-custom/authorized_keys
PermitRootLogin prohibit-password
PasswordAuthentication no
PermitEmptyPasswords no
KbdInteractiveAuthentication no
AuthenticationMethods publickey
PubkeyAuthentication yes
UsePAM no
AllowUsers root
PidFile /run/sc5000-custom-sshd.pid
Subsystem sftp /usr/libexec/sftp-server
'''

UNITS = {
    'sc5000-custom.target': '''[Unit]
Description=SC5000 custom services
Requires=sc5000-custom-prepare.service
Wants=sc5000-custom-sshd.service
After=etc.mount var.mount data.mount sc5000-custom-prepare.service
''',
    'sc5000-custom-prepare.service': '''[Unit]
Description=Prepare persistent SC5000 access
RequiresMountsFor=/data
Requires=etc.mount var.mount
After=etc.mount var.mount
Before=sc5000-custom-sshd.service engine.service stream-sync.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/libexec/sc5000-custom-prepare
''',
    'sc5000-custom-etc-reset.service': '''[Unit]
Description=Drop stale Engine 4.3.4 /etc overlay entries
DefaultDependencies=no
RequiresMountsFor=/data
Before=etc.mount
[Service]
Type=oneshot
ExecStart=/usr/libexec/sc5000-custom-etc-reset
''',
    'sc5000-custom-sshd.service': '''[Unit]
Description=SC5000 public key SSH
Requires=sc5000-custom-prepare.service
After=sc5000-custom-prepare.service network.target
Conflicts=sshd.service sshd.socket
[Service]
Type=simple
ExecStartPre=/usr/bin/mkdir -p /run/sshd
ExecStart=/usr/sbin/sshd -D -e -f /usr/share/sc5000-custom/sshd_config
Restart=on-failure
''',
}


def public_key(data):
    require(len(data) < 16384 and '\x00' not in data, 'invalid public key size')
    lines = data.strip().splitlines()
    require(len(lines) == 1, 'provide exactly one public key (no options)')
    fields = lines[0].split()
    require(len(fields) >= 2 and fields[0] in ('ssh-ed25519', 'ssh-rsa',
            'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521'),
            'unsupported key type or authorized_keys options')
    try:
        raw = base64.b64decode(fields[1], validate=True)
        n = struct.unpack('>I', raw[:4])[0]
        require(raw[4:4+n].decode('ascii') == fields[0], 'public key algorithm mismatch')
        # Validate the complete SSH key with OpenSSH, including truncated payloads.
        p = subprocess.run(['ssh-keygen', '-l', '-f', '-'], input=lines[0]+'\n',
                           capture_output=True, text=True)
        require(p.returncode == 0, 'invalid SSH public key')
    except (ValueError, struct.error, UnicodeError) as exc:
        raise SafetyError('malformed SSH key') from exc
    return (lines[0]+'\n').encode()


# Files sc5000-debloat --root rewrites in a staged root (#33). Copied out of the
# image, patched by the overlay's own script and written back, so the offline
# image and the runtime script share one definition.
DEBLOAT_INPUTS = ('/usr/Engine/Scripts/engine', '/usr/Engine/Scripts/setup-prerequisites.sh',
                  '/usr/Engine/Scripts/setup-usb-gadget.sh', '/usr/Engine/Scripts/encrypt-fs.sh',
                  '/etc/hosts')


def offline_debloat(fs, script):
    with tempfile.TemporaryDirectory(prefix='az01-debloat-', dir=fs.path.parent) as tmp:
        root = Path(tmp)
        (root/'usr/Engine/Scripts').mkdir(parents=True)
        (root/'etc').mkdir()
        before = {}
        for path in DEBLOAT_INPUTS:
            try:
                data = fs.read(path)
            except SafetyError:
                continue
            (root/path[1:]).write_bytes(data)
            before[path] = data
        script_path = root.parent/'sc5000-debloat'
        script_path.write_bytes(script)
        p = subprocess.run(['sh', str(script_path), '--root', str(root)], capture_output=True, text=True)
        require(p.returncode == 0, 'sc5000-debloat --root failed: ' + (p.stdout + p.stderr)[-800:])
        for source in sorted(root.rglob('*')):
            dest = '/' + source.relative_to(root).as_posix()
            if source.is_symlink():
                fs.link(dest, os.readlink(source))
            elif source.is_file():
                data = source.read_bytes()
                if before.get(dest) != data:
                    mode = 0o755 if dest.startswith('/usr/Engine/Scripts/') else 0o644
                    fs.write(dest, data, mode)


VERSION_RE = re.compile(r'\d+(\.\d+){1,3}')


def inject(fs, key, services=(), overlay=None, version=None):
    additions, links, planned = {}, {}, {}
    if overlay:
        root = Path(overlay)
        require(root.is_dir() and not root.is_symlink(), 'overlay must be a real directory')
        for source in sorted(root.rglob('*')):
            s = source.lstat()
            dest = '/'+source.relative_to(root).as_posix()
            writable(dest)
            require(stat.S_ISDIR(s.st_mode) or stat.S_ISLNK(s.st_mode) or
                    (stat.S_ISREG(s.st_mode) and s.st_nlink == 1),
                    'overlay hardlinks and special files forbidden: '+dest)
            planned[dest] = None
            if stat.S_ISLNK(s.st_mode):
                links[dest] = planned[dest] = os.readlink(source)
            elif stat.S_ISREG(s.st_mode):
                additions[dest] = (source.read_bytes(), stat.S_IMODE(s.st_mode) & 0o755)
        for path, target in links.items():
            fs.validate_link(path, target, planned)
    service_data = {}
    for source in services:
        source = Path(source)
        require((re.fullmatch(r'sc5000-[a-z0-9-]+@?\.service', source.name) or
                 source.name == 'engine-guardian.service') and
                source.name not in UNITS and source.name not in service_data,
                'custom service must have a unique supported service name')
        data = source.read_bytes()
        require(b'[Service]' in data and b'ExecStart=' in data and b'\x00' not in data,
                'invalid custom service')
        service_data[source.name] = data
    if version is not None:
        require(VERSION_RE.fullmatch(version), 'invalid --set-version (e.g. 5.0.4.1)')
        # The deck's update check reads it; the release tag compares against it.
        additions['/etc/sc5000/version.txt'] = ((version + '\n').encode(), 0o644)
    etc = sorted(p for p in additions if p.startswith('/etc/'))
    digest = hashlib.sha256()
    for path in etc:
        digest.update(path.encode() + b'\0' + additions[path][0] + b'\0')
    epoch = '%s-%s' % (version or 'engine-5.0.4', digest.hexdigest()[:12]) if etc else 'engine-5.0.4'
    for path, (data, mode) in additions.items():
        fs.write(path, data, mode)
    for path, target in links.items():
        fs.link(path, target, planned)
    if '/usr/local/bin/sc5000-debloat' in additions:
        offline_debloat(fs, additions['/usr/local/bin/sc5000-debloat'][0])
    fs.write('/usr/share/sc5000-custom/authorized_keys', key, 0o600)
    fs.write('/usr/share/sc5000-custom/sshd_config', SSHD_CONFIG)
    fs.write('/usr/libexec/sc5000-custom-prepare', PREPARE, 0o755)
    # sc5000/stems-runtime-prepare: the live Stems deployment of 2026-11-03 put it
    # in the /etc upper layer; its /usr/local/bin overlay would shadow the image's
    # stems-processor wrapper.
    managed = [p[len('/etc/'):] for p in etc] + (['sc5000/stems-runtime-prepare'] if etc else [])
    fs.write('/usr/libexec/sc5000-custom-etc-reset', etc_reset(epoch, managed), 0o755)
    # A locked root account is rejected even for public-key auth with UsePAM=no.
    # A fresh random unreachable password hash unlocks the account; SSH password
    # and interactive authentication remain explicitly disabled.
    shadow = fs.read('/etc/shadow').decode().splitlines()
    require(sum(line.startswith('root:') for line in shadow) == 1, 'ambiguous root account')
    p = subprocess.run(['openssl', 'passwd', '-6', '-stdin'], input=secrets.token_hex(64),
                       capture_output=True, text=True)
    require(p.returncode == 0 and p.stdout.startswith('$6$'), 'cannot generate root password hash')
    for i, line in enumerate(shadow):
        if line.startswith('root:'):
            fields = line.split(':')
            fields[1] = p.stdout.strip()
            shadow[i] = ':'.join(fields)
    fs.write('/etc/shadow', ('\n'.join(shadow)+'\n').encode(), 0o400)
    for name, content in UNITS.items():
        fs.write('/usr/lib/systemd/system/'+name, content.encode())
    fs.link('/usr/lib/systemd/system/multi-user.target.wants/sc5000-custom.target',
            '/usr/lib/systemd/system/sc5000-custom.target')
    fs.link('/usr/lib/systemd/system/etc.mount.wants/sc5000-custom-etc-reset.service',
            '/usr/lib/systemd/system/sc5000-custom-etc-reset.service')
    for name, data in service_data.items():
        fs.write('/usr/lib/systemd/system/'+name, data)
        fs.write('/usr/lib/systemd/system/'+name+'.d/sc5000-ordering.conf',
                 b'[Unit]\nRequires=sc5000-custom-prepare.service\n'
                 b'After=sc5000-custom-prepare.service etc.mount var.mount\n')
        if not name.endswith('@.service'):
            fs.link('/usr/lib/systemd/system/sc5000-custom.target.wants/'+name,
                    '/usr/lib/systemd/system/'+name)


def new_output(path):
    p = Path(path).absolute()
    require(not p.exists() and not p.is_symlink(), 'output exists; refusing overwrite: '+str(p))
    require(p.parent.is_dir(), 'output parent does not exist')
    # Resolve parent aliases before checking worktree ancestry. A lab symlink
    # pointing into a worktree subdirectory must not bypass the binary rule.
    p = p.parent.resolve(strict=True)/p.name
    require(not p.exists() and not p.is_symlink(), 'output exists: '+str(p))
    require(not any(c in str(p) for c in '\\"\n\r') and
            all(ord(c) >= 32 for c in str(p)), 'unsafe host output path')
    # Never write firmware into this or any other git worktree.
    require(not any((a/'.git').exists() for a in [p.parent, *p.parent.parents]),
            'binary output must be outside every git worktree')
    return p


def check_fs(path):
    p = subprocess.run(['e2fsck', '-fn', str(path)], capture_output=True, text=True)
    require(p.returncode == 0, 'filesystem check failed: '+(p.stdout+p.stderr)[-1500:])


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    for name in ('info', 'unpack', 'patch-rootfs', 'repack'):
        s = sub.add_parser(name)
        s.add_argument('original', type=Path, help='pristine official AZ01 image')
        if name == 'unpack':
            s.add_argument('--output', required=True, help='new external directory')
        elif name == 'patch-rootfs':
            s.add_argument('--output', required=True, help='new external ext2 image')
            s.add_argument('--ssh-public-key', required=True, type=Path)
            s.add_argument('--overlay', type=Path)
            s.add_argument('--service', action='append', default=[], type=Path)
            s.add_argument('--set-version', help='EngineX release written to /etc/sc5000/version.txt, e.g. 5.0.4.1')
        elif name == 'repack':
            s.add_argument('--rootfs', required=True, type=Path)
            s.add_argument('--output', required=True, help='new external AZ01 image')
    args = parser.parse_args(argv)
    try:
        image = Image(args.original.read_bytes())
        if args.command == 'info':
            print(json.dumps(image.info(), indent=2))
            return 0
        output = new_output(args.output)
        if args.command == 'unpack':
            # Build privately, publish only after every check succeeds.
            with tempfile.TemporaryDirectory(prefix='az01-unpack-', dir=output.parent) as temp:
                work = Path(temp)
                for part in image.parts:
                    (work/(part['name']+'.payload')).write_bytes(image.payload(part['name']))
                with (work/'rootfs.ext2').open('wb') as f:
                    inflate(image.payload(), f)
                ExtFS(work/'rootfs.ext2')
                (work/'manifest.json').write_text(json.dumps(image.info(), indent=2)+'\n')
                os.rename(work, output)
        else:
            with tempfile.TemporaryDirectory(prefix='az01-work-', dir=output.parent) as temp:
                stock = Path(temp)/'stock.ext2'
                with stock.open('wb') as f:
                    inflate(image.payload(), f)
                original_fs = ExtFS(stock)
                before = original_fs.snapshot()
                if args.command == 'patch-rootfs':
                    key = public_key(args.ssh_public_key.read_text())
                    candidate = Path(temp)/'patched.ext2'
                    shutil.copyfile(stock, candidate)
                    inject(ExtFS(candidate), key, args.service, args.overlay, args.set_version)
                else:
                    require(args.rootfs.is_file() and not args.rootfs.is_symlink(),
                            'candidate must be a regular file')
                    candidate = Path(temp)/'candidate.ext2'
                    shutil.copyfile(args.rootfs, candidate)
                candidate_fs = ExtFS(candidate)
                require(candidate_fs.geometry() == original_fs.geometry(),
                        'rootfs size/geometry/UUID/features changed; partition resize forbidden')
                require(candidate_fs.snapshot() == before,
                        'protected bootloader/MCU content, metadata or ancestor changed')
                check_fs(candidate)
                if args.command == 'patch-rootfs':
                    os.link(candidate, output)
                else:
                    result = Path(temp)/'result.img'
                    if candidate.read_bytes() == stock.read_bytes():
                        result.write_bytes(image.data)  # Exact no-op round trip.
                    else:
                        compressed = compress_rootfs(candidate.read_bytes(), image.payload())
                        result.write_bytes(image.replace_rootfs(compressed))
                    os.link(result, output)
        print(json.dumps(dict(output=str(output), verified=True)))
        return 0
    except (SafetyError, OSError, lzma.LZMAError, RecursionError) as exc:
        parser.exit(2, 'refused: '+str(exc)+'\n')


if __name__ == '__main__':
    raise SystemExit(main())
