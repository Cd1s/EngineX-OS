#!/bin/bash
# #207, build host only: Engine's own Reverb in the PRIME 4 sandbox, with the hook's decay patch
# and with the original instructions (test_reverb_native.c). Give FX_HOOK=<built hook> to test
# one that is not the repository's.
set -euo pipefail
cd "$(dirname "$0")/../.."
FX_DRIVER=$PWD/tools/az01-packer/test_reverb_native.c FX_DRIVER_TAG=FXREVERB FX_TIMEOUT=${FX_TIMEOUT:-260} \
    exec bash tools/az01-packer/test_fx_selector_sandbox.sh
