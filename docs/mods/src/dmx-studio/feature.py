"""Generic DMX console. Hardware/fixture calibration is intentionally explicit."""
import ctypes
import ctypes.util
import fcntl
import ipaddress
import json
import math
import os
from pathlib import Path
import socket
import struct
import termios
import threading
import time
import uuid
from __main__ import save_json

# Channel numbers are offsets from fixture start, not vendor-specific personalities.
PROFILES = {
    "LED Par": ["dimmer", "red", "green", "blue"],
    "LED Bar": ["dimmer", "red", "green", "blue", "strobe"],
    "Moving Head Beam": ["pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "strobe"],
    "Moving Head Wash": ["pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "beam", "strobe"],
    "Moving Head Spot": ["pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "beam", "strobe"],
    "Moving Head 3-in-1": ["pan", "panFine", "tilt", "tiltFine", "dimmer", "red", "green", "blue", "gobo", "beam", "mode", "strobe"],
    "Smoke": ["smoke"],
}


def number(value, low, high):
    value = float(value)
    if not math.isfinite(value) or not low <= value <= high:
        raise ValueError(f"expected number in {low}..{high}")
    return value


def integer(value, low, high):
    parsed = number(value, low, high)
    if int(parsed) != parsed:
        raise ValueError("expected an integer")
    return int(parsed)


def artnet(universe, sequence, frame):
    return b"Art-Net\x00" + struct.pack("<H", 0x5000) + struct.pack(">HBB", 14, sequence, 0) + struct.pack("<H", universe - 1) + struct.pack(">H", 512) + bytes(frame)


def sacn(universe, sequence, frame, cid, terminated=False):
    packet = bytearray(638)
    packet[:16] = b"\x00\x10\x00\x00ASC-E1.17\x00\x00\x00"
    struct.pack_into(">HI", packet, 16, 0x7000 | 622, 4)
    packet[22:38] = cid
    struct.pack_into(">HI", packet, 38, 0x7000 | 600, 2)
    packet[44:108] = b"EngineX DMX Studio".ljust(64, b"\x00")
    packet[108] = 100
    packet[111] = sequence
    packet[112] = 0x40 if terminated else 0
    struct.pack_into(">H", packet, 113, universe)
    struct.pack_into(">HBBHHH", packet, 115, 0x7000 | 523, 2, 0xA1, 0, 1, 513)
    packet[125] = 0
    packet[126:] = bytes(frame)
    return bytes(packet)


class USB:
    """One uDMX per selected bus/address; libusb is a system prerequisite."""
    def __init__(self, bus, address):
        lib = ctypes.util.find_library("usb-1.0")
        if not lib:
            raise RuntimeError("uDMX requires preinstalled libusb-1.0")
        self.lib = ctypes.CDLL(lib)
        L = self.lib
        ptr = ctypes.c_void_p
        L.libusb_init.argtypes = [ctypes.POINTER(ptr)]
        L.libusb_exit.argtypes = [ptr]
        L.libusb_get_device_list.argtypes = [ptr, ctypes.POINTER(ctypes.POINTER(ptr))]
        L.libusb_get_device_list.restype = ctypes.c_ssize_t
        L.libusb_free_device_list.argtypes = [ctypes.POINTER(ptr), ctypes.c_int]
        L.libusb_get_device_descriptor.argtypes = [ptr, ptr]
        L.libusb_get_bus_number.argtypes = [ptr]
        L.libusb_get_device_address.argtypes = [ptr]
        L.libusb_open.argtypes = [ptr, ctypes.POINTER(ptr)]
        L.libusb_close.argtypes = [ptr]
        L.libusb_control_transfer.argtypes = [ptr, ctypes.c_uint8, ctypes.c_uint8, ctypes.c_uint16,
                                             ctypes.c_uint16, ptr, ctypes.c_uint16, ctypes.c_uint]
        self.context, self.handle = ptr(), ptr()
        if L.libusb_init(ctypes.byref(self.context)) != 0:
            raise RuntimeError("libusb init failed")
        devices = ctypes.POINTER(ptr)()
        count = L.libusb_get_device_list(self.context, ctypes.byref(devices))
        try:
            for index in range(max(0, count)):
                device = devices[index]
                descriptor = ctypes.create_string_buffer(18)
                if L.libusb_get_device_descriptor(device, descriptor) != 0:
                    continue
                vid, pid = struct.unpack_from("=HH", descriptor.raw, 8)
                if (vid, pid) == (0x16C0, 0x05DC) and L.libusb_get_bus_number(device) == bus and L.libusb_get_device_address(device) == address:
                    if L.libusb_open(device, ctypes.byref(self.handle)) != 0:
                        raise RuntimeError("uDMX open failed; check USB permissions")
                    break
            if not self.handle:
                raise RuntimeError("uDMX 16c0:05dc not found at selected bus/address")
        except Exception:
            L.libusb_exit(self.context)
            raise
        finally:
            if count >= 0:
                L.libusb_free_device_list(devices, 1)

    def send(self, frame):
        data = (ctypes.c_ubyte * 512).from_buffer_copy(bytes(frame))
        # uDMX SET_CHANNEL_RANGE: vendor OUT, request=2, count=512, start=0.
        result = self.lib.libusb_control_transfer(self.handle, 0x40, 2, 512, 0, data, 512, 100)
        if result != 512:
            raise RuntimeError(f"uDMX transfer failed: {result}")

    def close(self):
        self.lib.libusb_close(self.handle)
        self.lib.libusb_exit(self.context)


class Serial:
    def __init__(self, device):
        path = Path(device).resolve(strict=True)
        if not str(path).startswith(("/dev/ttyUSB", "/dev/ttyACM")):
            raise ValueError("FTDI device must be /dev/ttyUSB* or /dev/ttyACM*")
        self.fd = os.open(path, os.O_WRONLY | os.O_NOCTTY | os.O_NONBLOCK)
        try:
            # Linux termios2 BOTHER: 250000 baud, 8N2. Stock tty driver must support it.
            settings = bytearray(44)
            fcntl.ioctl(self.fd, 0x802C542A, settings)
            struct.pack_into("=IIII", settings, 0, 0, 0, 0x1000 | 0x30 | 0x40 | 0x80 | 0x800, 0)
            struct.pack_into("=II", settings, 36, 250000, 250000)
            fcntl.ioctl(self.fd, 0x402C542B, settings)
            os.set_blocking(self.fd, True)
        except Exception:
            os.close(self.fd)
            raise

    def send(self, frame):
        termios.tcdrain(self.fd)
        fcntl.ioctl(self.fd, 0x5427)  # TIOCSBRK
        time.sleep(0.00012)
        fcntl.ioctl(self.fd, 0x5428)  # TIOCCBRK
        time.sleep(0.000012)
        data = memoryview(b"\x00" + bytes(frame))
        while data:
            sent = os.write(self.fd, data)
            if sent <= 0:
                raise RuntimeError("short DMX serial write")
            data = data[sent:]

    def close(self):
        os.close(self.fd)


class Feature:
    def __init__(self, state):
        self.state = state
        self.config = dict(outputs=[], patch=[], bpm=120, effect="Circle", speed=4,
                           pan=32768, tilt=32768, width=16000, height=16000,
                           intensity=0, red=255, green=255, blue=255,
                           smokeLevel=0, smokeInterval=30, smokeDuration=2, smokeArmed=False)
        if (state / "show.json").exists():
            self.config.update(self.validated(json.loads((state / "show.json").read_text())))
        self.config["smokeArmed"] = False
        self.flash_until = 0
        self.running = False
        self.error = ""
        self.sequence = 0
        self.cid = uuid.uuid4().bytes
        self.adapters = []
        self.frames = {}
        self.lock = threading.RLock()
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.loop, daemon=True)
        self.thread.start()

    def validated(self, config):
        c = dict(self.config, **config)
        if c["effect"] not in ("Circle", "8-Figure", "Pan-Tilt Sweep", "Static"):
            raise ValueError("unknown EFX")
        for key, bounds in dict(bpm=(20, 300), speed=(1, 64), pan=(0, 65535), tilt=(0, 65535), width=(0, 32767), height=(0, 32767), intensity=(0, 255), red=(0, 255), green=(0, 255), blue=(0, 255), smokeLevel=(0, 255), smokeInterval=(1, 3600), smokeDuration=(0, 60)).items():
            c[key] = number(c[key], *bounds)
        if c["smokeDuration"] > c["smokeInterval"]:
            raise ValueError("smoke duration cannot exceed interval")
        if type(c["smokeArmed"]) is not bool:
            raise ValueError("smokeArmed must be boolean")
        if not isinstance(c["outputs"], list) or len(c["outputs"]) > 8 or not isinstance(c["patch"], list) or len(c["patch"]) > 128:
            raise ValueError("maximum 8 outputs / 128 fixtures")
        used_devices = set()
        for output in c["outputs"]:
            integer(output["universe"], 1, 32768)
            protocol = output["protocol"]
            if protocol in ("artnet", "sacn"):
                ipaddress.IPv4Address(output["host"])
                if protocol == "sacn":
                    integer(output["universe"], 1, 63999)
            elif protocol == "ftdi":
                device = str(output["device"])
                if not device.startswith(("/dev/ttyUSB", "/dev/ttyACM")):
                    raise ValueError("invalid FTDI device")
                if device in used_devices:
                    raise ValueError("USB outputs must use different devices")
                used_devices.add(device)
            elif protocol == "udmx":
                key = (integer(output["bus"], 1, 255), integer(output["address"], 1, 127))
                if key in used_devices:
                    raise ValueError("USB outputs must use different devices")
                used_devices.add(key)
            else:
                raise ValueError("protocol must be artnet, sacn, ftdi or udmx")
        occupied = set()
        for fixture in c["patch"]:
            profile = PROFILES[fixture["profile"]]
            address = integer(fixture["address"], 1, 513 - len(profile))
            universe = integer(fixture["universe"], 1, 32768)
            for key in ("x", "y"):
                number(fixture.get(key, 0.5), 0, 1)
            for key in ("gobo", "beam", "mode", "strobe"):
                integer(fixture.get(key, 0), 0, 255)
            for slot in range(address, address + len(profile)):
                if (universe, slot) in occupied:
                    raise ValueError("fixture channels overlap")
                occupied.add((universe, slot))
        return c

    def render(self, now):
        c = self.config
        phase = now * c["bpm"] / 60 / c["speed"] * 2 * math.pi
        x, y = 0, 0
        if c["effect"] == "Circle":
            x, y = math.sin(phase), math.cos(phase)
        elif c["effect"] == "8-Figure":
            x, y = math.sin(phase), math.sin(2 * phase)
        elif c["effect"] == "Pan-Tilt Sweep":
            x, y = 2 / math.pi * math.asin(math.sin(phase)), math.sin(phase / 2)
        pan = int(max(0, min(65535, c["pan"] + x * c["width"])))
        tilt = int(max(0, min(65535, c["tilt"] + y * c["height"])))
        smoke = c["smokeArmed"] and (now < self.flash_until or now % c["smokeInterval"] < c["smokeDuration"])
        values = dict(pan=pan >> 8, panFine=pan & 255, tilt=tilt >> 8, tiltFine=tilt & 255,
                      dimmer=int(c["intensity"]), red=int(c["red"]), green=int(c["green"]), blue=int(c["blue"]),
                      smoke=int(c["smokeLevel"]) if smoke else 0)
        frames = {int(o["universe"]): bytearray(512) for o in c["outputs"]}
        for fixture in c["patch"]:
            frame = frames.setdefault(int(fixture["universe"]), bytearray(512))
            for offset, channel in enumerate(PROFILES[fixture["profile"]]):
                frame[int(fixture["address"]) - 1 + offset] = values.get(channel, int(fixture.get(channel, 0)))
        return frames

    def send(self, frames, terminated=False):
        self.sequence = self.sequence % 255 + 1
        failures = []
        for output, adapter in self.adapters:
            universe = int(output["universe"])
            frame = frames.get(universe, bytearray(512))
            try:
                if output["protocol"] == "artnet":
                    adapter.sendto(artnet(universe, self.sequence, frame), (output["host"], 6454))
                elif output["protocol"] == "sacn":
                    adapter.sendto(sacn(universe, self.sequence, frame, self.cid, terminated), (output["host"], 5568))
                else:
                    adapter.send(frame)
            except Exception as exc:
                # A failing route must not prevent blackout of remaining routes.
                failures.append(str(exc))
        if failures:
            raise RuntimeError("; ".join(failures))

    def blackout(self):
        self.running = False
        self.flash_until = 0
        self.config["smokeArmed"] = False
        self.frames = {int(o["universe"]): bytearray(512) for o in self.config["outputs"]}
        failures = []
        for _ in range(3):
            try:
                self.send(self.frames, terminated=True)
            except Exception as exc:
                failures.append(str(exc))
        if failures:
            self.error = "; ".join(failures)
        for _, adapter in self.adapters:
            adapter.close()
        self.adapters = []

    def loop(self):
        while not self.stop.wait(1 / 30):
            with self.lock:
                self.frames = self.render(time.monotonic())
                if self.running:
                    try:
                        self.send(self.frames)
                    except Exception as exc:
                        self.error = str(exc)
                        self.blackout()

    def status(self):
        with self.lock:
            return dict(config=self.config, profiles=PROFILES, running=self.running, error=self.error,
                        frames={str(key): list(value) for key, value in self.frames.items()})

    def action(self, request):
        with self.lock:
            action = request["action"]
            if action == "configure":
                config = self.validated(request["config"])
                if self.running and any(config[key] != self.config[key] for key in ("outputs", "patch")):
                    raise ValueError("stop output before changing patch or routes")
                save_json(self.state / "show.json", dict(config, smokeArmed=False))
                self.config = config
            elif action == "start":
                if not self.running:
                    if not self.config["outputs"]:
                        raise ValueError("configure an output first")
                    try:
                        for output in self.config["outputs"]:
                            protocol = output["protocol"]
                            if protocol in ("artnet", "sacn"):
                                adapter = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
                                adapter.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
                                adapter.settimeout(0.1)
                            elif protocol == "ftdi":
                                adapter = Serial(output["device"])
                            else:
                                adapter = USB(int(output["bus"]), int(output["address"]))
                            self.adapters.append((output, adapter))
                    except Exception:
                        self.blackout()
                        raise
                    self.running, self.error = True, ""
            elif action == "stop":
                self.blackout()
            elif action == "flash":
                self.flash_until = time.monotonic() + 0.5 if request.get("held") is True and self.running else 0
            else:
                raise ValueError("unknown action")
            return self.status()

    def close(self):
        self.stop.set()
        self.thread.join(timeout=2)
        with self.lock:
            self.blackout()
