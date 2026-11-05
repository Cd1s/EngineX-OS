#!/usr/bin/env python3
"""Static dependency audit of an unpacked EngineX OS rootfs (release gate, step 1).

    rootfs_audit.py <rootfs tree> --overlay rootfs-overlay [--require-cmd NAME]...

Checks everything EngineX adds (the overlay and the packer's sc5000-custom-*
units/scripts) against what the vendor base really ships: hard unit
dependencies, Exec*= commands, EnvironmentFile=, script interpreters and the
absolute tool paths scripts call. 5.3.0 dropped etc.mount/var.mount; a hard
Requires= on them silently disabled prepare, the UI packs and SSH.
FAIL lines make the exit status 1; WARN lines are printed for review.
"""
import argparse
import os
import re
import sys

UNIT_DIRS = ('/etc/systemd/system', '/usr/lib/systemd/system', '/lib/systemd/system')
PATH = ('/usr/local/sbin', '/usr/local/bin', '/usr/sbin', '/usr/bin', '/sbin', '/bin')
HARD = ('Requires', 'BindsTo', 'Requisite')
SOFT = ('Wants', 'After', 'Before', 'PartOf', 'Upholds', 'Slice')
EXEC_RE = re.compile(r'Exec(Start|StartPre|StartPost|Stop|StopPost|Reload|Condition)')
UNIT_SUFFIX = ('.service', '.target', '.slice', '.socket', '.timer', '.mount', '.path')
TOOL_RE = re.compile(r'(?<![\w$.{}/-])(/(?:usr/)?(?:local/)?s?bin/[A-Za-z0-9._+-]+|/usr/libexec/[A-Za-z0-9._+-]+)')
COMMAND_V_RE = re.compile(r'command -v ([A-Za-z0-9._+-]+)')
BUILTIN_UNITS = {'-.mount', '-.slice', 'system.slice', 'init.scope'}


def resolve(root, path, depth=0):
    """Host path of `path` inside `root`, following symlinks as the deck would; None if absent."""
    parts = [p for p in path.split('/') if p]
    cur = ''
    for i, part in enumerate(parts):
        nxt = cur + '/' + part
        if os.path.islink(root + nxt):
            if depth > 40:
                return None
            target = os.readlink(root + nxt)
            base = target if target.startswith('/') else (cur or '/') + '/' + target
            rest = '/'.join(parts[i + 1:])
            return resolve(root, os.path.normpath(base + ('/' + rest if rest else '')), depth + 1)
        cur = nxt
    return root + (cur or '/') if os.path.exists(root + (cur or '/')) else None


class Audit:
    def __init__(self, root, overlay):
        self.root, self.overlay = root.rstrip('/'), overlay
        self.fails, self.warns = [], []
        self.ours = set()
        for d, _, files in os.walk(overlay):
            for f in files:
                self.ours.add('/' + os.path.relpath(os.path.join(d, f), overlay))
        self.ours.discard('/README.md')  # build-release.sh drops it
        for d in ('/usr/lib/systemd/system', '/usr/libexec'):
            host = resolve(self.root, d)
            for f in sorted(os.listdir(host)) if host else ():
                if f.startswith('sc5000-custom'):
                    self.ours.add(d + '/' + f)
        self.our_units = {os.path.basename(p) for p in self.ours if p.endswith(UNIT_SUFFIX)}
        self.fstab = set()
        fstab = resolve(self.root, '/etc/fstab')
        with open(fstab or os.devnull, errors='replace') as f:
            lines = f.read().splitlines()
        for line in lines:
            fields = line.split()
            if len(fields) > 1 and not fields[0].startswith('#') and fields[1].startswith('/'):
                self.fstab.add(mount_unit(fields[1]))

    def fail(self, msg):
        if msg not in self.fails:
            self.fails.append(msg)

    def warn(self, msg):
        if msg not in self.warns:
            self.warns.append(msg)

    def unit_exists(self, name):
        if name in BUILTIN_UNITS or name in self.fstab:
            return True
        names = [name]
        m = re.fullmatch(r'([^@]+@)[^.]+(\.\w+)', name)
        if m:
            names.append(m.group(1) + m.group(2))
        return any(resolve(self.root, d + '/' + n) for d in UNIT_DIRS for n in names)

    def command(self, cmd):
        if cmd.startswith('/'):
            return resolve(self.root, cmd)
        return next((p for p in (resolve(self.root, d + '/' + cmd) for d in PATH) if p), None)

    def unit(self, path):
        owner = os.path.basename(os.path.dirname(path))
        if owner.endswith('.d') and not self.unit_exists(owner[:-2]):
            self.warn('%s: drop-in for %s, which this base does not ship' % (path, owner[:-2]))
            return
        with open(resolve(self.root, path), errors='replace') as f:
            lines = f.read().splitlines()
        for line in lines:
            key, sep, value = line.strip().partition('=')
            if not sep or key.startswith(('#', ';', '[')):
                continue
            if key in HARD or key in SOFT:
                for dep in value.split():
                    if '%' in dep or dep.endswith('.device'):
                        continue
                    if key in HARD and dep not in self.our_units:
                        self.fail('%s: %s=%s is a vendor unit; use Wants=/After= or '
                                  'RequiresMountsFor= (5.3.0 dropped etc.mount)' % (path, key, dep))
                    elif not self.unit_exists(dep):
                        (self.fail if key in HARD else self.warn)(
                            '%s: %s=%s does not exist in this rootfs' % (path, key, dep))
            elif EXEC_RE.fullmatch(key) and value:
                cmd = value.lstrip('-@:+!|')
                if cmd and not self.command(cmd.split()[0]):
                    ignored = '-' in value[:len(value) - len(cmd)]  # systemd ignores a failing "-" command
                    (self.warn if ignored else self.fail)('%s: %s command %s not found' % (path, key, cmd.split()[0]))
            elif key == 'EnvironmentFile' and not value.startswith('-') and not resolve(self.root, value):
                self.fail('%s: EnvironmentFile=%s not found' % (path, value))

    def script(self, path):
        host = resolve(self.root, path)
        with open(host, 'rb') as f:
            head = f.read(2)
            if head != b'#!':
                return
            text = (head + f.read()).decode(errors='replace')
        interp = text.split('\n', 1)[0][2:].split()
        if not interp or not self.command(interp[0]):
            self.fail('%s: interpreter %s not found' % (path, interp[0] if interp else '?'))
        for tool in sorted(set(TOOL_RE.findall(text))):
            if self.command(tool):
                continue
            guarded = re.search(r'-[xefs] "?%s\b' % re.escape(tool), text)
            (self.warn if guarded else self.fail)('%s: calls %s, not in this rootfs%s' % (
                path, tool, ' (guarded)' if guarded else ''))
        for tool in sorted(set(COMMAND_V_RE.findall(text))):
            if not self.command(tool):
                self.warn('%s: optional tool %s is absent, that branch never runs' % (path, tool))

    def run(self, require_cmds=()):
        for path in sorted(self.ours):
            if not resolve(self.root, path):
                self.fail('%s: shipped by the overlay but missing from the image' % path)
            elif '/systemd/system/' in path and (path.endswith(UNIT_SUFFIX) or path.endswith('.conf')):
                self.unit(path)
            elif os.path.isfile(resolve(self.root, path)):
                self.script(path)
        for cmd in require_cmds:
            if not self.command(cmd):
                self.fail('the suite runs %s, not in this rootfs' % cmd)
        return not self.fails


def mount_unit(path):
    path = path.strip('/')
    if not path:
        return '-.mount'
    return ''.join(c if c.isalnum() or c in ':_.' or c == '/' else '\\x%02x' % ord(c)
                   for c in path).replace('/', '-') + '.mount'


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split('\n')[0])
    ap.add_argument('root')
    ap.add_argument('--overlay', required=True)
    ap.add_argument('--require-cmd', action='append', default=[])
    a = ap.parse_args(argv)
    audit = Audit(a.root, a.overlay)
    ok = audit.run(a.require_cmd)
    for m in audit.warns:
        print('WARN ' + m)
    for m in audit.fails:
        print('FAIL ' + m)
    print('audit: %d files, %d FAIL, %d WARN' % (len(audit.ours), len(audit.fails), len(audit.warns)))
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
