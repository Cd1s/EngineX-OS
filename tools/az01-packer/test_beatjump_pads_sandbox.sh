#!/bin/bash
# The four players' Beat Jump pad mode (Shift + LOOP: pad 1 the stock JumpBack, pad 2 the stock JumpForward, pads 3-8
# nothing and unlit) through the pack's real controller-side QML, in each board's own stock Engine natively on the build
# host: beatjump-pads-probe.qml instantiates the generated GlobalAssignmentConfig, PadModeSelect, ActionPads and its RGB
# pad LEDs as the players' assignment files do and drives them through ValueNoteAssignment.onNote, as MIDI notes do.
# It checks entry, the two jumps on the real /Engine/Deck<N>/Track/BeatJump properties, pads 3-8 and Shift + pad,
# a pad held into the mode, leaving by any unshifted pad mode button (its stock action still runs), padsEnabled, the
# LEDs, Layer B / 4-deck decks keeping their own mode, and the setting from the main UI. On every other board
# Shift + LOOP and the pads stay stock.
#   bash tools/az01-packer/test_beatjump_pads_sandbox.sh <new-private-dir> [BOARD ...]
#   (default: JP07 JP08 JP13 JP14, and JP11 JC11 NH08 JP21 as all-in-ones)
# Same sandbox as test_controller_qml_compile.sh (CQC_BUILDER works here too); run it in your own mount namespace
# under the build host's heavy-job lock.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
W=${1:?new private sandbox directory required}
shift
boards=("$@")
[ ${#boards[@]} -gt 0 ] || boards=(JP07 JP08 JP13 JP14 JP11 JC11 NH08 JP21)
# Players run twice: the stock 2-deck layout and the 4-deck layout (Paired Player is the same layout).
exec env CQC_PROBE="$REPO/tools/az01-packer/beatjump-pads-probe.qml" CQC_TAG=BJT CQC_ARMS="pack fourdeck" \
    bash "$REPO/tools/az01-packer/test_controller_qml_compile.sh" "$W" "${boards[@]}"
