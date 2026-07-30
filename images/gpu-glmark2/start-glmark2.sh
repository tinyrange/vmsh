#!/bin/sh
set -eu

mkdir -p /run/user/1000 /tmp/.X11-unix
chmod 1777 /tmp/.X11-unix
rm -f /tmp/.X11-unix/X0

# SquadVM currently uses the X11 socket as its desktop-ready signal. glmark2
# renders directly through KMS/GBM, so this socket is readiness-only.
socat UNIX-LISTEN:/tmp/.X11-unix/X0,fork EXEC:/bin/cat &
readiness_pid=$!
trap 'kill "$readiness_pid" 2>/dev/null || true' EXIT INT TERM

touch /run/user/1000/squadvm-desktop-ready

# Run glmark2's complete built-in suite. The persistent log makes scene
# completion and renderer failures observable from the host through /shared
# without changing the guest workload.
exec glmark2-es2-drm \
    --annotate \
    --run-forever \
    --visual-config alpha=0 \
    >/shared/glmark2-full.log 2>&1
