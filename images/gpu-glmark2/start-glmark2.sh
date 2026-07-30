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

# Keep this corpus explicit and deterministic. Add scenes only after the owned
# decoder passes the existing list without falling back to software rendering.
exec glmark2-es2-drm \
    --annotate \
    --run-forever \
    --visual-config alpha=0 \
    --benchmark build:use-vbo=true \
    --benchmark build:use-vbo=false \
    --benchmark texture:texture-filter=linear \
    --benchmark shading:shading=gouraud \
    --benchmark desktop:effect=blur:windows=4
