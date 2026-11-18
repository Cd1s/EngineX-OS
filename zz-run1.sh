# Throwaway: baseline arms + Engine property dump. Runs under the heavy lock.
set -u
. /etc/profile.d/buildtools.sh
REPO=$PWD; S=$REPO/tools/az01-packer
export ENGINEX_MIGRATION=$REPO/rootfs-overlay/usr/libexec/enginex-migration
sed -e 's#"\$REPO/tools/ui-mod/build_packs.py"#"$CUE_BP"#' -e 's#"\$REPO/tools/az01-packer/cue-countdown-probe.qml"#"$CUE_PROBE"#' \
    -e '/> "\$R\/data\/enginex\/ui\/settings.conf"/a printf "%s" "${CUE_CONF:-}" >> "$R/data/enginex/ui/settings.conf"' "$S/cue-countdown-sandbox.sh" > "$S/zz-sbx.sh"
grep -c 'CUE_BP\|CUE_PROBE\|CUE_CONF' "$S/zz-sbx.sh"
sed 's/@OFF@/false/' "$S/zz-cue-measure.qml" > "$S/zz-on.qml"; sed 's/@OFF@/true/' "$S/zz-cue-measure.qml" > "$S/zz-off.qml"
for arm in ${ARMS:-old sonnet off}; do
    W=/var/tmp/cuefable-$arm; bp=$REPO/tools/ui-mod/build_packs.py; probe=$S/zz-on.qml; conf=
    case $arm in old*) bp=$REPO/tools/ui-mod/zz_bp_old.py ;; sonnet*) bp=$REPO/tools/ui-mod/zz_bp_sonnet.py ;; off*) probe=$S/zz-off.qml; conf=$'cue_countdown=0\n' ;; esac
    [ -z "$(findmnt -R "$W" 2>/dev/null)" ] && rm -rf "$W"
    echo "ARM $arm $(date +%T)"
    CUE_BP=$bp CUE_PROBE=$probe CUE_CONF=$conf CUE_COUNTDOWN_TIMEOUT=150 bash "$S/zz-sbx.sh" "$W" JP07 2>&1 | sed "s/^/$arm: /"
    grep -a -c "" "$W/JP07-engine.log" 2>/dev/null
    [ -z "$(findmnt -R "$W" 2>/dev/null)" ] && rm -rf "$W"
done
echo ZZ-RUN-END
