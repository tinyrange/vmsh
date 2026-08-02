#!/bin/sh
set -eu

mkdir -p /run/vmsh-gpu-cube
mkdir -p /run/user/1000 /tmp/.X11-unix
chmod 1777 /tmp/.X11-unix
rm -f /tmp/.X11-unix/X0

# SquadVM's normal desktop handshake waits for an X11 socket. This image uses
# DRM/KMS directly, so keep a harmless local socket open solely to satisfy that
# existing session-readiness contract.
socat UNIX-LISTEN:/tmp/.X11-unix/X0,fork EXEC:/bin/cat &
readiness_pid=$!
trap 'kill "$readiness_pid" 2>/dev/null || true' EXIT INT TERM

touch /run/user/1000/squadvm-desktop-ready

# kmscube treats a readable stdin as an interactive request to stop. A
# systemd service normally gets /dev/null, which is permanently readable, so
# give it an idle pipe kept open by fd 3 instead.
rm -f /run/vmsh-gpu-cube/stdin
mkfifo /run/vmsh-gpu-cube/stdin
exec 3<>/run/vmsh-gpu-cube/stdin
exec kmscube </run/vmsh-gpu-cube/stdin 2>&1
