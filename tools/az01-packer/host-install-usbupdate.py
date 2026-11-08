#!/usr/bin/env python3
"""Flash a /data image over USB to a deck in Firmware Update Mode (1cc5:2027, az0x-splash usbupdate) (#116).

This is the write-part flow of the official libaz01-prog, checked on a Rane SYSTEM ONE:

    host -> "AZ0O" write-part header + "data\\0"   deck -> "AZ0I" status 5 (ready for raw bytes)
    host -> the image as raw bulk writes + a zero-length packet
    deck -> "AZ0I" status 1 (written)

It REPLACES the deck's /data partition. Needs pyusb and libusb:

    pip3 install pyusb    (+ macOS: brew install libusb; Windows: WinUSB driver via Zadig)

Usage:
    host-install-usbupdate.py <data.img>
"""
import argparse
import os
import struct
import sys

VID, PID = 0x1CC5, 0x2027
CMD_WRITE_PART = 0x5AA5FB04
ST_WRITTEN, ST_ENOENT, ST_READY = 1, 3, 5
CHUNK = 256 * 1024
PARTS = (b'data', b'userdata')  # AZ04 names it data, 32-bit AZ0x userdata


class FlashError(Exception):
    pass


def write_part_header(part, total, seq):
    """write-part request: magic, cmd, total_bytes, seq, 0, packet length, part offset 0, flags 1 (write if changed)."""
    name = part + b'\0'
    return struct.pack('<4sIQIHHQQ', b'AZ0O', CMD_WRITE_PART, total, seq, 0, 40 + len(name), 0, 1) + name


def send(ep_out, data, timeout):
    n = ep_out.write(data, timeout=timeout)
    if n != len(data):
        raise FlashError(f'short USB write: {n} of {len(data)} bytes')


def parse_reply(rx, cmd=CMD_WRITE_PART):
    if len(rx) < 24:
        raise FlashError(f'short reply from the deck: {bytes(rx).hex(" ")}')
    magic, got, _, seq, status, _len = struct.unpack('<4sIQIHH', bytes(rx[:24]))
    if magic != b'AZ0I' or got != cmd:
        raise FlashError(f'unexpected reply from the deck: {bytes(rx).hex(" ")}')
    return seq, status


def flash(ep_out, ep_in, f, total, out=sys.stdout):
    """Run write-part on ep_out/ep_in (pyusb endpoints or fakes); f is the image opened for reading. Returns the partition."""
    seq = 1
    for part in PARTS:
        send(ep_out, write_part_header(part, total, seq), 5000)
        _, status = parse_reply(ep_in.read(512, timeout=5000))
        seq += 1
        if status == ST_READY:
            break
        if status != ST_ENOENT:
            raise FlashError(f'deck refused write-part {part.decode()}: status {status}')
        print(f'partition {part.decode()} not found, trying the next name', file=out)
    else:
        raise FlashError('the deck has neither a data nor a userdata partition')
    sent, shown = 0, -1
    while sent < total:
        chunk = f.read(min(CHUNK, total - sent))
        if not chunk:
            raise FlashError(f'image ended after {sent} of {total} bytes')
        send(ep_out, chunk, 10000)
        sent += len(chunk)
        if (pct := sent * 100 // total) != shown:
            shown = pct
            print(f'\rwriting {part.decode()}: {pct:3d}% ({sent >> 20} / {total >> 20} MiB)', end='', file=out, flush=True)
    print(file=out)
    send(ep_out, b'', 30000)  # zero-length packet ends the raw stream
    _, status = parse_reply(ep_in.read(512, timeout=300000))
    if status != ST_WRITTEN:
        raise FlashError(f'deck did not confirm the write: status {status}')
    return part.decode()


def open_deck():
    try:
        import usb.core
        import usb.util
    except ImportError:
        raise FlashError('pyusb is missing: pip3 install pyusb (macOS also: brew install libusb)')
    try:
        dev = usb.core.find(idVendor=VID, idProduct=PID)
    except usb.core.NoBackendError:
        raise FlashError('libusb is missing: macOS brew install libusb; Linux apt install libusb-1.0-0; Windows pip install libusb-package')
    if dev is None:
        raise FlashError('no deck in Firmware Update Mode (USB 1cc5:2027) found')
    try:
        if sys.platform.startswith('linux') and dev.is_kernel_driver_active(0):
            dev.detach_kernel_driver(0)
        dev.set_configuration()
    except usb.core.USBError as e:
        raise FlashError(f'cannot open the deck ({e}): run with sudo, or add the udev rule for 1cc5')
    intf = dev.get_active_configuration()[(0, 0)]

    def ep(direction):
        return usb.util.find_descriptor(intf, custom_match=lambda e: usb.util.endpoint_direction(e.bEndpointAddress) == direction)
    return ep(usb.util.ENDPOINT_OUT), ep(usb.util.ENDPOINT_IN)


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    p.add_argument('image', help='ext4 /data image (host-install-userdata.py output)')
    a = p.parse_args(argv)
    total = os.path.getsize(a.image)
    try:
        ep_out, ep_in = open_deck()
        with open(a.image, 'rb') as f:
            part = flash(ep_out, ep_in, f, total)
    except FlashError as e:
        raise SystemExit(f'host-install-usbupdate: {e}')
    print(f'Wrote {a.image} to the {part} partition. Press the power button to turn the deck off and on again.')


if __name__ == '__main__':
    main()
