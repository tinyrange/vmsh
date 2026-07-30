# Guest glmark2 image

This ARM64 SquadVM guest is the compatibility gate after the first accelerated
KMS cube. It runs a fixed `glmark2-es2-drm` scene corpus through Mesa VirGL,
cc's owned decoder and TGSI compiler, the Darwin OpenGL backend, and Glass.

The run passes only when the guest reports `virgl`, every listed scene
completes, and the service remains active without Mesa falling back to a
software renderer. Build the archive on `astra-pi5`; keep all local runtime,
storage, and cache state under `build/`.

The image patches glmark2's DRM mode selection to honor the connector's
preferred host scanout. Upstream otherwise chooses the largest synthetic
fallback mode exposed by Linux DRM, which is not a mode SquadVM requested.
