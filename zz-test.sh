set -u
cd tools/az01-packer
python3 -B -m unittest test_ui_perf 2>&1 | tail -40
python3 -B -m unittest -k local_countdown -k cue_countdown -k test_cue_info -k shadow_cue -k cue test_ui_mod 2>&1 | tail -60
echo ZZ-TEST-END
