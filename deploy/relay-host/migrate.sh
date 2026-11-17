#!/bin/sh
# Read the pre-5.3.0.5 backend names only during an operator-run upgrade.
relay_previous_units='sc5000-deemix-relay.service sc5000-relay.service enginex-deemix-relay.service'
relay_previous_commands='sc5000-relay sc5000-deemix-relay'
relay_previous_state=/var/lib/sc5000-relay
stems_previous_unit=sc5000-stemsd.service

migrate_stems() {
    for pair in /etc/sc5000-stems:/etc/enginex-stems /opt/sc5000-stems:/opt/enginex-stems; do
        old=${pair%%:*}; new=${pair#*:}
        if [ -d "$old" ] && [ ! -L "$old" ] && { [ -e "$new" ] || [ -L "$new" ]; }; then
            echo "Both old and new renderer paths exist: $old, $new" >&2
            return 1
        fi
    done
    if [ -f "/etc/systemd/system/$stems_previous_unit" ] && [ ! -L "/etc/systemd/system/$stems_previous_unit" ]; then
        systemctl disable --now "$stems_previous_unit"
    fi
    for pair in /etc/sc5000-stems:/etc/enginex-stems /opt/sc5000-stems:/opt/enginex-stems; do
        old=${pair%%:*}; new=${pair#*:}
        if [ -d "$old" ] && [ ! -L "$old" ]; then
            mv "$old" "$new"
            ln -s "$new" "$old"
        fi
    done
    ln -sfn enginex-stemsd.service "/etc/systemd/system/$stems_previous_unit"
}

installed_relay_unit() {
    for name in enginex-relay.service $relay_previous_units; do
        if [ -f "/etc/systemd/system/$name" ]; then
            printf '%s\n' "/etc/systemd/system/$name"
            return
        fi
    done
    return 0
}

migrate_relay() {
    current=/var/lib/enginex-relay
    if [ -e "$relay_previous_state" ] && [ "$(readlink -f "$relay_previous_state")" != "$(readlink -f "$current")" ]; then
        # DynamicUser keeps the real state under /var/lib/private.
        source=$(readlink -f "$relay_previous_state")
        case "$source" in
            "$relay_previous_state"|/var/lib/private/sc5000-relay) ;;
            *) echo 'Refusing an unrelated legacy relay state symlink' >&2; return 1 ;;
        esac
        if [ -e "$current" ] || [ -L "$current" ]; then
            echo 'Both old and new relay state exist; keep a backup and resolve them before upgrading' >&2
            return 1
        fi
    else
        source=
    fi
    for name in $relay_previous_units; do
        previous=/etc/systemd/system/$name
        if [ -f "$previous" ] && [ "$(readlink "$previous" 2>/dev/null || :)" != enginex-relay.service ]; then
            systemctl disable --now "$name"
        fi
        if [ -d "$previous.d" ]; then
            mkdir -p /etc/systemd/system/enginex-relay.service.d
            for file in "$previous.d"/*.conf; do
                [ -f "$file" ] || continue
                dest=/etc/systemd/system/enginex-relay.service.d/${file##*/}
                if [ -e "$dest" ] && ! cmp -s "$file" "$dest"; then
                    echo "Conflicting relay override: $dest" >&2
                    return 1
                fi
                mv "$file" "$dest"
            done
            rmdir "$previous.d"
        fi
    done
    if [ -n "$source" ]; then
        mv "$source" "$current"
        [ ! -L "$relay_previous_state" ] || rm "$relay_previous_state"
        ln -s enginex-relay "$relay_previous_state"
    fi
}

relay_compatibility_aliases() {
    for name in $relay_previous_units; do
        ln -sfn enginex-relay.service "/etc/systemd/system/$name"
    done
    for name in $relay_previous_commands; do
        ln -sfn enginex-relay "/usr/local/bin/$name"
    done
}

rewrite_relay_unit() {
    sed -e 's/sc5000-deemix-relay/enginex-relay/g' \
        -e 's/enginex-deemix-relay/enginex-relay/g' \
        -e 's/sc5000-relay/enginex-relay/g' "$1"
}
