# Firefox WebGL guest

This dedicated ARM64 fixture runs Firefox ESR as an ordinary X11 desktop
application and continuously cycles four local WebGL 1 workloads through Mesa
VirGL and the owned Darwin renderer:

- indexed, depth-tested and lit cubes;
- offscreen framebuffer rendering with a post-processing pass;
- a dynamic vertex buffer containing additive point particles; and
- a full-screen procedural fragment shader.

The page has no external network or asset dependency. A loopback-only Python
server hosts the files and records structured browser telemetry in
`/shared/firefox-webgl-telemetry.json`. The telemetry includes Firefox's WebGL
vendor, renderer and version strings, canvas size, current scene, frame count,
uptime, completed cycles, visited scenes, and measured frame rate. Each scene
also writes one actual WebGL canvas readback to
`/shared/firefox-webgl-scene-N.png` for deterministic visual inspection. The
launcher separately refuses to continue
unless `glxinfo -B` identifies the Xorg renderer as VirGL.

Other observable files in `/shared` are:

- `firefox-webgl-glxinfo.log`: GLX vendor, renderer, and version selection;
- `firefox-webgl-Xorg.log`: modesetting, DRI, and GLX initialization;
- `firefox-webgl-stdout.log`: Firefox standard output and diagnostics;
- `firefox-webgl-server.log`: local page and telemetry requests;
- `firefox-webgl-openbox.log`: kiosk window-manager diagnostics; and
- `firefox-webgl-startup-error.log`: a concise Xorg, VirGL, or server failure.

Build the Docker archive natively on `astra-pi5`. Launch it with a unique VM
name and keep the SquadVM cache, shared storage, archive, and any VirGL captures
beneath the repository's `build/gpu-firefox/` directory. The correctness gate
is sustained telemetry from all four scenes with a VirGL WebGL renderer and no
host renderer rejection; FPS is diagnostic rather than a hard threshold.
