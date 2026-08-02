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
        http://127.0.0.1:8080/ \
        >/shared/firefox-webgl-stdout.log 2>&1 </dev/null &
firefox_pid=$!

wait "$firefox_pid"
