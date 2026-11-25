# Loader audit of the release gate, shared with its test (bash, sourced; root, for chroot).
# Review of the SHOUTcast mode, finding 2: the gate ran `ld.so --list` only on files of the committed
# overlay, so a program the build adds to the image (enginex-aacenc) or to the second-generation
# bundles was never checked, and `ld.so --list` never looks at a program's own interpreter.

# elf_loader_audit <rootfs> <loader> <path inside the rootfs>
#   0: the file loads: its program interpreter (PT_INTERP) is in the rootfs and that interpreter
#      resolves every library and symbol version it needs (<loader> stands in for shared libraries,
#      which have no PT_INTERP);
#   1: it does not; the reason is on stdout;
#   2: not a dynamic ELF (or not there): nothing to check.
#   A program linked for another loader (soft-float /lib/ld-linux.so.3, musl) passed
#   `ld.so --list` and would not start on the deck, so the interpreter is checked first.
elf_loader_audit() {
    local root=$1 loader=$2 f=$3 interp out
    readelf -h "$root$f" >/dev/null 2>&1 || return 2
    readelf -l "$root$f" 2>/dev/null | grep -q 'INTERP\|DYN' || return 2
    interp=$(readelf -l "$root$f" 2>/dev/null | sed -n 's/.*interpreter: \(.*\)]/\1/p')
    if [ -n "$interp" ]; then
        # Resolved inside the rootfs: its loader is often an absolute symlink.
        if ! chroot "$root" /bin/sh -c '[ -f "$1" ]' sh "$interp"; then
            echo "$f: program interpreter $interp is not in the rootfs"
            return 1
        fi
    else
        interp=$loader
    fi
    if ! out=$(chroot "$root" "$interp" --list "$f" 2>&1) || grep -q 'not found' <<< "$out"; then
        printf '%s\n' "$out"
        return 1
    fi
    return 0
}

# gen2_bundle_members <dir> <version> <member> <work dir>
#   Every second-generation image of <version> in <dir> (*-Update.img that starts with the 128-byte
#   ENGINEX_DATA_BUNDLE_IMG_V1 header, then the bundle's tar.gz; first-generation AZ01 images are
#   skipped) gives one line "<PREFIX> <path of <member> extracted under <work dir>>", the path left
#   empty when the bundle lacks <member>. Nonzero when an image cannot be read.
gen2_bundle_members() {
    local dir=$1 version=$2 member=$3 work=$4 img prefix top out
    for img in "$dir"/*-"$version"-EngineX-OS-*-Update.img; do
        [ -f "$img" ] || continue
        [ "$(head -c 27 "$img" | tr -d '\0')" = "ENGINEX_DATA_BUNDLE_IMG_V1" ] || continue
        prefix=$(basename "$img"); prefix=${prefix%%-*}
        top=$prefix-$version-EngineX-OS-Data-Bundle
        out=$work/$prefix
        rm -rf "$out"
        mkdir -p "$out"
        tail -c +129 "$img" | tar -xzf - -C "$out" "$top/$member" 2>/dev/null || :
        if [ -f "$out/$top/$member" ]; then
            echo "$prefix $out/$top/$member"
        else
            tail -c +129 "$img" | tar -tzf - >/dev/null || return 1  # unreadable, not just without <member>
            echo "$prefix "
        fi
    done
}

# gen2_aacenc_audit: the release gate's check of the second-generation images of the same build,
#   which sit next to the new image (a single-group build has none). Their bundles' enginex-aacenc
#   must load, and open the AAC encoder, on the stock rootfs each installs on: SC LIVE 4's for the
#   six armhf models ($GATE_GEN2_ROOTFS), SYSTEM ONE's for AZ04 ($GATE_AZ04_ROOTFS). Uses the gate's
#   $W, $NEW_IMG, $NEWV and its pass / fail / note / check; mounts, so only in a private mount
#   namespace. The stock rootfs is bound read-only, with a tmpfs on its /tmp for the bundle's copy.
gen2_aacenc_audit() {
    local list prefix enc root loader rc
    if ! list=$(gen2_bundle_members "$(dirname "$NEW_IMG")" "$NEWV" bin/enginex-aacenc "$W/gen2-members"); then
        fail "gen2: a second-generation image of $NEWV next to the new image cannot be read"; return
    fi
    [ -n "$list" ] || { note "gen2: no second-generation image of $NEWV next to the new image; bundle encoder audit skipped"; return; }
    mkdir -p "$W/gen2-root"
    while read -r prefix enc; do
        case $prefix in
            PRIME4G2|SYSTEMONE) root=$GATE_AZ04_ROOTFS loader=/lib/ld-linux-aarch64.so.1 ;;
            *) root=$GATE_GEN2_ROOTFS loader=/lib/ld-linux-armhf.so.3 ;;
        esac
        [ -n "$enc" ] || { fail "gen2 $prefix: the bundle has no bin/enginex-aacenc"; continue; }
        # /tmp must be a real directory there: a symlink would be resolved on the host by mount.
        [ -d "$root/tmp" ] && [ ! -L "$root/tmp" ] ||
            { fail "gen2 $prefix: no stock rootfs at $root (GATE_GEN2_ROOTFS / GATE_AZ04_ROOTFS)"; continue; }
        if ! { mount --bind "$root" "$W/gen2-root" && mount -o remount,bind,ro "$W/gen2-root" && mount -t tmpfs tmpfs "$W/gen2-root/tmp"; }; then
            umount -R "$W/gen2-root" 2>/dev/null || :
            fail "gen2 $prefix: cannot mount $root"; continue
        fi
        install -D -m 0755 "$enc" "$W/gen2-root/tmp/gate/enginex-aacenc"
        rc=0; elf_loader_audit "$W/gen2-root" "$loader" /tmp/gate/enginex-aacenc < /dev/null > "$W/ldd.txt" 2>&1 || rc=$?
        if [ "$rc" = 0 ]; then pass "gen2 $prefix: libraries of bin/enginex-aacenc"
        else fail "gen2 $prefix: libraries of bin/enginex-aacenc"; cat "$W/ldd.txt"; fi
        check "gen2 $prefix: bin/enginex-aacenc opens the stock AAC encoder" chroot "$W/gen2-root" /tmp/gate/enginex-aacenc --check < /dev/null
        umount -R "$W/gen2-root" || fail "gen2 $prefix: cannot unmount $W/gen2-root"
    done <<< "$list"
}
