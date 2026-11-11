#!/bin/bash
# Native DSP checks. For this task run through remote-build on the build host.
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
work=${OUT_DIR:-$repo/out/fx-tests}
mkdir -p "$work"
for test in test_pioneer_selector test_dsp_smooth test_fx_ms; do
    for sanitizer in plain asan tsan; do
        flags=(-std=c11 -O1 -g -Wall -Wextra -Werror -pthread)
        case "$sanitizer" in
            asan) flags+=(-fsanitize=address,undefined -fno-sanitize-recover=undefined) ;;
            tsan) flags+=(-fsanitize=thread) ;;
        esac
        gcc "${flags[@]}" "$repo/tools/ui-mod/$test.c" -lm -o "$work/$test-$sanitizer"
        "$work/$test-$sanitizer" > "$work/$test-$sanitizer.log" 2>&1
        echo "$test $sanitizer PASS"
    done
done

arm-linux-gnueabihf-gcc -O2 -Wall -Wextra -Werror -Wl,-z,max-page-size=4096 \
    "$repo/tools/ui-mod/test_flanger_scope.c" -ldl -lpthread -o "$work/test_flanger_scope"
"$work/test_flanger_scope" > "$work/test_flanger_scope.log" 2>&1
echo "test_flanger_scope armhf PASS"
for arch in AZ04 armhf; do
    compiler=gcc; if [ "$arch" = armhf ]; then compiler=arm-linux-gnueabihf-gcc; fi
    "$compiler" -O2 "$repo/tools/ui-mod/test_ms_master_hook.c" -ldl -lpthread -lm -o "$work/test_ms_master_hook-$arch"
    "$work/test_ms_master_hook-$arch" > "$work/test_ms_master_hook-$arch.log" 2>&1
    echo "test_ms_master_hook $arch PASS"
    for contract in fast off; do
        define=(); if [ "$contract" = off ]; then define=(-DGOLDEN_FP_OFF); fi
        "$compiler" -O2 "${define[@]}" -ffp-contract="$contract" "$repo/tools/ui-mod/test_master_golden.c" -ldl -lpthread -o "$work/test_master_golden-$arch-$contract"
        "$work/test_master_golden-$arch-$contract" > "$work/test_master_golden-$arch-$contract.log" 2>&1
        echo "test_master_golden $arch $contract PASS"
        "$compiler" -O2 "${define[@]}" -ffp-contract="$contract" "$repo/tools/ui-mod/test_px_off_golden.c" -lm -o "$work/test_px_off_golden-$arch-$contract"
        "$work/test_px_off_golden-$arch-$contract" > "$work/test_px_off_golden-$arch-$contract.log" 2>&1
        echo "test_px_off_golden $arch $contract PASS"
    done
done
