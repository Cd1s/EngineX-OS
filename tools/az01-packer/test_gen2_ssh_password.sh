#!/bin/bash
# Called only inside test_sclive4_bootstrap.sh's private mount/network/PID namespace.
set -euo pipefail
root=$1 work=$2
[ "$root" = "$work/root" ] || exit 2
work=$(mktemp -d "$work/ssh299-XXXXXX")
umask 077
ip link set lo up
# The suite must see the sandbox deck, not the build host's device tree.
mount -t tmpfs tmpfs "$root/sys"
mkdir -p "$root/sys/firmware/devicetree/base"
printf 'inmusic,jp21\0' > "$root/sys/firmware/devicetree/base/compatible"
ssh-keygen -q -t ed25519 -N '' -f "$work/id_test"
python3 - "$work" <<'PY'
import json, pathlib, secrets, sys
p=pathlib.Path(sys.argv[1])
password=secrets.token_hex(24)
(p/'password').write_text(password+'\n')
(p/'request.json').write_text(json.dumps({'password':password,'port':2222,'enabled':True,'authorizedKey':(p/'id_test.pub').read_text()}))
PY
for _ in {1..50}; do
    if curl -sf http://127.0.0.1:8080/v1/enginex/ssh -o "$work/status.json"; then break; fi
    sleep .1
done
curl -sf -X POST -H 'Content-Type: application/json' --data-binary "@$work/request.json" \
    http://127.0.0.1:8080/v1/enginex/ssh -o "$work/saved.json"
grep -q '"hasPassword":true' "$work/saved.json"
grep -q '^root:\$6\$' "$root/data/enginex/ssh-shadow"
[ "$(stat -c %a "$root/data/enginex/ssh-shadow")" = 600 ]
# chroot has no running systemd. Execute exactly the generated sshd command.
chroot "$root" pkill -x sshd || true
command=$(sed -n 's/^ExecStart=\(\/.*\)/\1/p' "$root/run/systemd/system/sshd.service.d/51-port.conf")
chroot "$root" /bin/sh -c "$command" > "$work/sshd.log" 2>&1 &
printf '[127.0.0.1]:2222 ' > "$work/known_hosts"
chroot "$root" /usr/bin/ssh-keygen -y -f /etc/ssh/ssh_host_ed25519_key >> "$work/known_hosts"
cat > "$work/askpass" <<'SH'
#!/bin/sh
cat "$ISSUE299_PASSWORD_FILE"
SH
chmod 700 "$work/askpass"
ssh_options=(-F /dev/null -p 2222 -o "UserKnownHostsFile=$work/known_hosts" -o StrictHostKeyChecking=yes -o ConnectTimeout=5)
password_login() {
    ISSUE299_PASSWORD_FILE="$work/password" SSH_ASKPASS="$work/askpass" SSH_ASKPASS_REQUIRE=force DISPLAY=:299 \
        ssh "${ssh_options[@]}" -o PreferredAuthentications=password -o PubkeyAuthentication=no \
        -o NumberOfPasswordPrompts=1 root@127.0.0.1 true </dev/null > /dev/null 2> "$work/client.log"
}
for _ in {1..50}; do
    if password_login; then break; fi
    sleep .1
done
password_login
# Public-key login still works beside the opt-in password.
ssh "${ssh_options[@]}" -o BatchMode=yes -o PreferredAuthentications=publickey -i "$work/id_test" \
    root@127.0.0.1 true </dev/null > /dev/null 2> "$work/key-client.log"
# Simulate reboot: drop the live shadow mount, then restore solely from /data.
chroot "$root" pkill -x sshd
while mountpoint -q "$root/etc/shadow"; do umount "$root/etc/shadow"; done
chroot "$root" /bin/sh /data/enginex/bundle/boot-activate.sh > "$work/restore.log" 2>&1
password_login
if touch "$root/etc/ssh299-must-stay-readonly" 2>/dev/null; then
    echo 'FAIL: password setup made /etc writable'; exit 1
fi
printf '  Gen2 SSH password on read-only /etc, key login and boot restore: PASS\n'
