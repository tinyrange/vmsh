#!/bin/sh
set -eu

user=webgl
uid=1000
gid=1000
home=/home/webgl
display=:0
runtime_dir="/run/user/$uid"
profile="$home/.mozilla/firefox-vmsh"

install -d -o "$uid" -g "$gid" -m 0700 "$runtime_dir" "$profile"
install -d -m 0755 /run/dbus
install -d -o root -g root -m 1777 /tmp/.ICE-unix /tmp/.X11-unix
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
rm -f \
    /shared/firefox-webgl-startup-error.log \
    /shared/firefox-webgl-telemetry.json \
    /shared/firefox-webgl-telemetry.json.new

firefox_url=http://127.0.0.1:8080/
if [ -f /shared/.vmsh-webgl-cts ]; then
    cts_mode=$(sed -n '1{s/[[:space:]]//g;p;}' /shared/.vmsh-webgl-cts)
    case "$cts_mode" in
        1|webgl1)
            cts_name=webgl1
            cts_version=1.0.4
            ;;
        2|webgl2)
            cts_name=webgl2
            cts_version=2.0.1
            ;;
        *)
            echo "Firefox WebGL fixture: .vmsh-webgl-cts must contain webgl1 or webgl2" \
                | tee /shared/firefox-webgl-startup-error.log >&2
            exit 1
            ;;
    esac
    rm -f \
        "/shared/firefox-webgl-cts-$cts_name.status" \
        "/shared/firefox-webgl-cts-$cts_name.status.new" \
        "/shared/firefox-webgl-cts-$cts_name.txt" \
        "/shared/firefox-webgl-cts-$cts_name.txt.new"
    firefox_url="http://127.0.0.1:8080/cts/webgl-conformance-tests.html?version=$cts_version&run=1&postResults=1&quiet=1"

    append_cts_filter() {
        filter_name=$1
        filter_path=$2
        if [ ! -s "$filter_path" ]; then
            return
        fi
        filter_value=$(sed -n '1{s/[[:space:]]//g;p;}' "$filter_path")
        case "$filter_value" in
            *[!A-Za-z0-9_./,-]*)
                echo "Firefox WebGL fixture: $filter_path contains unsupported URL characters" \
                    | tee /shared/firefox-webgl-startup-error.log >&2
                exit 1
                ;;
        esac
        firefox_url="$firefox_url&$filter_name=$filter_value"
    }
    append_cts_filter include /shared/.vmsh-webgl-cts-include
    append_cts_filter skip /shared/.vmsh-webgl-cts-skip
fi

setsid /usr/bin/dbus-daemon --system --nofork --nopidfile </dev/null \
    >/shared/firefox-webgl-dbus.log 2>&1 &
dbus_pid=$!
setsid /usr/lib/xorg/Xorg "$display" \
    -noreset \
    -nolisten tcp \
    -novtswitch \
    -logfile /shared/firefox-webgl-Xorg.log </dev/null &
xorg_pid=$!
window_manager_pid=
server_pid=
firefox_pid=

cleanup() {
    for cleanup_pid in "$firefox_pid" "$server_pid" "$window_manager_pid" "$xorg_pid" "$dbus_pid"; do
        if [ -n "$cleanup_pid" ]; then
            kill "$cleanup_pid" 2>/dev/null || true
        fi
    done
}
trap cleanup EXIT TERM
trap : INT HUP

attempt=0
while [ ! -S /tmp/.X11-unix/X0 ]; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 400 ]; then
        echo "Firefox WebGL fixture: Xorg did not create display :0" \
            | tee /shared/firefox-webgl-startup-error.log >&2
        exit 1
    fi
    sleep 0.05
done

guest_env="DISPLAY=$display HOME=$home USER=$user XDG_RUNTIME_DIR=$runtime_dir"
setpriv --reuid="$uid" --regid="$gid" --init-groups env $guest_env \
    glxinfo -B > /shared/firefox-webgl-glxinfo.log 2>&1

if ! grep -Eq '^OpenGL renderer string: .*virgl' /shared/firefox-webgl-glxinfo.log; then
    echo "Firefox WebGL fixture: Mesa did not select the virgl renderer" \
        | tee /shared/firefox-webgl-startup-error.log >&2
    cat /shared/firefox-webgl-glxinfo.log >&2
    exit 1
fi

setpriv --reuid="$uid" --regid="$gid" --init-groups env $guest_env \
    xset s off -dpms
setsid setpriv --reuid="$uid" --regid="$gid" --init-groups env $guest_env \
    openbox --config-file "$home/.config/openbox/rc.xml" \
    >/shared/firefox-webgl-openbox.log 2>&1 </dev/null &
window_manager_pid=$!

setsid /usr/local/lib/vmsh-webgl/telemetry-server.py \
    >/shared/firefox-webgl-server.log 2>&1 </dev/null &
server_pid=$!

attempt=0
while ! python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8080/", timeout=1).read(1)' 2>/dev/null; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 100 ]; then
        echo "Firefox WebGL fixture: local demo server did not become ready" \
            | tee /shared/firefox-webgl-startup-error.log >&2
        exit 1
    fi
    sleep 0.05
done

touch "$runtime_dir/squadvm-desktop-ready"
chown "$uid:$gid" "$runtime_dir/squadvm-desktop-ready"

setsid setpriv --reuid="$uid" --regid="$gid" --init-groups env \
    $guest_env \
    MOZ_ENABLE_WAYLAND=0 \
    MOZ_X11_EGL=1 \
    firefox-esr \
        --no-remote \
        --new-instance \
        --kiosk \
        --profile "$profile" \
        "$firefox_url" \
        >/shared/firefox-webgl-stdout.log 2>&1 </dev/null &
firefox_pid=$!

if wait "$firefox_pid"; then
    firefox_status=0
else
    firefox_status=$?
fi
if [ -n "${cts_name:-}" ] && [ ! -f "/shared/firefox-webgl-cts-$cts_name.txt" ]; then
    {
        echo "firefox_status=$firefox_status"
        echo
        free -h
        echo
        cat /proc/meminfo
        echo
        ps aux --sort=-rss
    } > "/shared/firefox-webgl-cts-$cts_name-system.log" 2>&1 || true
    dmesg > "/shared/firefox-webgl-cts-$cts_name-dmesg.log" 2>&1 || true
    journalctl --boot --no-pager \
        > "/shared/firefox-webgl-cts-$cts_name-journal.log" 2>&1 || true
    printf 'browser-exited:%s\n' "$firefox_status" \
        > "/shared/firefox-webgl-cts-$cts_name.status"
    while :; do
        sleep 3600
    done
fi
exit "$firefox_status"
