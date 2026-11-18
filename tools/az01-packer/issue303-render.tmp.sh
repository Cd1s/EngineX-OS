#!/bin/bash
# Local verification driver: reuse the existing board sandbox and EGLFS setup.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
driver=$here/issue303-derived.tmp.sh
awk -v slot="$here/slot-icon-sandbox.sh" '
 /^    \(cd .*hook-az04.so/ { print "    if [[ \" ${boards[*]} \" == *\" JP24 \"* ]]; then"; print; print "    fi"; next }
 /mount --bind "\$LAB\/\$image\/rootfs"/ {
   print "    mkdir -p \"$W/upper-$board\" \"$W/work-$board\""
   print "    mount -t overlay overlay -o \"lowerdir=$LAB/$image/rootfs,upperdir=$W/upper-$board,workdir=$W/work-$board\" \"$R\""
   next
 }
 /^for n in null zero/ {
    while ((getline line < slot)>0) {
       if (line ~ /^mkdir -p "\$R\/dev\/dri"/) copy=1
       if (line ~ /^for n in null zero/) break
       if (copy) {
         if (line ~ /^mkdir -p "\$R\/usr\/lib\/arm-linux-gnueabihf"/) print "if [ \"$kind\" != az04 ]; then"
         if (line ~ /^printf .*headless/) print "else\nmkdir -p \"$R/usr/lib/aarch64-linux-gnu\" \"$R/usr/share/glvnd\"\nmount --bind /usr/lib/aarch64-linux-gnu \"$R/usr/lib/aarch64-linux-gnu\"\nmount -o remount,bind,ro \"$R/usr/lib/aarch64-linux-gnu\"\nmount --bind /usr/share/glvnd \"$R/usr/share/glvnd\"\nmount -o remount,bind,ro \"$R/usr/share/glvnd\"\nmount --bind /usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1 \"$R/lib/ld-linux-aarch64.so.1\"\nfi"
         print line
       }
    }
 }
 /cc-fit-probe.qml/ {print "cp \"$REPO/tools/az01-packer/crossfader-reverse-probe.qml\" \"$R/data/enginex/ui/packs/en/Skins/Common/Items/QmlWarnings.qml\""; next}
 {gsub(/LD_LIBRARY_PATH=\/usr\/qt\/lib/,"LD_LIBRARY_PATH=/usr/lib/arm-linux-gnueabihf:/usr/lib/aarch64-linux-gnu:/usr/lib:/usr/qt/lib"); gsub(/QT_QPA_PLATFORM=offscreen/,"QT_QPA_PLATFORM=eglfs QT_QPA_EGLFS_INTEGRATION=eglfs_kms LIBGL_ALWAYS_SOFTWARE=1 GALLIUM_DRIVER=llvmpipe LP_NUM_THREADS=2"); gsub(/mx-cc-fit.png/,"crossfader-reverse.png"); gsub(/-cc.png/,"-reverse.png")}
 /grep -aE .*X166/ { print "grep -a \"X303 \" \"$W/$board-engine.log\" | cut -c1-185"; print "grep -aq \"X303 PASS|\" \"$W/$board-engine.log\""; exit }
 {print}
' "$here/cc-fit-sandbox.sh" > "$driver"
exec bash "$driver" "$@"
