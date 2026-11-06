#!/bin/sh
# Flash a modified /data image to a second-generation deck in fastboot mode (#61, #88).
# Decks name the partition "data" or "userdata"; ask which one has a size.
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

PART=userdata
if fastboot getvar partition-size:data 2>&1 | grep -Eq 'partition-size:data:[[:space:]]*(0x[0-9a-fA-F]+|[0-9]+)'; then
    PART=data
fi

echo "Flashing the $PART partition with $IMAGE..."
fastboot flash "$PART" "$IMAGE"

echo "Rebooting device..."
fastboot reboot

echo "Done! The device will boot stock Engine, trigger the ALSA one-shot boot hook, and restart Engine with EngineX OS."
