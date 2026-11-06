#!/bin/sh
# Flash modified userdata image to a second-generation deck in fastboot mode (#61).
set -eu

IMAGE="${1:-userdata.img}"

if [ ! -f "$IMAGE" ]; then
    echo "Usage: $0 <userdata.img>" >&2
    exit 1
fi

if ! command -v fastboot >/dev/null 2>&1; then
    echo "Error: fastboot command not found in PATH" >&2
    exit 1
fi

echo "Connecting to the device in fastboot mode..."
fastboot devices

echo "Flashing userdata partition with $IMAGE..."
fastboot flash userdata "$IMAGE"

echo "Rebooting device..."
fastboot reboot

echo "Done! The device will boot stock Engine, trigger the ALSA one-shot boot hook, and restart Engine with EngineX OS."
