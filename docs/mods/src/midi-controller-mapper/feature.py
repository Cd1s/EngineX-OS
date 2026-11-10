"""Generic MIDI Controller Mapper Mod helper (Pioneer CDJ-400 first target).

Runs at lowest scheduling priority (os.nice(19)), reads USB MIDI controllers via
ALSA sequencer (libasound.so.2) without touching Denon's internal KnownDevices
SysEx gate or switching hardware layers, integrates CDJ-400 continuous jog wheel
velocity (Bn 10, 64=stop) over dt, enforces soft takeover with ▲▼ indicators on
the tempo slider (Bn 1D), and coalesces Engine property updates at ~60 Hz.
"""
import ctypes
from pathlib import Path
import json
import os
import select
import threading
import time
from __main__ import save_json

HERE = Path(__file__).resolve().parent
BUILTIN_PRESETS_DIR = HERE / "presets"

SND_SEQ_OPEN_DUPLEX = 3
SND_SEQ_NONBLOCK = 1
SND_SEQ_PORT_CAP_READ = 1 << 0
SND_SEQ_PORT_CAP_WRITE = 1 << 1
SND_SEQ_PORT_CAP_SUBS_READ = 1 << 5
SND_SEQ_PORT_CAP_SUBS_WRITE = 1 << 6
SND_SEQ_PORT_TYPE_MIDI_GENERIC = 1 << 1
SND_SEQ_PORT_TYPE_APPLICATION = 1 << 20

SND_SEQ_EVENT_CLIENT_EXIT = 61
SND_SEQ_EVENT_PORT_EXIT = 64
SND_SEQ_EVENT_NONE = 255

VALID_ACTIONS = {
    "toggle_play",
    "hold_cue",
    "seek_forward",
    "seek_backward",
    "track_next",
    "track_prev",
    "loop_in",
    "loop_out",
    "reloop_exit",
    "cycle_tempo_range",
    "toggle_keylock",
    "toggle_vinyl_mode",
    "toggle_sync",
    "loop_halve",
    "loop_double",
    "load_selected_track",
    "toggle_browse_view",
    "browse_scroll",
}

VALID_JOG_ENCODINGS = {
    "cdj_continuous_velocity",
    "relative_center_64",
    "relative_2s_complement",
    "relative_sign_magnitude",
}


class pollfd(ctypes.Structure):
    _fields_ = [
        ("fd", ctypes.c_int),
        ("events", ctypes.c_short),
        ("revents", ctypes.c_short),
    ]


def validate_preset(data):
    if not isinstance(data, dict):
        raise ValueError("preset must be a JSON object")
    for req in ("id", "name", "jog", "tempoSlider", "controls"):
        if req not in data:
            raise ValueError(f"preset missing required field '{req}'")
    pid = str(data["id"]).strip()
    if not pid or any(c in pid for c in "/\\.."):
        raise ValueError("invalid preset id")
    jog = data["jog"]
    if not isinstance(jog, dict):
        raise ValueError("preset 'jog' must be an object")
    enc = jog.get("encoding", "cdj_continuous_velocity")
    if enc not in VALID_JOG_ENCODINGS:
        raise ValueError(f"unsupported jog encoding '{enc}'")
    for k in ("touchNote", "scratchCc", "pitchBendCc"):
        if k in jog and jog[k] is not None:
            v = int(jog[k])
            if not 0 <= v <= 127:
                raise ValueError(f"jog.{k} out of MIDI range 0..127")
    ts = data["tempoSlider"]
    if not isinstance(ts, dict) or not 0 <= int(ts.get("cc", 29)) <= 127:
        raise ValueError("preset 'tempoSlider.cc' must be in 0..127")
    controls = data["controls"]
    if not isinstance(controls, dict) or not controls:
        raise ValueError("preset 'controls' must be a non-empty object")
    for cname, spec in controls.items():
        if not isinstance(spec, dict):
            raise ValueError(f"control '{cname}' must be an object")
        ctype = spec.get("type")
        if ctype not in ("note", "cc"):
            raise ValueError(f"control '{cname}' has invalid type '{ctype}'")
        num = int(spec.get("number", -1))
        if not 0 <= num <= 127:
            raise ValueError(f"control '{cname}' number {num} out of range 0..127")
        act = spec.get("action")
        if act not in VALID_ACTIONS:
            raise ValueError(f"control '{cname}' has unknown action '{act}'")
    return data


def percentile(sorted_vals, pct):
    if not sorted_vals:
        return 0.0
    idx = (len(sorted_vals) - 1) * (pct / 100.0)
    lo = int(idx)
    hi = min(lo + 1, len(sorted_vals) - 1)
    frac = idx - lo
    return round(sorted_vals[lo] * (1.0 - frac) + sorted_vals[hi] * frac, 3)


class Feature:
    def __init__(self, state_dir):
        self.state_dir = Path(state_dir)
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.user_presets_dir = self.state_dir / "presets"
        self.user_presets_dir.mkdir(parents=True, exist_ok=True)
        self.config_path = self.state_dir / "config.json"

        self.nice_level = self._apply_low_priority()
        self.lock = threading.RLock()

        self.presets = {}
        self._load_presets()

        saved = {}
        if self.config_path.is_file():
            try:
                saved = json.loads(self.config_path.read_text())
            except Exception:
                saved = {}

        default_preset = "pioneer-cdj-400" if "pioneer-cdj-400" in self.presets else next(iter(self.presets), "")
        self.enabled = bool(saved.get("enabled", False))
        self.target_deck = int(saved.get("targetDeck", 3))
        if self.target_deck not in (1, 2, 3, 4):
            self.target_deck = 3
        self.preset_id = str(saved.get("presetId", default_preset))
        if self.preset_id not in self.presets and default_preset:
            self.preset_id = default_preset
        self.selected_device = str(saved.get("selectedDevice", "auto"))

        preset = self.current_preset()
        self.vinyl_mode = (preset.get("jog", {}).get("modeDefault", "vinyl") == "vinyl") if preset else True

        # Live deck state reported by QML bridge
        self.deck_state = {
            "songLoaded": False,
            "playing": False,
            "posNorm": 0.0,
            "durationSec": 0.0,
            "speed": 0.5,
            "speedRangeIndex": 0,
            "speedRangeCount": 4,
            "keylock": False,
            "sync": False,
            "loopEnabled": False,
            "browseOpen": False,
        }

        # Continuous jog & pitch-bend integration state
        self.jog_touched = False
        self.was_playing_before_touch = False
        self.jog_velocity = 0
        self.last_jog_step_at = 0.0
        self.virtual_pos_norm = 0.0
        self.virtual_pos_valid = False
        self.pitch_bend_dir = 0  # -1 backward, 0 neutral, +1 forward
        self.pitch_bend_amount = 0.0
        self.last_pitch_bend_at = 0.0

        # Soft takeover state machine for tempo slider (Bn 1D)
        self.fader_norm = 0.5
        self.fader_seen = False
        self.soft_locked = False
        self.last_written_speed = None
        self.last_written_speed_at = 0.0
        self.speed_offset_up = False
        self.speed_offset_down = False
        self.speed_neutral = True

        # Coalesced Engine property operations waiting for QML bridge poll
        self.pending_values = {}
        self.pending_actions = []
        self.pending_batch_t0 = None
        self.seq = 0
        self.inflight_batches = {}

        # Safety & telemetry counters
        self.error = ""
        self.connected = False
        self.connected_client = -1
        self.connected_port = -1
        self.connected_name = ""
        self.event_count = 0
        self.malformed_count = 0
        self.rate_window_start = time.monotonic()
        self.rate_window_events = 0
        self.events_per_sec = 0.0
        self.last_midi_hex = ""
        self.last_control_label = ""
        self.last_delta_pos_sec = 0.0
        self.latency_samples = []
        self.last_poll_at = 0.0

        # CPU usage tracking for helper process
        self._last_cpu_time = time.process_time()
        self._last_wall_time = time.monotonic()
        self.cpu_percent = 0.0

        # ALSA sequencer handle & background thread
        self.asound = None
        self.seq_handle = ctypes.c_void_p()
        self.in_port = -1
        self.midi_dev = ctypes.c_void_p()
        self.stop_event = threading.Event()
        self.thread = None
        self._init_alsa()
        if self.asound and self.seq_handle.value:
            self.thread = threading.Thread(target=self._alsa_loop, name="midi-alsa-seq", daemon=True)
            self.thread.start()

    def _apply_low_priority(self):
        try:
            cur = os.nice(0)
            if cur < 19:
                return os.nice(19 - cur)
            return cur
        except OSError:
            return 0

    def _load_presets(self):
        self.presets.clear()
        for folder in (BUILTIN_PRESETS_DIR, self.user_presets_dir):
            if not folder.is_dir():
                continue
            for p in sorted(folder.glob("*.json")):
                try:
                    raw = json.loads(p.read_text(encoding="utf-8"))
                    validate_preset(raw)
                    self.presets[raw["id"]] = raw
                except Exception:
                    continue

    def current_preset(self):
        return self.presets.get(self.preset_id) or next(iter(self.presets.values()), {})

    def _save_config(self):
        save_json(
            self.config_path,
            {
                "enabled": self.enabled,
                "targetDeck": self.target_deck,
                "presetId": self.preset_id,
                "selectedDevice": self.selected_device,
            },
        )

    def _init_alsa(self):
        try:
            asound = ctypes.CDLL("libasound.so.2")
            seq = ctypes.c_void_p()
            if asound.snd_seq_open(ctypes.byref(seq), b"default", SND_SEQ_OPEN_DUPLEX, SND_SEQ_NONBLOCK) < 0:
                return
            asound.snd_seq_set_client_name(seq, b"EngineX-MIDI-Mapper")
            port = asound.snd_seq_create_simple_port(
                seq,
                b"MIDI Mapper Input",
                SND_SEQ_PORT_CAP_WRITE | SND_SEQ_PORT_CAP_SUBS_WRITE | SND_SEQ_PORT_CAP_READ | SND_SEQ_PORT_CAP_SUBS_READ,
                SND_SEQ_PORT_TYPE_MIDI_GENERIC | SND_SEQ_PORT_TYPE_APPLICATION,
            )
            if port < 0:
                asound.snd_seq_close(seq)
                return
            # Subscribe to System:Announce (0:1) for instant USB disconnect notifications
            asound.snd_seq_connect_from(seq, port, 0, 1)
            mdev = ctypes.c_void_p()
            asound.snd_midi_event_new(64, ctypes.byref(mdev))
            if mdev.value:
                asound.snd_midi_event_no_status(mdev, 1)
            self.asound = asound
            self.seq_handle = seq
            self.in_port = port
            self.midi_dev = mdev
            if self.enabled:
                self._try_connect_device()
        except Exception:
            self.asound = None

    def list_alsa_devices(self):
        if not self.asound or not self.seq_handle.value:
            return []
        asound = self.asound
        seq = self.seq_handle
        my_client = asound.snd_seq_client_id(seq)
        cinfo = ctypes.create_string_buffer(256)
        pinfo = ctypes.create_string_buffer(256)
        asound.snd_seq_client_info_set_client(cinfo, -1)
        asound.snd_seq_client_info_get_name.restype = ctypes.c_char_p
        asound.snd_seq_port_info_get_name.restype = ctypes.c_char_p

        devices = []
        while asound.snd_seq_query_next_client(seq, cinfo) >= 0:
            cid = asound.snd_seq_client_info_get_client(cinfo)
            if cid in (0, 14, my_client):
                continue
            cname = (asound.snd_seq_client_info_get_name(cinfo) or b"").decode("utf-8", "replace").strip()
            if cname.startswith("Midi::Out::") or cname.startswith("EngineX-"):
                continue
            asound.snd_seq_port_info_set_client(pinfo, cid)
            asound.snd_seq_port_info_set_port(pinfo, -1)
            while asound.snd_seq_query_next_port(seq, pinfo) >= 0:
                pid = asound.snd_seq_port_info_get_port(pinfo)
                caps = asound.snd_seq_port_info_get_capability(pinfo)
                if (caps & SND_SEQ_PORT_CAP_READ) and (caps & SND_SEQ_PORT_CAP_SUBS_READ):
                    pname = (asound.snd_seq_port_info_get_name(pinfo) or b"").decode("utf-8", "replace").strip()
                    devices.append(
                        {
                            "id": f"{cid}:{pid}",
                            "client": cid,
                            "port": pid,
                            "clientName": cname,
                            "portName": pname,
                            "label": f"{cname} [{cid}:{pid}]",
                        }
                    )
        return devices

    def _try_connect_device(self):
        with self.lock:
            if not self.asound or not self.seq_handle.value:
                return False
            devices = self.list_alsa_devices()
            if not devices:
                self.connected = False
                return False
            chosen = None
            if self.selected_device and self.selected_device != "auto":
                for d in devices:
                    if d["id"] == self.selected_device or d["clientName"] == self.selected_device:
                        chosen = d
                        break
            if chosen is None:
                patterns = [p.lower() for p in self.current_preset().get("match", {}).get("alsaClientPatterns", [])]
                for d in devices:
                    cname_l = d["clientName"].lower()
                    if any(pat in cname_l for pat in patterns):
                        chosen = d
                        break
                if chosen is None and self.selected_device == "auto":
                    chosen = devices[0]
            if chosen is None:
                self.connected = False
                return False
            if self.connected and self.connected_client == chosen["client"] and self.connected_port == chosen["port"]:
                return True
            self._disconnect_alsa_locked()
            rc = self.asound.snd_seq_connect_from(
                self.seq_handle, self.in_port, chosen["client"], chosen["port"]
            )
            if rc < 0:
                self.connected = False
                return False
            self.connected = True
            self.connected_client = chosen["client"]
            self.connected_port = chosen["port"]
            self.connected_name = chosen["label"]
            self.error = ""
            return True

    def _disconnect_alsa_locked(self):
        if self.asound and self.seq_handle.value and self.connected_client >= 0 and self.connected_port >= 0:
            try:
                self.asound.snd_seq_disconnect_from(
                    self.seq_handle, self.in_port, self.connected_client, self.connected_port
                )
            except Exception:
                pass
        self.connected = False
        self.connected_client = -1
        self.connected_port = -1
        self.connected_name = ""

    def _auto_disable_on_error_locked(self, reason):
        self.enabled = False
        self.error = reason
        self.jog_velocity = 0
        self.jog_touched = False
        self.pitch_bend_dir = 0
        self.pitch_bend_amount = 0.0
        self.pending_values["nudgeFwd"] = 0.0
        self.pending_values["nudgeBwd"] = 0.0
        self.pending_values["cueHold"] = False
        self._disconnect_alsa_locked()

    def simulate_disconnect(self, client_id=None):
        with self.lock:
            if client_id is None or client_id == self.connected_client or self.enabled:
                cname = self.connected_name or "USB MIDI controller"
                self._auto_disable_on_error_locked(
                    f"MIDI controller disconnected ({cname}). Mapper auto-disabled for deck safety."
                )

    def _mark_batch_t0_locked(self, now):
        self.pending_batch_t0 = now

    def _alsa_loop(self):
        self._apply_low_priority()
        asound = self.asound
        seq = self.seq_handle
        npfds = asound.snd_seq_poll_descriptors_count(seq, select.POLLIN)
        pfds = (pollfd * max(1, npfds))()
        asound.snd_seq_poll_descriptors(seq, pfds, npfds, select.POLLIN)
        poller = select.poll()
        for i in range(npfds):
            poller.register(pfds[i].fd, pfds[i].events)

        ev_ptr = ctypes.c_void_p()
        midi_buf = (ctypes.c_ubyte * 32)()

        while not self.stop_event.is_set():
            with self.lock:
                active_motion = self.enabled and (self.jog_velocity != 0 or self.pitch_bend_dir != 0)
            timeout_ms = 16 if active_motion else 250
            poller.poll(timeout_ms)
            if self.stop_event.is_set():
                break
            now = time.monotonic()
            while True:
                rc = asound.snd_seq_event_input(seq, ctypes.byref(ev_ptr))
                if rc < 0 or not ev_ptr.value:
                    break
                raw = ctypes.string_at(ev_ptr.value, 28)
                ev_type = raw[0]
                if ev_type in (SND_SEQ_EVENT_CLIENT_EXIT, SND_SEQ_EVENT_PORT_EXIT):
                    addr_client = raw[16]
                    with self.lock:
                        if self.connected and addr_client == self.connected_client:
                            self.simulate_disconnect(addr_client)
                    continue
                if not self.enabled:
                    continue
                if self.midi_dev.value:
                    asound.snd_midi_event_reset_decode(self.midi_dev)
                    nbytes = asound.snd_midi_event_decode(self.midi_dev, midi_buf, 32, ev_ptr)
                    if nbytes > 0:
                        msg = [ int(midi_buf[i]) for i in range(nbytes) ]
                        self.process_midi_message(msg, now=now)
                    elif ev_type not in (60, 62, 63, 65, 66, 67):
                        self.record_malformed("unrecognized ALSA event payload", now=now)
            with self.lock:
                if self.enabled and (self.jog_velocity != 0 or self.pitch_bend_dir != 0):
                    self.integrate_jog_step(now=time.monotonic())

    def record_malformed(self, detail="malformed MIDI message", now=None):
        if now is None:
            now = time.monotonic()
        with self.lock:
            self.malformed_count += 1
            limit = int(self.current_preset().get("malformedLimit", 16))
            if self.malformed_count >= limit:
                self._auto_disable_on_error_locked(
                    f"Disabled due to repeated malformed MIDI data ({self.malformed_count} errors: {detail})."
                )

    def process_midi_message(self, raw_bytes, now=None):
        if now is None:
            now = time.monotonic()
        with self.lock:
            if not self.enabled:
                return False
            if not isinstance(raw_bytes, (list, tuple, bytes, bytearray)) or len(raw_bytes) < 2 or len(raw_bytes) > 3:
                self.record_malformed("invalid MIDI message length", now=now)
                return False
            b0 = int(raw_bytes[0])
            b1 = int(raw_bytes[1])
            b2 = int(raw_bytes[2]) if len(raw_bytes) > 2 else 0
            if not (0x80 <= b0 <= 0xEF) or not (0 <= b1 <= 127) or not (0 <= b2 <= 127):
                self.record_malformed("invalid MIDI status or data byte", now=now)
                return False

            preset = self.current_preset()
            # Storm protection
            if now - self.rate_window_start >= 1.0:
                self.events_per_sec = round(self.rate_window_events / max(0.001, now - self.rate_window_start), 1)
                self.rate_window_start = now
                self.rate_window_events = 0
            self.rate_window_events += 1
            storm_limit = int(preset.get("stormLimitPerSec", 2400))
            if self.rate_window_events > storm_limit:
                self._auto_disable_on_error_locked(
                    f"MIDI message storm detected (>{storm_limit} msg/s). Auto-disabled to protect audio engine."
                )
                return False

            ch_cfg = preset.get("midiChannel", "any")
            msg_ch = b0 & 0x0F
            if ch_cfg != "any" and int(ch_cfg) != msg_ch:
                return False

            status_hi = b0 & 0xF0
            self.event_count += 1
            self.last_midi_hex = f"{b0:02X} {b1:02X} {b2:02X}"

            # Note On / Note Off
            if status_hi in (0x80, 0x90):
                pressed = (status_hi == 0x90) and (b2 > 0)
                return self._handle_note_locked(b1, pressed, preset, now)

            # Control Change (CC)
            if status_hi == 0xB0:
                return self._handle_cc_locked(b1, b2, preset, now)

            return False

    def _handle_note_locked(self, note, pressed, preset, now):
        jog = preset.get("jog", {})
        touch_note = jog.get("touchNote")
        if touch_note is not None and int(touch_note) == note:
            self.last_control_label = "Jog Platter Touch" if pressed else "Jog Platter Release"
            self._mark_batch_t0_locked(now)
            self.jog_touched = bool(pressed)
            self.last_jog_step_at = now
            self.pending_values["vinylMode"] = bool(self.vinyl_mode)
            self.pending_values["jogTouch"] = bool(pressed)
            if not pressed:
                self.jog_velocity = 0
            return True

        for _, spec in preset.get("controls", {}).items():
            if spec.get("type") == "note" and int(spec.get("number", -1)) == note:
                act = spec.get("action")
                self.last_control_label = spec.get("label", act)
                self._mark_batch_t0_locked(now)
                if act == "hold_cue":
                    self.pending_values["cueHold"] = bool(pressed)
                    return True
                if not pressed:
                    return True
                if act == "toggle_vinyl_mode":
                    self.vinyl_mode = not self.vinyl_mode
                    self.pending_values["vinylMode"] = self.vinyl_mode
                    return True
                self.pending_actions.append({"action": act})
                return True
        return False

    def _handle_cc_locked(self, cc, value, preset, now):
        jog = preset.get("jog", {})
        scratch_cc = jog.get("scratchCc")
        pb_cc = jog.get("pitchBendCc")

        # Jog scratch / continuous velocity CC (CDJ-400 Bn 10)
        if scratch_cc is not None and int(scratch_cc) == cc:
            self._mark_batch_t0_locked(now)
            enc = jog.get("encoding", "cdj_continuous_velocity")
            center = int(jog.get("centerValue", 64))
            if enc == "cdj_continuous_velocity":
                # Integrate elapsed time at previous velocity before updating velocity
                if self.jog_velocity != 0 and self.last_jog_step_at > 0:
                    self.integrate_jog_step(now=now)
                self.jog_velocity = value - center
                self.last_jog_step_at = now
                self.last_control_label = f"Jog Scratch Vel ({self.jog_velocity:+d})"
                if self.jog_velocity == 0:
                    self.pitch_bend_dir = 0
                    self.pitch_bend_amount = 0.0
                    self.pending_values["nudgeFwd"] = 0.0
                    self.pending_values["nudgeBwd"] = 0.0
                else:
                    # Apply immediate initial micro-step (4 ms equivalent) for zero-wait responsiveness
                    self.integrate_jog_step(dt_override=0.004, now=now)
                return True
            else:
                delta = self._decode_relative_cc(value, enc, center)
                self._apply_jog_delta_sec_locked(
                    (delta / 63.0) * 0.016 * float(jog.get("scratchSensitivitySecPerSec", 1.8)),
                    now,
                )
                return True

        # Outer-ring pitch bend CC (CDJ-400 Bn 30)
        if pb_cc is not None and int(pb_cc) == cc:
            self._mark_batch_t0_locked(now)
            center = int(jog.get("centerValue", 64))
            max_off = max(1, int(jog.get("maxOffset", 63)))
            max_amt = float(jog.get("pitchBendMaxAmount", 0.65))
            offset = value - center
            self.last_control_label = f"Jog Pitch Bend ({offset:+d})"
            if offset == 0:
                self.pitch_bend_dir = 0
                self.pitch_bend_amount = 0.0
                self.pending_values["nudgeFwd"] = 0.0
                self.pending_values["nudgeBwd"] = 0.0
            elif offset > 0:
                amt = min(1.0, (offset / max_off) * max_amt)
                self.pitch_bend_dir = 1
                self.pitch_bend_amount = amt
                self.last_pitch_bend_at = now
                self.pending_values["nudgeFwd"] = round(amt, 4)
                self.pending_values["nudgeBwd"] = 0.0
            else:
                amt = min(1.0, (abs(offset) / max_off) * max_amt)
                self.pitch_bend_dir = -1
                self.pitch_bend_amount = amt
                self.last_pitch_bend_at = now
                self.pending_values["nudgeFwd"] = 0.0
                self.pending_values["nudgeBwd"] = round(amt, 4)
            return True

        # Tempo slider CC (CDJ-400 Bn 1D)
        ts = preset.get("tempoSlider", {})
        if int(ts.get("cc", 29)) == cc:
            self._mark_batch_t0_locked(now)
            norm = value / 127.0 if value != 64 else 0.5
            if ts.get("invert", False):
                norm = 1.0 - norm
            self._handle_tempo_slider_locked(norm, ts)
            return True

        # Other CC controls (e.g. Browse rotary selector Bn 4F)
        for _, spec in preset.get("controls", {}).items():
            if spec.get("type") == "cc" and int(spec.get("number", -1)) == cc:
                act = spec.get("action")
                if act == "browse_scroll":
                    enc = spec.get("encoding", "relative_center_64")
                    steps = self._decode_relative_cc(value, enc, 64)
                    if steps != 0:
                        self._mark_batch_t0_locked(now)
                        self.last_control_label = f"{spec.get('label', 'Browse')} ({steps:+d})"
                        self.pending_actions.append({"action": "browse_scroll", "steps": steps})
                        return True
        return False

    def _decode_relative_cc(self, value, encoding, center=64):
        if encoding == "relative_2s_complement":
            return value if value < 64 else value - 128
        if encoding == "relative_sign_magnitude":
            return -(value & 0x3F) if (value & 0x40) else (value & 0x3F)
        return value - center

    def _handle_tempo_slider_locked(self, norm, ts_cfg):
        pickup = float(ts_cfg.get("pickupThreshold", 0.025))
        deadband = float(ts_cfg.get("neutralDeadband", 0.004))
        use_soft = bool(ts_cfg.get("softTakeover", True))
        deck_speed = float(self.deck_state.get("speed", 0.5))

        prev_fader = self.fader_norm if self.fader_seen else norm
        self.fader_norm = norm
        self.fader_seen = True

        if not use_soft:
            self.soft_locked = True
        else:
            # Check if physical fader crossed or entered pickup window around deck_speed
            crossed = (prev_fader <= deck_speed <= norm) or (norm <= deck_speed <= prev_fader)
            close_enough = abs(norm - deck_speed) <= pickup
            if crossed or close_enough:
                self.soft_locked = True

        if self.soft_locked:
            self.speed_offset_up = False
            self.speed_offset_down = False
            self.speed_neutral = abs(norm - 0.5) <= deadband
            target_speed = 0.5 if self.speed_neutral else round(norm, 5)
            self.last_written_speed = target_speed
            self.last_written_speed_at = time.monotonic()
            self.deck_state["speed"] = target_speed
            self.pending_values["speed"] = target_speed
            self.last_control_label = f"Tempo Slider ({(target_speed - 0.5) * 200.0:+.1f}%)"
        else:
            # Waiting for soft takeover pickup: indicate direction to move physical fader
            self.speed_offset_up = norm < (deck_speed - pickup)
            self.speed_offset_down = norm > (deck_speed + pickup)
            self.speed_neutral = False
            arrow = "▲" if self.speed_offset_up else ("▼" if self.speed_offset_down else "●")
            self.last_control_label = f"Tempo Soft Takeover {arrow}"

        self.pending_values["speedOffsetUp"] = self.speed_offset_up
        self.pending_values["speedOffsetDown"] = self.speed_offset_down
        self.pending_values["speedNeutral"] = self.speed_neutral

    def integrate_jog_step(self, dt_override=None, now=None):
        """Time-integrates CDJ-400 continuous jog velocity (Bn 10, 64=stop):
        Δpos_sec = (velocity / maxOffset) * dt * scratchSensitivitySecPerSec.
        """
        if now is None:
            now = time.monotonic()
        with self.lock:
            if not self.enabled:
                return 0.0
            jog = self.current_preset().get("jog", {})
            pb_timeout_sec = float(jog.get("pitchBendTimeoutMs", 120)) / 1000.0
            if self.pitch_bend_dir != 0 and self.last_pitch_bend_at > 0 and (now - self.last_pitch_bend_at) > pb_timeout_sec and self.jog_velocity == 0:
                self.pitch_bend_dir = 0
                self.pitch_bend_amount = 0.0
                self.pending_values["nudgeFwd"] = 0.0
                self.pending_values["nudgeBwd"] = 0.0
                self._mark_batch_t0_locked(now)

            if self.jog_velocity == 0:
                self.last_jog_step_at = now
                return 0.0

            if dt_override is not None:
                dt = float(dt_override)
            else:
                dt = max(0.0, min(0.1, now - self.last_jog_step_at)) if self.last_jog_step_at > 0 else 0.016
            self.last_jog_step_at = now
            if dt <= 0.0:
                return 0.0

            max_off = max(1, int(jog.get("maxOffset", 63)))
            sens = float(jog.get("scratchSensitivitySecPerSec", 1.8))
            playing = bool(self.deck_state.get("playing", False))

            # If playing in CDJ mode (or Vinyl mode without platter touch), Bn 10 acts as pitch bend
            if playing and not self.jog_touched and not self.was_playing_before_touch:
                max_amt = float(jog.get("pitchBendMaxAmount", 0.65))
                amt = min(1.0, (abs(self.jog_velocity) / max_off) * max_amt)
                self._mark_batch_t0_locked(now)
                if self.jog_velocity > 0:
                    self.pitch_bend_dir = 1
                    self.pitch_bend_amount = amt
                    self.pending_values["nudgeFwd"] = round(amt, 4)
                    self.pending_values["nudgeBwd"] = 0.0
                else:
                    self.pitch_bend_dir = -1
                    self.pitch_bend_amount = amt
                    self.pending_values["nudgeFwd"] = 0.0
                    self.pending_values["nudgeBwd"] = round(amt, 4)
                return 0.0

            delta_sec = (self.jog_velocity / float(max_off)) * dt * sens
            self._apply_jog_delta_sec_locked(delta_sec, now)
            return delta_sec

    def _apply_jog_delta_sec_locked(self, delta_sec, now):
        dur = float(self.deck_state.get("durationSec", 0.0))
        if dur <= 0.01:
            dur = 180.0
        base_pos = self.virtual_pos_norm if self.virtual_pos_valid else float(self.deck_state.get("posNorm", 0.0))
        next_pos = max(0.0, min(1.0, base_pos + (delta_sec / dur)))
        self.virtual_pos_norm = next_pos
        self.virtual_pos_valid = True
        self.deck_state["posNorm"] = next_pos
        self.last_delta_pos_sec = round(delta_sec, 5)
        # 26325.0 unnormalized ExternalScratchWheelMove ticks = 1.0s of audio in Engine 5.3.0
        prev_ticks = float(self.pending_values.get("jogMoveTicks", 0.0))
        self.pending_values["jogMoveTicks"] = round(prev_ticks + delta_sec * 26325.0, 2)
        self.pending_values["setPosNorm"] = round(next_pos, 6)
        self._mark_batch_t0_locked(now)

    def update_deck_state(self, state):
        if not isinstance(state, dict):
            return
        with self.lock:
            for k in (
                "songLoaded",
                "playing",
                "posNorm",
                "durationSec",
                "speedRangeIndex",
                "speedRangeCount",
                "keylock",
                "sync",
                "loopEnabled",
                "browseOpen",
            ):
                if k in state and state[k] is not None:
                    self.deck_state[k] = state[k]

            if not self.jog_touched and self.jog_velocity == 0 and "posNorm" in state and state["posNorm"] is not None:
                self.virtual_pos_norm = float(state["posNorm"])
                self.virtual_pos_valid = True

            if "speed" in state and state["speed"] is not None:
                new_speed = float(state["speed"])
                now_m = time.monotonic()
                if self.last_written_speed is not None and (now_m - self.last_written_speed_at) < 0.35:
                    pass
                else:
                    self.deck_state["speed"] = new_speed
                    pickup = float(self.current_preset().get("tempoSlider", {}).get("pickupThreshold", 0.025))
                    if self.fader_seen and abs(new_speed - self.fader_norm) > pickup:
                        if self.last_written_speed is None or abs(new_speed - self.last_written_speed) > (pickup * 0.6):
                            self.soft_locked = False
                            self.speed_offset_up = self.fader_norm < (new_speed - pickup)
                            self.speed_offset_down = self.fader_norm > (new_speed + pickup)
                            self.speed_neutral = False
                            self.pending_values["speedOffsetUp"] = self.speed_offset_up
                            self.pending_values["speedOffsetDown"] = self.speed_offset_down
                            self.pending_values["speedNeutral"] = self.speed_neutral

    def poll_bridge(self, deck_state=None, ack_seq=None, ack_apply_ms=None, now=None):
        if now is None:
            now = time.monotonic()
        with self.lock:
            self.last_poll_at = now
            if ack_seq is not None:
                queue_wait_ms = self.inflight_batches.pop(int(ack_seq), None)
                if queue_wait_ms is not None:
                    apply_ms = float(ack_apply_ms) if ack_apply_ms is not None else 2.5
                    lat_ms = max(0.1, min(250.0, queue_wait_ms + max(0.2, apply_ms)))
                    self.latency_samples.append(round(lat_ms, 3))
                    if len(self.latency_samples) > 500:
                        self.latency_samples = self.latency_samples[-500:]
                if len(self.inflight_batches) > 64:
                    self.inflight_batches.clear()

            if deck_state:
                self.update_deck_state(deck_state)

            if self.enabled and (self.jog_velocity != 0 or self.pitch_bend_dir != 0):
                self.integrate_jog_step(now=now)

            if not self.enabled or (not self.pending_values and not self.pending_actions):
                return {
                    "enabled": self.enabled,
                    "targetDeck": self.target_deck,
                    "seq": 0,
                    "values": {},
                    "actions": [],
                }

            self.seq += 1
            seq = self.seq
            t0 = self.pending_batch_t0 if self.pending_batch_t0 is not None else now
            queue_wait_ms = max(0.05, (now - t0) * 1000.0)
            self.inflight_batches[seq] = queue_wait_ms
            self.pending_batch_t0 = None
            values = dict(self.pending_values)
            actions = list(self.pending_actions)
            self.pending_values.clear()
            self.pending_actions.clear()
            return {
                "enabled": self.enabled,
                "targetDeck": self.target_deck,
                "seq": seq,
                "values": values,
                "actions": actions,
            }

    def _update_cpu_percent(self):
        now_wall = time.monotonic()
        now_cpu = time.process_time()
        dt_wall = now_wall - self._last_wall_time
        if dt_wall >= 0.25:
            dt_cpu = max(0.0, now_cpu - self._last_cpu_time)
            self.cpu_percent = round((dt_cpu / dt_wall) * 100.0, 2)
            self._last_wall_time = now_wall
            self._last_cpu_time = now_cpu

    def latency_summary(self):
        with self.lock:
            if not self.latency_samples:
                return {"count": 0, "p50Ms": 0.0, "p95Ms": 0.0, "maxMs": 0.0, "avgMs": 0.0}
            s = sorted(self.latency_samples)
            return {
                "count": len(s),
                "p50Ms": percentile(s, 50),
                "p95Ms": percentile(s, 95),
                "maxMs": round(s[-1], 3),
                "avgMs": round(sum(s) / len(s), 3),
            }

    def status(self):
        with self.lock:
            self._update_cpu_percent()
            if self.enabled and not self.connected:
                self._try_connect_device()
            preset = self.current_preset()
            led_cfg = preset.get("ledFeedback", {})
            devices = self.list_alsa_devices()
            return {
                "enabled": self.enabled,
                "running": self.enabled and self.connected,
                "connected": self.connected,
                "connectedDevice": self.connected_name,
                "selectedDevice": self.selected_device,
                "devices": devices,
                "targetDeck": self.target_deck,
                "presetId": self.preset_id,
                "presetName": preset.get("name", self.preset_id),
                "presets": [
                    {"id": k, "name": v.get("name", k), "model": v.get("model", "")}
                    for k, v in sorted(self.presets.items())
                ],
                "vinylMode": self.vinyl_mode,
                "softTakeover": {
                    "locked": self.soft_locked,
                    "faderNorm": round(self.fader_norm, 4),
                    "deckSpeedNorm": round(float(self.deck_state.get("speed", 0.5)), 4),
                    "speedOffsetUp": self.speed_offset_up,
                    "speedOffsetDown": self.speed_offset_down,
                    "speedNeutral": self.speed_neutral,
                    "indicator": "▲" if self.speed_offset_up else ("▼" if self.speed_offset_down else "●"),
                },
                "telemetry": {
                    "nice": self.nice_level,
                    "cpuPercent": self.cpu_percent,
                    "eventCount": self.event_count,
                    "malformedCount": self.malformed_count,
                    "eventsPerSec": self.events_per_sec,
                    "lastMidiHex": self.last_midi_hex,
                    "lastControl": self.last_control_label,
                    "jogVelocity": self.jog_velocity,
                    "jogTouched": self.jog_touched,
                    "lastDeltaPosSec": self.last_delta_pos_sec,
                    "latency": self.latency_summary(),
                },
                "ledFeedbackSupported": bool(led_cfg.get("supported", False)),
                "ledFeedbackNote": led_cfg.get("note", ""),
                "error": self.error,
            }

    def action(self, req):
        act = req.get("action")
        if act == "poll":
            return self.poll_bridge(
                deck_state=req.get("deckState"),
                ack_seq=req.get("ackSeq"),
                ack_apply_ms=req.get("ackApplyMs"),
            )
        with self.lock:
            if act == "enable":
                self.error = ""
                self.malformed_count = 0
                self.enabled = True
                self._try_connect_device()
                self._save_config()
                return self.status()
            if act == "disable":
                self.enabled = False
                self.jog_velocity = 0
                self.jog_touched = False
                self.pitch_bend_dir = 0
                self._disconnect_alsa_locked()
                self._save_config()
                return self.status()
            if act == "configure":
                if "targetDeck" in req:
                    td = int(req["targetDeck"])
                    if td not in (1, 2, 3, 4):
                        raise ValueError("targetDeck must be 1, 2, 3, or 4")
                    self.target_deck = td
                if "presetId" in req:
                    pid = str(req["presetId"])
                    self._load_presets()
                    if pid not in self.presets:
                        raise ValueError(f"unknown preset '{pid}'")
                    self.preset_id = pid
                    self.vinyl_mode = self.current_preset().get("jog", {}).get("modeDefault", "vinyl") == "vinyl"
                if "selectedDevice" in req:
                    self.selected_device = str(req["selectedDevice"])
                    self._disconnect_alsa_locked()
                if "enabled" in req:
                    self.enabled = bool(req["enabled"])
                    if self.enabled:
                        self.error = ""
                        self.malformed_count = 0
                if self.enabled:
                    self._try_connect_device()
                else:
                    self._disconnect_alsa_locked()
                self._save_config()
                return self.status()
            if act == "refresh_devices":
                self._load_presets()
                if self.enabled:
                    self._try_connect_device()
                return self.status()
            if act == "save_preset":
                preset_obj = req.get("preset")
                validate_preset(preset_obj)
                pid = preset_obj["id"]
                save_json(self.user_presets_dir / f"{pid}.json", preset_obj)
                self._load_presets()
                return self.status()
            if act == "inject_midi":
                # Used by unit tests and sandbox benchmark harness
                messages = req.get("messages", [])
                if not isinstance(messages, list):
                    raise ValueError("messages must be a list")
                for m in messages:
                    self.process_midi_message(m)
                return self.status()
            if act == "reset_telemetry":
                self.latency_samples.clear()
                self.inflight_batches.clear()
                self.pending_batch_t0 = None
                self.event_count = 0
                self.malformed_count = 0
                return self.status()
        raise ValueError(f"unknown action '{act}'")

    def close(self):
        self.stop_event.set()
        if self.thread and self.thread.is_alive():
            self.thread.join(timeout=0.5)
        with self.lock:
            self._disconnect_alsa_locked()
            if self.asound:
                if self.midi_dev.value:
                    self.asound.snd_midi_event_free(self.midi_dev)
                    self.midi_dev = ctypes.c_void_p()
                if self.seq_handle.value:
                    self.asound.snd_seq_close(self.seq_handle)
                    self.seq_handle = ctypes.c_void_p()
