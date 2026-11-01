import hashlib
import struct
import unittest

import inspect_fit
from az01_packer import SafetyError


def fit_fixture():
    names = ('compatible', 'description', 'inmusic,version', 'partition',
             'compression', 'data', 'algo', 'value')
    strings = b''
    offsets = {}
    for name in names:
        offsets[name] = len(strings)
        strings += name.encode()+b'\0'

    def node(name):
        b = name.encode()+b'\0'
        return struct.pack('>I', 1)+b+bytes(-len(b) % 4)

    def prop(name, value):
        return struct.pack('>III', 3, len(value), offsets[name])+value+bytes(-len(value) % 4)

    close = struct.pack('>I', 2)
    tree = node('')+prop('compatible', b'inmusic,jp07\0')
    tree += prop('description', b'JP07 upgrade image\0')+prop('inmusic,version', b'4.3.4\0')
    tree += node('images')
    for name in ('splash', 'recoverysplash', 'rootfs'):
        payload = ('payload-'+name).encode()
        tree += node(name)+prop('partition', name.encode()+b'\0')
        tree += prop('compression', b'xz\0')+prop('data', payload)
        tree += node('hash')+prop('algo', b'sha1\0')
        tree += prop('value', hashlib.sha1(payload).digest())+close+close
    tree += close+close+struct.pack('>I', 9)
    return (struct.pack('>10I', 0xd00dfeed, 56+len(tree)+len(strings), 56,
                        56+len(tree), 40, 17, 16, 0, len(strings), len(tree))+
            bytes(16)+tree+strings)


class FitTests(unittest.TestCase):
    def test_inline_sha1_records_and_offsets(self):
        data = fit_fixture()
        result = inspect_fit.inspect(data)
        self.assertEqual(result['version'], '4.3.4')
        self.assertTrue(result['verified'])
        for p in result['partitions']:
            self.assertEqual(data[p['offset']:p['offset']+p['size']],
                             ('payload-'+p['name']).encode())

    def test_bad_hash_and_truncation_are_refused(self):
        data = fit_fixture()
        pos = inspect_fit.inspect(data)['partitions'][0]['offset']
        bad = bytearray(data)
        bad[pos] ^= 1
        for d in (bytes(bad), data[:-1], data[:39]):
            with self.assertRaises(SafetyError):
                inspect_fit.inspect(d)


if __name__ == '__main__':
    unittest.main()
