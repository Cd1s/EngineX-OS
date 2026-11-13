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
# "data" only when it has a size, as enginex-installer decides it (dataPartition). BH2-A7-12:
# any answer with a number counted, also the 0 of a bootloader that has no such partition.
size=$(fastboot getvar partition-size:data 2>&1 | grep -Eo 'partition-size:data:[[:space:]]*(0x[0-9a-fA-F]+|[0-9]+)' | head -n 1 | sed 's/.*:[[:space:]]*//')
if [ -n "$size" ] && [ "$((size))" -gt 0 ]; then
    PART=data
fi

echo "Flashing the $PART partition with $IMAGE (sending over USB may take several minutes, do not disconnect)..."
fastboot flash "$PART" "$IMAGE"

echo "Rebooting device..."
fastboot reboot

echo "Done! The device will boot stock Engine, trigger the ALSA one-shot boot hook, and restart Engine with EngineX OS."
