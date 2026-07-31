#!/bin/sh
set -eu

user=arena
uid=1000
gid=1000
home=/home/arena
display=:0
runtime_dir="/run/user/$uid"
ready_file="$runtime_dir/squadvm-desktop-ready"

install -d -o "$uid" -g "$gid" -m 0700 "$runtime_dir"
install -d -m 0755 /run/dbus
install -d -o root -g root -m 1777 /tmp/.ICE-unix /tmp/.X11-unix
install -d -o "$uid" -g "$gid" -m 0700 "$home/.openarena/baseoa"
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0 "$ready_file"
rm -f /shared/openarena-console.log
rm -f "$home/.openarena/baseoa/qconsole.log"
ln -s /shared/openarena-console.log "$home/.openarena/baseoa/qconsole.log"
chown -h "$uid:$gid" "$home/.openarena/baseoa/qconsole.log"

setsid /usr/bin/dbus-daemon --system --nofork --nopidfile </dev/null \
    >/shared/openarena-dbus.log 2>&1 &
dbus_pid=$!
setsid /usr/lib/xorg/Xorg "$display" \
    -noreset \
    -nolisten tcp \
    -novtswitch \
    -logfile /shared/openarena-Xorg.log </dev/null &
xorg_pid=$!
game_pid=

cleanup() {
    if [ -n "$game_pid" ]; then
        kill "$game_pid" 2>/dev/null || true
    fi
    kill "$xorg_pid" 2>/dev/null || true
    kill "$dbus_pid" 2>/dev/null || true
}
trap cleanup EXIT TERM
trap : INT HUP

attempt=0
while [ ! -S /tmp/.X11-unix/X0 ]; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 400 ]; then
        echo "OpenArena fixture: Xorg did not create display :0" \
            | tee /shared/openarena-startup-error.log >&2
        exit 1
    fi
    sleep 0.05
done

setpriv --reuid="$uid" --regid="$gid" --init-groups env \
    DISPLAY="$display" \
    HOME="$home" \
    USER="$user" \
    XDG_RUNTIME_DIR="$runtime_dir" \
    glxinfo -B > /shared/openarena-glxinfo.log 2>&1

if ! grep -Eq '^OpenGL renderer string: .*virgl' /shared/openarena-glxinfo.log; then
    echo "OpenArena fixture: Mesa did not select the virgl renderer" \
        | tee /shared/openarena-startup-error.log >&2
    cat /shared/openarena-glxinfo.log >&2
    exit 1
fi

setpriv --reuid="$uid" --regid="$gid" --init-groups env \
    DISPLAY="$display" \
    HOME="$home" \
    USER="$user" \
    XDG_RUNTIME_DIR="$runtime_dir" \
    xset s off -dpms

touch "$ready_file"
chown "$uid:$gid" "$ready_file"

setsid setpriv --reuid="$uid" --regid="$gid" --init-groups env \
    DISPLAY="$display" \
    HOME="$home" \
    USER="$user" \
    XDG_RUNTIME_DIR="$runtime_dir" \
    stdbuf -oL -eL /usr/games/openarena \
        +set fs_homepath "$home/.openarena" \
        +set com_hunkMegs 512 \
        +set timedemo 1 \
        +demo openarena-benchmark \
        >/shared/openarena-stdout.log 2>&1 </dev/null &
game_pid=$!

wait "$game_pid"
