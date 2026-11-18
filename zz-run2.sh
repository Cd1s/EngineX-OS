# Throwaway: light tests, then the sandbox arms, then the wider test classes. Runs under the heavy lock.
set -u
. /etc/profile.d/buildtools.sh
REPO=$PWD; S=$REPO/tools/az01-packer
export ENGINEX_MIGRATION=$REPO/rootfs-overlay/usr/libexec/enginex-migration
python3 -c "import ast;ast.parse(open('tools/ui-mod/build_packs.py').read())" || exit 1
( cd "$S" && python3 -B -m unittest test_ui_perf 2>&1 | tail -25; python3 -B -m unittest -k local_countdown -k cue -k Cue test_ui_mod 2>&1 | tail -45 ) > /tmp/cuefable-light.txt 2>&1
cat /tmp/cuefable-light.txt
if grep -qE '^(FAILED|ERROR)' /tmp/cuefable-light.txt; then echo ZZ-LIGHT-FAILED; echo ZZ-RUN-END; exit 1; fi
echo ZZ-LIGHT-OK
sed -e 's#"\$REPO/tools/ui-mod/build_packs.py"#"$CUE_BP"#' -e 's#"\$REPO/tools/az01-packer/cue-countdown-probe.qml"#"$CUE_PROBE"#' \
    -e '/> "\$R\/data\/enginex\/ui\/settings.conf"/a printf "%s" "${CUE_CONF:-}" >> "$R/data/enginex/ui/settings.conf"' "$S/cue-countdown-sandbox.sh" > "$S/zz-sbx.sh"
sed 's/@OFF@/false/' "$S/zz-cue-measure.qml" > "$S/zz-on.qml"; sed 's/@OFF@/true/' "$S/zz-cue-measure.qml" > "$S/zz-off.qml"
for arm in ${ARMS:-fable probe off sonnet old fable2}; do
    W=/var/tmp/cuefable-$arm; bp=$REPO/tools/ui-mod/build_packs.py; probe=$S/zz-on.qml; conf=; to=150
    case $arm in old*) bp=$REPO/tools/ui-mod/zz_bp_old.py ;; sonnet*) bp=$REPO/tools/ui-mod/zz_bp_sonnet.py ;; off*) probe=$S/zz-off.qml; conf=$'cue_countdown=0\n' ;; probe*) probe=$S/cue-countdown-probe.qml; to=300 ;; esac
    [ -z "$(findmnt -R "$W" 2>/dev/null)" ] && rm -rf "$W"
    echo "ARM $arm $(date +%T)"
    CUE_BP=$bp CUE_PROBE=$probe CUE_CONF=$conf CUE_COUNTDOWN_TIMEOUT=$to bash "$S/zz-sbx.sh" "$W" JP07 2>&1 | sed "s/^/$arm: /"
    grep -a -E "qml|QML|TypeError|ReferenceError|SyntaxError" "$W/JP07-engine.log" 2>/dev/null | grep -a -i -E "scCue|EngineXServices|Error" | head -8 | cut -c1-220 | sed "s/^/$arm-log: /"
    [ -z "$(findmnt -R "$W" 2>/dev/null)" ] && rm -rf "$W"
done
echo ZZ-ARMS-END
( cd "$S" && python3 -B -m unittest test_ui_mod.ShadowDisplayTest test_ui_perf 2>&1 | tail -30; python3 -B -m unittest -k whole_bpm -k WholeBpm -k Bpm test_ui_mod 2>&1 | tail -8 )
echo ZZ-RUN-END
