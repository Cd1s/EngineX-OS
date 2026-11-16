"""The final rename keeps one-time upgrade migration and no compatibility link."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import az01_packer as az
import legacy_names as legacy


class UpgradeContractTest(unittest.TestCase):
    def migrate(self, data):
        script = az.MIGRATE.replace('/data', str(data))
        p = subprocess.run(['sh', '-s'], input=script, text=True, capture_output=True)
        self.assertEqual(p.returncode, 0, p.stderr)

    def test_upgrade_moves_real_data_and_removes_only_our_link(self):
        for kind in ('directory', 'relative', 'absolute', 'foreign', 'foreign-broken'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp:
                data = Path(tmp) / 'data'; data.mkdir()
                new, old = data / 'enginex', data / legacy.DATA_NAME
                if kind == 'directory':
                    old.mkdir(); (old / 'music.flac').write_bytes(b'user music')
                else:
                    new.mkdir(); (new / 'music.flac').write_bytes(b'user music')
                    foreign = data / 'Music'; foreign.mkdir(); (foreign / 'keep').write_text('keep')
                    target = {'relative': 'enginex', 'absolute': str(new),
                              'foreign': 'Music', 'foreign-broken': 'missing'}[kind]
                    old.symlink_to(target)
                for _ in range(2):
                    self.migrate(data)
                self.assertEqual((new / 'music.flac').read_bytes(), b'user music')
                if kind.startswith('foreign'):
                    self.assertEqual(os.readlink(old), target)
                    self.assertEqual((foreign / 'keep').read_text(), 'keep')
                else:
                    self.assertFalse(os.path.lexists(old))

    def test_upgrade_rewrites_owned_values_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            data = Path(tmp) / 'data'; new = data / 'enginex'
            (new / 'ui').mkdir(parents=True)
            settings = new / 'ui/settings.conf'
            settings.write_text('console_url=http://' + legacy.PROJECT + '.lan:8080\n'
                                'name=SC5000\nfour_deck_role=24\n')
            (new / 'ui-scale.env').write_text(legacy.PROJECT.upper() + '_UI_SCALE=0.75\n')
            (new / 'state').mkdir()
            socket = new / 'state/socket.json'
            socket.write_text(json.dumps({'socket': '/run/' + legacy.PROJECT + '-tailscaled.sock'}))
            paused = new / 'mods-state/club/.paused'
            paused.parent.mkdir(parents=True)
            paused.write_text(legacy.DEEMIX + '.service\n' + legacy.DEEMIX_UNIT + '\nforeign.service\n')
            foreign = data / 'Music'
            foreign.mkdir()
            (foreign / '.paused').write_text(legacy.DEEMIX + '.service\n')
            (new / 'mods-state/foreign').symlink_to(foreign)
            alsa = data / 'system/var/overlay/lib/alsa/conf.d'
            alsa.mkdir(parents=True)
            (alsa / legacy.ALSA_FILE).write_text('lib "' + str(data / legacy.DATA_NAME) + '/bundle/lib/libengine-ui-hook.so"\nfunc ' + legacy.PROJECT + '_alsa_boot_hook\n')
            accounts = data / 'StreamingAccounts.json'
            account = {'accessToken': legacy.PROJECT + '-token', 'refreshToken': legacy.PROJECT + '-refresh'}
            accounts.write_text(json.dumps({'TIDAL': json.dumps(account), 'other': 'real-token-unchanged'}))
            self.migrate(data)
            self.assertEqual(settings.read_text(), 'console_url=http://enginex.lan:8080\nname=SC5000\nfour_deck_role=24\n')
            self.assertEqual((new / 'ui-scale.env').read_text(), 'ENGINEX_UI_SCALE=0.75\n')
            self.assertEqual(json.loads(socket.read_text()), {'socket': '/run/enginex-tailscaled.sock'})
            self.assertEqual(paused.read_text(), 'enginex-deemixd.service\nenginex-deemixd.service\nforeign.service\n')
            self.assertEqual((foreign / '.paused').read_text(), legacy.DEEMIX + '.service\n')
            self.assertFalse((alsa / legacy.ALSA_FILE).exists())
            self.assertEqual((alsa / '99-enginex.conf').read_text(), 'lib "' + str(new) + '/bundle/lib/libengine-ui-hook.so"\nfunc enginex_alsa_boot_hook\n')
            migrated = json.loads(accounts.read_text())
            self.assertEqual(json.loads(migrated['TIDAL']), {'accessToken': 'enginex', 'refreshToken': 'enginex'})
            self.assertEqual(migrated['other'], 'real-token-unchanged')
            stamp = settings.stat().st_mtime_ns
            self.migrate(data)
            self.assertEqual(settings.stat().st_mtime_ns, stamp)

    def test_two_real_directories_preserve_settings_and_conflicts(self):
        with tempfile.TemporaryDirectory() as tmp:
            data = Path(tmp) / 'data'; old, new = data / legacy.DATA_NAME, data / 'enginex'
            (old / 'ui').mkdir(parents=True); (new / 'ui/packs').mkdir(parents=True)
            (old / 'ui/settings.conf').write_text('four_deck_role=24\n')
            (old / 'music.flac').write_bytes(b'old recording')
            (new / 'music.flac').write_bytes(b'current recording')
            self.migrate(data)
            self.assertEqual((new / 'ui/settings.conf').read_text(), 'four_deck_role=24\n')
            self.assertEqual((new / 'music.flac').read_bytes(), b'current recording')
            self.assertEqual([p.read_bytes() for p in new.glob('legacy-data.*/music.flac')], [b'old recording'])
            self.assertFalse(os.path.lexists(old))
