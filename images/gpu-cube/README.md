# Guest OpenGL cube image

This image is the end-to-end virtio-gpu acceptance guest. It runs the standard
`kmscube` KMS/GBM/EGL application directly on the guest DRM device. The
application obtains OpenGL ES from Mesa, reports its EGL and GL identities to
the system journal, and continuously page-flips a spinning cube.

The acceptance result is valid only when the renderer identity is VirGL and the
commands have crossed cc's virtio-gpu 3D transport. A software renderer result
such as llvmpipe does not pass.

Build the ARM64 Docker archive on `astra-pi5`, copy it into
`build/gpu-cube`, and run SquadVM with explicit repository-local `-cache-dir`
and `-storage` paths. The Unix socket created by `start-cube.sh` exists only to
satisfy SquadVM's current X11-oriented desktop-ready handshake; rendering does
not use X11.
