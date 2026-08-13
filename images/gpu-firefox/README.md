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

The same image also contains a pinned copy of the official Khronos WebGL
conformance suite. Before boot, write either `webgl1` or `webgl2` to
`/shared/.vmsh-webgl-cts` to select it instead of the demo. The suite starts
automatically and posts its complete text report to one of:

- `/shared/firefox-webgl-cts-webgl1.txt`;
- `/shared/firefox-webgl-cts-webgl2.txt`.

The matching `.status` file contains `running` or `complete`. Run WebGL 1 and
WebGL 2 in separate fresh VM sessions so browser and driver state cannot leak
between results. Failed page records include their individual assertion
messages so renderer mismatches can be diagnosed without an interactive
browser session. Without the sentinel, the original four-scene workload is
unchanged.

For failure isolation, `.vmsh-webgl-cts-include` and
`.vmsh-webgl-cts-skip` may each contain one comma-separated list of suite URL
regular expressions. If Firefox exits before posting a report, the status file
records `browser-exited:<status>` and the Xorg desktop remains alive for
inspection instead of closing the VM window.

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
