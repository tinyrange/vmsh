# OpenArena desktop OpenGL guest

This dedicated ARM64 fixture runs stock OpenArena 0.8.8 as an ordinary GLX
application through Xorg's modesetting driver. It is the compatibility and
performance gate after the complete `glmark2-es2-drm` corpus.

The service refuses to start the game unless `glxinfo -B` identifies the
renderer as `virgl`. At image-build time it derives a deterministic
1,200-message benchmark from the bundled protocol-71 `demo088-test1`; no game
data is copied into this repository. The slice includes complete renderer and
map initialization, model and texture loading, and sustained bot combat, while
ending before the original demo's pathologically slow final segment.

The timedemo runs at 1440x900, uncapped and with vertical synchronization
disabled. OpenArena's `nextdemo` setting repeats the same workload so the native
Glass frontend remains useful for visual inspection and repeated performance
sampling. FPS, timer, and snapshot diagnostics remain visible; the network
lagometer is disabled because its red, blue, and green graph resembles a
corrupted texture in showcase captures.

All observable output is exported through the guest `/shared` mount:

- `openarena-glxinfo.log`: GLX vendor, renderer, and version selection.
- `openarena-console.log`: engine initialization and timedemo results.
- `openarena-stdout.log`: launcher and engine standard output.
- `openarena-Xorg.log`: modesetting, DRI, and GLX initialization.
- `openarena-startup-error.log`: a concise failure when Xorg or VirGL is absent.

A reference Darwin/ARM64 development run initially completed the 1440x900
workload with:

```text
1201 frames 79.5 seconds 15.1 fps 16.0/66.2/122.0/16.7 ms
```

After batching guest buffer transfers, retaining unchanged framebuffer
attachments, and avoiding redundant host program binds, the same M4 reference
host completes it with:

```text
1201 frames 54.5 seconds 22.0 fps 10.0/45.4/94.0/11.6 ms
```

Treat these numbers as profiling baselines, not correctness thresholds. The
behavioral gate is that Mesa selects VirGL, the real game completes the slice,
and the console emits a timedemo result without a renderer rejection.

Build the Docker archive natively on `astra-pi5`. Run it with a unique VM name
and keep the SquadVM cache, shared storage, archive, and captures beneath the
repository's `build/` directory. `LIBGL_ALWAYS_SOFTWARE=1` is reserved for a
separate comparison image or explicit benchmark run; the accelerated fixture
never silently falls back to llvmpipe.
