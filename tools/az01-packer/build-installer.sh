#!/bin/sh
# Usage: sh tools/az01-packer/build-installer.sh <VERSION> <OUT_DIR>
# Writes the 4 EngineX-Installer-<VERSION>-<platform> archives into OUT_DIR with the
# flat layout shipped since 5.3.0.3: launcher + enginex-installer[.exe] + az01_packer.py
# + explicit legacy helpers and dependency notices. Zips are written with Python (no `zip` on
# the build host) so the executable bits survive for macOS users.
set -eu
V=$1
OUT=$(cd "$2" && pwd)
cd "$(dirname "$0")/../.."
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
for t in linux-amd64 macos-amd64 macos-arm64 windows-amd64; do
	os=${t%-*}
	arch=${t#*-}
	goos=$os
	[ "$os" = macos ] && goos=darwin
	exe=enginex-installer
	stemsd=enginex-stemsd
	if [ "$os" = windows ]; then
		exe=enginex-installer.exe
		stemsd=enginex-stemsd.exe
	fi
	d=$W/$t
	mkdir -p "$d/stems" "$d/third_party"
	CGO_ENABLED=0 GOOS=$goos GOARCH=$arch go build -trimpath -ldflags "-X main.installerVersion=$V" -o "$d/$exe" ./cmd/enginex-installer
	CGO_ENABLED=0 GOOS=$goos GOARCH=$arch go build -trimpath -o "$d/stems/$stemsd" ./cmd/enginex-stemsd
	cp tools/az01-packer/az01_packer.py tools/az01-packer/host-install-userdata.py tools/az01-packer/host-install-fastboot.sh "$d/"
	cp -R third_party/installer "$d/third_party/"
	cp tools/stems/worker.py tools/stems/run-home-worker.sh tools/stems/run-home-worker.ps1 tools/stems/README.md "$d/stems/"
	chmod 644 "$d/az01_packer.py" "$d/stems/worker.py" "$d/stems/run-home-worker.ps1" "$d/stems/README.md"
	chmod 755 "$d/$exe" "$d/host-install-userdata.py" "$d/host-install-fastboot.sh" "$d/stems/$stemsd" "$d/stems/run-home-worker.sh"
	# Every launcher passes its arguments on (--uninstall, --image, --key...: BH-G2-8).
	case $os in
	linux)
		printf '#!/bin/sh\ncd "$(dirname "$0")"\nexec ./enginex-installer "$@"\n' >"$d/install-enginex.sh"
		printf '#!/bin/sh\ncd "$(dirname "$0")"\nexec ./enginex-installer --restore-stock "$@"\n' >"$d/Restore-Official-Firmware.sh"
		chmod 755 "$d/install-enginex.sh" "$d/Restore-Official-Firmware.sh"
		tar --owner=0 --group=0 -C "$d" -czf "$OUT/EngineX-Installer-$V-$t.tar.gz" .
		continue
		;;
	macos)
		printf '#!/bin/sh\ncd "$(dirname "$0")"\n./enginex-installer "$@"\nprintf "Press Enter to close..."\nread answer\n' >"$d/Install-EngineX.command"
		printf '#!/bin/sh\ncd "$(dirname "$0")"\n./enginex-installer --restore-stock "$@"\nprintf "Press Enter to close..."\nread answer\n' >"$d/Restore-Official-Firmware.command"
		chmod 755 "$d/Install-EngineX.command" "$d/Restore-Official-Firmware.command"
		cp tools/stems/run-home-worker.command "$d/stems/"
		chmod 755 "$d/stems/run-home-worker.command"
		;;
	windows)
		printf '@echo off\r\ncd /d "%%~dp0"\r\nenginex-installer.exe %%*\r\npause\r\n' >"$d/Install-EngineX.bat"
		printf '@echo off\r\ncd /d "%%~dp0"\r\nenginex-installer.exe --restore-stock %%*\r\npause\r\n' >"$d/Restore-Official-Firmware.bat"
		chmod 644 "$d/Install-EngineX.bat" "$d/Restore-Official-Firmware.bat"
		;;
	esac
	python3 -I - "$d" "$OUT/EngineX-Installer-$V-$t.zip" <<'EOF'
import os, sys, zipfile
src, dst = sys.argv[1:]
with zipfile.ZipFile(dst, "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk(src):
        dirs.sort()
        for name in sorted(files):
            full = os.path.join(root, name)
            rel = os.path.relpath(full, src).replace(os.sep, "/")
            info = zipfile.ZipInfo.from_file(full, rel)  # keeps the mode bits
            info.compress_type = zipfile.ZIP_DEFLATED
            with open(full, "rb") as f:
                z.writestr(info, f.read())
EOF
done
(
	cd "$OUT"
	set --
	for f in *-Update.img *-Data-Bundle.tar.gz EngineX-Installer-"$V"-* RELEASE-TAG.txt; do
		[ -e "$f" ] && set -- "$@" "$f"
	done
	sha256sum "$@" > SHA256SUMS.txt
	# This is the list that is published and signed (sign-release.sh, #239); an older signature is for older bytes.
	rm -f SHA256SUMS.txt.sig
)
ls -l "$OUT"/EngineX-Installer-"$V"-*
