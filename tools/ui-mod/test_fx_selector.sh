#!/bin/bash
# Native DSP checks. For this task run through remote-build on the build host.
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
