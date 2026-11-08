import importlib.util
import io
import struct
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('host_install_usbupdate', HERE / 'host-install-usbupdate.py')
hiu = importlib.util.module_from_spec(spec)
spec.loader.exec_module(hiu)


def reply(seq, status):
    return struct.pack('<4sIQIHH', b'AZ0I', hiu.CMD_WRITE_PART, 0, seq, status, 24)


class FakeDeck:
    """Bulk endpoints of az0x-splash usbupdate: replies are queued, writes recorded."""

    def __init__(self, *statuses):
        self.replies = [reply(i + 1, s) for i, s in enumerate(statuses)]
        self.writes = []

    def write(self, data, timeout):
        self.writes.append(bytes(data))
        return len(data)

    def read(self, size, timeout):
        return self.replies.pop(0)


class USBUpdateTest(unittest.TestCase):
    def test_header_matches_the_system_one_trace(self):
        # SBDJUK's TX on a Rane SYSTEM ONE (#116), seq 0x0a, 64 bytes.
        want = bytes.fromhex('41 5a 30 4f 04 fb a5 5a 40 00 00 00 00 00 00 00 0a 00 00 00 00 00 2d 00'
                             '00 00 00 00 00 00 00 00 01 00 00 00 00 00 00 00 64 61 74 61 00')
        self.assertEqual(hiu.write_part_header(b'data', 64, 0x0a), want)
        seq, status = hiu.parse_reply(bytes.fromhex('41 5a 30 49 04 fb a5 5a 00 00 00 00 00 00 00 00'
                                                    '0a 00 00 00 05 00 18 00'))
        self.assertEqual((seq, status), (0x0a, hiu.ST_READY))

    def test_streams_raw_bytes_then_zlp(self):
        data = bytes(range(256)) * 4096 + b'tail'  # 1 MiB + 4: last chunk is short
        deck, out = FakeDeck(hiu.ST_READY, hiu.ST_WRITTEN), io.StringIO()
        self.assertEqual(hiu.flash(deck, deck, io.BytesIO(data), len(data), out), 'data')
        self.assertEqual(deck.writes[0], hiu.write_part_header(b'data', len(data), 1))
        self.assertEqual(b''.join(deck.writes[1:-1]), data)  # no AZ0O framing on the data
        self.assertTrue(all(len(w) <= hiu.CHUNK for w in deck.writes[1:-1]))
        self.assertEqual(deck.writes[-1], b'')
        self.assertIn('100% (1 / 1 MiB)', out.getvalue())

    def test_falls_back_to_userdata(self):
        deck = FakeDeck(hiu.ST_ENOENT, hiu.ST_READY, hiu.ST_WRITTEN)
        self.assertEqual(hiu.flash(deck, deck, io.BytesIO(b'x' * 10), 10, io.StringIO()), 'userdata')
        self.assertEqual(deck.writes[1], hiu.write_part_header(b'userdata', 10, 2))

    def test_errors(self):
        for statuses, size, msg in (
            ((2,), 10, 'status 2'),
            ((hiu.ST_ENOENT, hiu.ST_ENOENT), 10, 'neither'),
            ((hiu.ST_READY, 7), 10, 'did not confirm'),
            ((hiu.ST_READY,), 20, 'image ended'),  # file shorter than announced
        ):
            deck = FakeDeck(*statuses)
            with self.assertRaisesRegex(hiu.FlashError, msg):
                hiu.flash(deck, deck, io.BytesIO(b'x' * 10), size, io.StringIO())
        with self.assertRaisesRegex(hiu.FlashError, 'unexpected reply'):
            hiu.parse_reply(b'AZ0O' + bytes(20))
        with self.assertRaisesRegex(hiu.FlashError, 'short reply'):
            hiu.parse_reply(b'AZ0I')
        with self.assertRaisesRegex(hiu.FlashError, 'unexpected reply'):  # right magic, other command
            hiu.parse_reply(struct.pack('<4sIQIHH', b'AZ0I', 0x5AA5FB05, 0, 1, 5, 24))

    def test_short_usb_write_is_an_error(self):
        deck = FakeDeck(hiu.ST_READY, hiu.ST_WRITTEN)
        deck.write = lambda data, timeout: len(data) - 1
        with self.assertRaisesRegex(hiu.FlashError, 'short USB write'):
            hiu.flash(deck, deck, io.BytesIO(b'x' * 10), 10, io.StringIO())


if __name__ == '__main__':
    unittest.main()
