#!/bin/bash
# Native DSP checks. For this task run through remote-build on Oracle-Batam-arm.
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
work=${OUT_DIR:-$repo/out/fx-tests}
mkdir -p "$work"
for test in test_pioneer_selector test_dsp_smooth; do
    for sanitizer in plain asan tsan; do
        flags=(-std=c11 -O1 -g -Wall -Wextra -Werror -pthread)
        case "$sanitizer" in
            asan) flags+=(-fsanitize=address,undefined) ;;
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
for contract in fast off; do
    define=(); if [ "$contract" = off ]; then define=(-DGOLDEN_FP_OFF); fi
    gcc -O2 "${define[@]}" -ffp-contract="$contract" "$repo/tools/ui-mod/test_master_golden.c" -ldl -lpthread -o "$work/test_master_golden-$contract"
    "$work/test_master_golden-$contract" > "$work/test_master_golden-$contract.log" 2>&1
    echo "test_master_golden AZ04 $contract PASS"
done
