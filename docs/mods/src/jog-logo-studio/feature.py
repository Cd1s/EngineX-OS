"""600px media-profile logos. Existing artwork is preserved for disable/restore."""
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
from __main__ import media_file, save_json


class Feature:
    def __init__(self, state):
        self.state = state
        self.path = state / "logo.png"
        self.config = {"sync": False, "volumes": [], "enabled": False}
        if (state / "config.json").exists():
            self.config.update(json.loads((state / "config.json").read_text()))

    def status(self):
        return dict(self.config, ready=self.path.is_file(), preview=self.path.as_uri(),
                    note="Reload the media profile on each player after changing a logo.")

    def library(self, volume):
        path = Path(volume).resolve(strict=True)
        if not path.is_dir() or not any(path.is_relative_to(Path(root)) and path != Path(root) for root in ("/media", "/mnt")):
            raise ValueError("choose a media volume under /media or /mnt")
        library = path / "Engine Library"
        if not library.is_dir() or library.is_symlink():
            raise ValueError("media must already contain an Engine Library directory")
        return library

    def action(self, request):
        action = request["action"]
        if action == "crop":
            source = media_file(request["source"])
            self.state.mkdir(parents=True, exist_ok=True)
            temp = self.state / "logo.new.png"
            subprocess.run(["ffmpeg", "-nostdin", "-v", "error", "-y", "-i", str(source),
                            "-vf", "scale=600:600:force_original_aspect_ratio=increase,crop=600:600",
                            "-frames:v", "1", "-update", "1", str(temp)], check=True, capture_output=True, timeout=30)
            header = temp.read_bytes()[:24]
            if header[:8] != b"\x89PNG\r\n\x1a\n" or struct.unpack(">II", header[16:24]) != (600, 600):
                raise ValueError("image conversion did not produce a 600x600 PNG")
            os.replace(temp, self.path)
        elif action == "apply":
            volumes = request["volumes"]
            if not isinstance(volumes, list) or not 1 <= len(volumes) <= 16:
                raise ValueError("select 1..16 volume paths")
            if type(request["sync"]) is not bool or type(request["enabled"]) is not bool:
                raise ValueError("sync and enabled must be boolean")
            if request["enabled"] and not self.path.is_file():
                raise ValueError("crop an image first")
            libraries = list(dict.fromkeys(self.library(v) for v in (volumes if request["sync"] else volumes[:1])))
            # Validate every selected target before changing any target.
            for library in libraries:
                for name in ("logo.png", ".enginex-logo-original.png", ".enginex-logo-owned.json", ".enginex-logo-new.png"):
                    if (library / name).is_symlink():
                        raise ValueError("refusing a symlinked logo or backup")
            for library in libraries:
                target = library / "logo.png"
                backup = library / ".enginex-logo-original.png"
                marker = library / ".enginex-logo-owned.json"
                if request["enabled"]:
                    if not marker.exists():
                        if backup.exists():
                            raise ValueError("unowned logo backup already exists; resolve it before applying")
                        if target.exists():
                            shutil.copy2(target, backup)
                        save_json(marker, {"hadOriginal": target.exists()})
                    temp = library / ".enginex-logo-new.png"
                    shutil.copyfile(self.path, temp)
                    os.replace(temp, target)
                elif marker.exists():
                    owned = json.loads(marker.read_text())
                    if owned["hadOriginal"]:
                        if not backup.is_file():
                            raise ValueError("original logo backup is missing; refusing to remove current logo")
                        os.replace(backup, target)
                    else:
                        target.unlink(missing_ok=True)
                    marker.unlink()
            self.config = {key: request[key] for key in ("sync", "volumes", "enabled")}
            save_json(self.state / "config.json", self.config)
        else:
            raise ValueError("unknown action")
        return self.status()

    def close(self):
        pass
