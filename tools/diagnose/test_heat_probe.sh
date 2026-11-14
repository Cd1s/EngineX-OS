#!/bin/sh
# heat-probe.sh against a fake sysfs whose counters move while the probe sleeps.
# Linux only (the probe reads the real /proc):  sh tools/diagnose/test_heat_probe.sh
set -eu
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
S=$T/sys
c=$S/devices/system/cpu/cpu0
z=$S/class/thermal/thermal_zone0
g=$S/class/devfreq/ffa30000.gpu
pcm=$T/asound/card2/pcm0p/sub0
mkdir -p "$c/cpufreq/stats" "$c/cpuidle/state0" "$z" "$g" "$T/bin" "$pcm"
printf 'rate: 96000 (96000/1)\nperiod_size: 450\nbuffer_size: 900\n' > "$pcm/hw_params"
printf 'state: RUNNING\navail       : 173\navail_max   : 566\n' > "$pcm/status"
echo ondemand > "$c/cpufreq/scaling_governor"
echo 1200000 > "$c/cpufreq/scaling_min_freq"
echo 1200000 > "$c/cpufreq/scaling_cur_freq"
echo 1800000 > "$c/cpufreq/scaling_max_freq"
printf '408000 100\n1200000 500\n' > "$c/cpufreq/stats/time_in_state"
echo WFI > "$c/cpuidle/state0/name"
echo 1000000 > "$c/cpuidle/state0/time"
echo 10 > "$c/cpuidle/state0/usage"
echo soc-thermal > "$z/type"
echo 81500 > "$z/temp"
echo simple_ondemand > "$g/governor"
echo 300000000 > "$g/cur_freq"
# The probe's sleep: one real second, all of it at 1.2 GHz and none at 408 MHz; the clock
# is at 1.608 GHz when it is read, and the stream has underrun and stopped.
real_sleep=$(command -v sleep)
cat > "$T/bin/sleep" <<EOF
#!/bin/sh
"$real_sleep" "\$1"
printf '408000 100\n1200000 600\n' > "$c/cpufreq/stats/time_in_state"
echo 1500000 > "$c/cpuidle/state0/time"
echo 82000 > "$z/temp"
echo 1608000 > "$c/cpufreq/scaling_cur_freq"
printf 'state: XRUN\navail       : 950\navail_max   : 950\n' > "$pcm/status"
EOF
chmod +x "$T/bin/sleep"
HEAT_PROBE_SLEEP=$T/bin/sleep HEAT_PROBE_SYS=$S HEAT_PROBE_ASOUND=$T/asound sh "$here/heat-probe.sh" 1 selftest > "$T/out" 2> "$T/err" || { cat "$T/out" "$T/err"; exit 1; }
fail=0
want() { grep -Eq -- "$1" "$T/out" || { echo "FAIL missing: $1"; fail=1; }; }
want '^cpu0: ondemand 1200000 1200000 1800000'
want '^ffa30000.gpu: simple_ondemand \? 300000000 \?'
want '^thermal_zone0 soc-thermal temp=81500'
want '^t=0 soc-thermal=81500 1200000$'
want '^t=1 soc-thermal=82000 1608000$'
want '^card2/pcm0p/sub0/hw_params: rate: 96000 .*buffer_size: 900'
want '^ *1608000 kHz 100\.0$'
want '^card2/pcm0p/sub0/status 1 1 950$'
want '^cpu0 +1200000 kHz +[0-9]+\.[0-9]$'
want '^cpu0/cpuidle/state0 WFI +[0-9]+\.[0-9] '
want '^context switches/s [0-9]+ +forks/s [0-9]'
want '^cpu0 +[0-9.]+ +[0-9.]+ +[0-9.]+ +[0-9.]+ +[0-9.]+ \| +[0-9.]+ +[0-9.]+$'
want '^ *[0-9]+\.[0-9]+  all threads alive at the end$'
if grep -q '408000 kHz' "$T/out"; then echo 'FAIL a clock that was never used is listed'; fail=1; fi
if [ -s "$T/err" ]; then echo 'FAIL the probe wrote to stderr:'; cat "$T/err"; fail=1; fi
[ "$fail" = 0 ] || { cat "$T/out"; exit 1; }
echo 'PASS heat-probe self-test'
