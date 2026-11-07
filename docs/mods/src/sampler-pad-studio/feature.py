"""An independent eight-file bank, not an undocumented native sampler binding."""
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import uuid
import wave
from __main__ import media_file, save_json


class Feature:
    def __init__(self, state):
        self.state = state
        self.slots = [{"name": f"Pad {i + 1}", "path": "", "duration": 0} for i in range(8)]
        if (state / "bank.json").exists():
            bank = json.loads((state / "bank.json").read_text())
            if not isinstance(bank, list) or len(bank) != 8:
                raise ValueError("bank must have exactly eight slots")
            self.slots = bank

    def status(self):
        return dict(slots=self.slots, nativeBinding=False,
                    note="Files are ready for loading via the native browser; pad numbers here identify this Mod's bank.")

    def action(self, request):
        pad = request["pad"]
        if type(pad) is not int or not 1 <= pad <= 8:
            raise ValueError("pad must be 1..8")
        index = pad - 1
        action = request["action"]
        previous = self.slots[index].copy()
        old_path = Path(previous["path"]) if previous["path"] else None
        if action == "rename":
            name = request["name"].strip()
            if not name or len(name) > 80 or any(ord(ch) < 32 for ch in name):
                raise ValueError("name must be 1..80 visible characters")
            self.slots[index] = dict(previous, name=name)
        elif action == "clear":
            # Never delete media files. Previous Mod exports remain available in history.
            self.slots[index] = {"name": f"Pad {pad}", "path": "", "duration": 0}
        elif action in ("import", "slice"):
            source = media_file(request["source"])
            self.state.mkdir(parents=True, exist_ok=True)
            target = self.state / f"Pad{pad}-{uuid.uuid4().hex}.wav"
            try:
                if action == "import":
                    subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-i", str(source), "-vn", "-c:a", "pcm_s16le", str(target)],
                                   check=True, capture_output=True, timeout=120)
                else:
                    start, end = request["inSample"], request["outSample"]
                    rate = request.get("sampleRate", 44100)
                    if any(type(value) is not int for value in (start, end, rate)) or not 0 <= start < end or not 8000 <= rate <= 384000:
                        raise ValueError("expected integer sample positions: 0 <= inSample < outSample and valid sampleRate")
                    if end - start > rate * 600:
                        raise ValueError("a loop slice is limited to ten minutes")
                    try:
                        wav = wave.open(str(source), "rb")
                    except (wave.Error, EOFError):
                        wav = None
                    if wav is not None:
                        with wav:
                            if wav.getframerate() != rate or end > wav.getnframes():
                                raise ValueError("sample rate mismatch or slice outside WAV bounds")
                            wav.setpos(start)
                            data = wav.readframes(end - start)
                            with wave.open(str(target), "wb") as out:
                                out.setparams(wav.getparams())
                                out.writeframes(data)
                    else:
                        subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-i", str(source), "-vn", "-ar", str(rate),
                                        "-af", f"aresample={rate},atrim=start_sample={start}:end_sample={end},asetpts=PTS-STARTPTS",
                                        "-c:a", "pcm_s16le", str(target)], check=True, capture_output=True, timeout=120)
                with wave.open(str(target), "rb") as out:
                    count, sample_rate = out.getnframes(), out.getframerate()
                    if count == 0 or (action == "slice" and count != end - start):
                        raise ValueError("decoded audio is empty or the requested slice exceeds source bounds")
                    self.slots[index] = dict(name=source.stem[:80], path=str(target), duration=count / sample_rate)
            except Exception:
                target.unlink(missing_ok=True)
                raise
        else:
            raise ValueError("unknown action")
        try:
            save_json(self.state / "bank.json", self.slots)
        except Exception:
            self.slots[index] = previous
            raise
        return self.status()

    def close(self):
        pass
