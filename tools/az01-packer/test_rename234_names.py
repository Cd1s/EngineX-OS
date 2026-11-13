#!/usr/bin/env python3
"""#234 (5.3.0.5): what ships carries the EngineX names; every sc5000 left is on this list.

Shipped means the rootfs overlay, the deck's Go programs (suite, installer and their packages),
the hooks, the UI pack and Mod sources (docs/mods/src), the bundle and image builders and the user-run tools.
Tests, docs and the relay/stems server side (cmd/sc5000-relay, deploy/) are out of scope.

Each rule names the files it applies to and why the old name stays. A new sc5000 in a shipped
file fails here until it is renamed, or added to a rule with its reason.
"""
import fnmatch
import os
import re
import unittest
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

SHIPPED = [
    'rootfs-overlay', 'cmd/enginex-suite', 'cmd/enginex-installer', 'pkg', 'internal',
    'tools/ui-mod', 'tools/edisksd-hook', 'tools/mods', 'tools/diagnose', 'tools/tailscale',
    'tools/broadcast', 'tools/splash', 'tools/drmgrab', 'tools/stems', 'docs/mods/src',
    'tools/az01-packer/build-data-bundle.py', 'tools/az01-packer/az01_packer.py',
    'tools/az01-packer/host-install-userdata.py', 'tools/az01-packer/rootfs_audit.py',
]
NOT_SHIPPED = ['*_test.go', '*/test_*', '*.md', '*.so', '*.png', '*.jpg', '*.ttf', '*.gz', '*.bin',
               '*/__pycache__/*', '*/testdata/*']

ANY = ['*']
BUNDLE = ['tools/az01-packer/build-data-bundle.py']
UPDATERS = BUNDLE + ['pkg/webui/update.go', 'cmd/enginex-installer/main.go',
                     'tools/az01-packer/host-install-userdata.py', 'tools/ui-mod/engine_ui_hook.c']
SUITE = ['cmd/enginex-suite/main.go', 'rootfs-overlay/usr/local/bin/enginex-suite-exec']
PACKER = ['tools/az01-packer/az01_packer.py', 'tools/az01-packer/rootfs_audit.py']
STEMS = ['internal/stems/*', 'internal/stemsd/*', 'internal/stemsproc/*', 'tools/stems/*',
         'pkg/webui/relay.go', 'pkg/webui/ui_settings.go']

# (files, pattern, reason). In order, every match is cut from the line; a line that still says
# sc5000 afterwards is a finding.
ALLOW = [
    (ANY, r'99-sc5000-custom\.conf|\.99-sc5000-custom\.conf\.new|\w*sc5000_alsa_boot_hook\w*',
     'frozen: the ALSA conf file and hook symbol every installed deck loads Engine through'),
    (ANY, r'/data/sc5000-custom/bundle/lib/libengine-ui-hook\.so',
     'frozen: the hook library path the ALSA conf names'),
    (ANY, r'/data/sc5000-custom\b[\w./-]*|(?<![\w-])(?:legacy-)?sc5000-custom(?![\w.-])',
     '/data/sc5000-custom stays the compat link to /data/enginex (#103), and its readers'),
    (ANY, r'/v1/sc5000\b',
     'the permanent API alias older installers, UI packs and Mods call'),
    (ANY, r'/etc/sc5000(?:/version\.txt|/release-tag|/(?![\w.-])|(?![\w/.-]))',
     '/etc/sc5000 is the image\'s link to /etc/enginex; older decks keep their version there'),
    (ANY, r'\bSC5000(?:M|/SC6000|/SC5000M)?\b(?!_)|\bSC5000-',
     'the Denon model name and official image asset names'),
    (ANY, r'\bSC5000_(?:CUSTOM|ALSA_OVERLAY|UI_SCALE|STEMS_\w*|STEMSD|RELAY)\b|\\nSC5000_UI_SCALE',
     'environment of older updaters and hand-run tools, read under both names'),
    (ANY, r'\bsc5000\.lan\b', 'treated like enginex.lan as a placeholder, not a real console URL'),
    (STEMS + ['pkg/webui/*', 'internal/*', 'tools/stems/*'],
     r'\bsc5000-(?:stemsd|relay)\b|/etc/sc5000-stems/token|/opt/sc5000-stems\b[\w./-]*|https://relay\.example/sc5000|sc5000-stems-',
     'server side keeps its names (relay, stemsd, /etc/sc5000-stems); stems list ids are persisted'),
    (UPDATERS + SUITE + ['pkg/webui/*', 'internal/stems/client.go', 'docs/mods/src/minimal-club-mode/feature.go'],
     r'\bsc5000-suite\b[\w.]*|\bsc5000-deemixd\b|enginex-sc5000-deemixd\.service|99-sc5000\.conf'
     r'|/run/sc5000(?:-[\w.-]+)?\b|/sc5000-engine-script|/sc5000-suite\.|\$\w+/sc5000-(?:suite|deemixd)\w*',
     'names of releases before 5.3.0.5: ended, accepted and cleaned up at an in-place update'),
    (['pkg/webui/tailscale.go', 'pkg/mods/*', 'tools/mods/*', 'tools/tailscale/*', 'docs/mods/src/*'],
     r'\bsc5000-tailscaled\b|/run/sc5000-tailscaled\.sock',
     'the tailscale unit and socket name of releases before 5.3.0.5 and of installed Mods'),
    (['tools/edisksd-hook/*'], r'sc5000-hfs-views?\b|/run/sc5000-hfs-views',
     'views the hook of an older release mounted stay hidden after an in-place update'),
    (['pkg/webui/shadow_debug.go'], r'/tmp/sc5000-debug', 'debug flag file of older releases'),
    (PACKER, r'sc5000(?:/stems-runtime-prepare)?\b|sc5000-custom-\*?|dnsmasq\.d/sc5000-telemetry\.conf'
     r"|'sc5000'|\bsc5000\b",
     'etc-reset removes upper-layer copies under the old names; the audit knows them'),
    (['pkg/webui/radios.go'], r'sc5000-debloat\b', 'a deck whose read-only root still has it'),
    (['pkg/webui/spotify_mdns.go'], r'"sc5000"|\bsc5000\.local\b', 'the stock firmware\'s avahi host name'),
    (['pkg/webui/tidal_gateway.go'], r'\bsc5000-shadow-decks\b', 'a past incident, by name'),
    (['*'], r'/media/sc5000-sd-cache\b[\w./-]*', 'dead since 8856176; legacy read path'),
    (['pkg/webui/update.go', 'tools/az01-packer/build-data-bundle.py'], r'\(/etc/sc5000\b|\bsc5000\b',
     'comments about the layout before 5.3.0.5'),
]
ALLOW = [(files, re.compile(pat), why) for files, pat, why in ALLOW]

# Overlay entries that keep an sc5000 name are links to the enginex one (#234).
OVERLAY_LINKS = {
    'etc/sc5000': 'enginex',
    'usr/local/lib/sc5000': 'enginex',
    'usr/share/sc5000-custom': 'enginex',
    'usr/local/bin/sc5000-debloat': 'enginex-debloat',
    'usr/local/bin/sc5000-perf-tune': 'enginex-perf-tune',
    'usr/local/bin/sc5000-prolink-diag': 'enginex-prolink-diag',
    'usr/local/bin/sc5000-storage-mount': 'enginex-storage-mount',
    'usr/sbin/sc5000-bt-helper': 'enginex-bt-helper',
    'usr/sbin/sc5000-usb-audio': 'enginex-usb-audio',
}


def shipped_files(repo):
    for top in SHIPPED:
        base = repo / top
        if base.is_file():
            yield top
            continue
        for d, dirs, files in os.walk(base):
            for f in files:
                rel = os.path.relpath(os.path.join(d, f), repo)
                if os.path.islink(os.path.join(d, f)):
                    continue
                if any(fnmatch.fnmatch(rel, p) or fnmatch.fnmatch(f, p) for p in NOT_SHIPPED):
                    continue
                yield rel


def findings(repo):
    out = []
    for rel in shipped_files(repo):
        try:
            text = (repo / rel).read_text()
        except (UnicodeDecodeError, OSError):
            continue
        rules = [r for files, r, _ in ALLOW if any(fnmatch.fnmatch(rel, f) for f in files)]
        for n, line in enumerate(text.splitlines(), 1):
            if 'sc5000' not in line.lower():
                continue
            rest = line
            for r in rules:
                rest = r.sub('', rest)
            if 'sc5000' in rest.lower():
                out.append(f'{rel}:{n}: {line.strip()[:160]}')
    return out


class Rename234NamesTest(unittest.TestCase):
    def test_234_shipped_files_say_sc5000_only_where_the_allowlist_says_why(self):
        found = findings(REPO)
        self.assertEqual(found, [], 'sc5000 outside the #234 allowlist:\n' + '\n'.join(found))

    def test_234_overlay_entries_named_sc5000_are_links_to_the_enginex_ones(self):
        ov = REPO / 'rootfs-overlay'
        named = sorted(os.path.relpath(os.path.join(d, n), ov)
                       for d, dirs, files in os.walk(ov) for n in dirs + files if 'sc5000' in n)
        self.assertEqual(named, sorted(OVERLAY_LINKS))
        for rel, target in OVERLAY_LINKS.items():
            self.assertTrue((ov / rel).is_symlink(), rel)
            self.assertEqual(os.readlink(ov / rel), target, rel)

    def test_234_the_check_finds_a_new_old_name(self):
        """The scan itself works: an old unit name in a shipped file is a finding."""
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'pkg/webui').mkdir(parents=True)
            (root / 'pkg/webui/x.go').write_text(
                '// /data/sc5000-custom and /v1/sc5000/ui are fine\n'
                'const unit = "sc5000-custom.target"\n'
                'const old = "/etc/sc5000/perf.conf"\n')
            self.assertEqual([f.split(':')[1] for f in findings(root)], ['2', '3'])


if __name__ == '__main__':
    unittest.main()
