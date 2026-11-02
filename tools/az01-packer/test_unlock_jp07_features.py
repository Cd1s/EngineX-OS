import struct
import unittest

import unlock_jp07_features as u


def stock():
    # Synthetic image: only the patched sites and anchor strings hold stock bytes.
    buf = bytearray(u.UNLOCKED_STR_OFF + 16)
    buf[u.UNLOCKED_STR_OFF:u.UNLOCKED_STR_OFF + 9] = b"Unlocked\x00"
    buf[u.FALSE_STR_OFF:u.FALSE_STR_OFF + 6] = b"False\x00"
    for offset, expected, _, _ in u.PATCHES:
        buf[offset:offset + len(expected)] = expected
    buf[0x33649C] = 0x30
    return buf


def decode(buf, offset, n, key):
    return bytes(b - key for b in buf[offset:offset + n])


class UnlockJP07Test(unittest.TestCase):
    def test_sites_disjoint_and_same_length(self):
        spans = sorted((o, o + len(e)) for o, e, _, _ in u.PATCHES)
        for (_, end), (start, _) in zip(spans, spans[1:]):
            self.assertLessEqual(end, start)
        for offset, expected, replacement, desc in u.PATCHES:
            self.assertEqual(len(expected), len(replacement), desc)
            self.assertNotEqual(expected, replacement, desc)

    def test_stock_tidal_urls_decode(self):
        buf = stock()
        self.assertEqual(decode(buf, 0x336740, 24, 1) + bytes([buf[0x33649C] - 1]),
                         b"https://api.tidal.com/v1/")
        self.assertEqual(decode(buf, 0x336758, 24, 7) + decode(buf, 0x33652C, 2, 7),
                         b"https://auth.tidal.com/v1/")

    def test_patch_applies_bluetooth_filesystem_and_tidal(self):
        out = u.patch_engine_bytes(bytes(stock()))
        word = lambda off: struct.unpack_from("<I", out, off)[0]
        self.assertEqual(word(0xF62BB4), 0xE5C4324C)
        self.assertEqual(word(0xF65A48), 0xE5C4324E)
        self.assertEqual(word(0x1897E70), 0xE3A06004)
        self.assertEqual(word(0x1897DCC), 0x03A04001)
        self.assertEqual(word(0x1678644), 0xE320F000)
        self.assertEqual(word(0x58AF34), 0xE3A03000)
        self.assertEqual(word(0x167295C), 0xE3A00000)
        self.assertEqual(word(0x16728C0), 0xE3A00000)
        self.assertEqual(decode(out, 0x336740, 24, 1) + bytes([out[0x33649C] - 1]),
                         b"http://127.0.0.1:8080/v1/")
        self.assertEqual(decode(out, 0x336758, 24, 7) + decode(out, 0x33652C, 2, 7),
                         b"http://127.0.0.1:8080/au//")

    def test_mismatch_refused(self):
        for offset, expected, _, desc in u.PATCHES:
            buf = stock()
            buf[offset] ^= 0xFF
            with self.subTest(desc), self.assertRaises(ValueError):
                u.patch_engine_bytes(bytes(buf))

    def test_already_patched_refused(self):
        with self.assertRaises(ValueError):
            u.patch_engine_bytes(u.patch_engine_bytes(bytes(stock())))


if __name__ == "__main__":
    unittest.main()
