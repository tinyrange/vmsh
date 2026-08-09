# GPU conformance guest

This ARM64 SquadVM fixture turns the owned VirGL renderer into a reproducible
conformance target. It pins Khronos VK-GL-CTS `opengl-cts-4.6.8.1`, launches a
real Xorg/GLX desktop against Mesa VirGL, and writes all results beneath
`/shared/gpu-cts`.

The runtime uses Mesa 26.1.6, the current stable release, as one cohesive
userspace under `/opt/mesa`: EGL, GL, GLES1/GLES2, GBM, and the VirGL DRI driver
are built together. Keeping the loader and driver from the same build avoids
distribution-private DRI symbol ABI mismatches. GLES1 is enabled because the
official ES2 runner exercises EGL context creation for every advertised client
API; those cases run against a real implementation rather than being excluded.
The CTS builder remains separate so changing the guest driver does not also
change the pinned test binaries.

The service first captures `glxinfo` and a machine-readable manifest, runs
quick GLES2, GLES3, GLES3.1, and real desktop context probes from GL 3.0
through GL 4.1, and then executes the complete
dEQP-GLES2 corpus in stable top-level groups. Each group receives its own QPA,
text log, and atomic JSON status file, so stopping and restarting the same
fixture resumes at the next unfinished group. It finishes with Khronos's exact
`cts-runner --type=es2` configuration and preserves the submission-style QPA
and XML artifacts.

`manifest.json` records the exact CTS revision and guest GL identity.
`summary.json` is updated atomically after every group with structured case
status counts and completion progress; it is suitable for host-side monitoring
without making test prose part of the contract.

Each desktop probe runs that profile's <code>KHR-GL*.info.*</code> cases through
EGL, so it must create the requested core context before it can write passing
results. `manifest.json` remains the immediate API-ceiling record. Desktop GL
4.1 becomes a claimed target only after Mesa exposes the version and every
corresponding CTS slice passes.

Build the image natively on `astra-pi5`. Export only the final Docker image,
copy it into `build/gpu-cts`, and launch it with a unique VM name, explicit
repository-local `-cache-dir` and `-storage`, and the native Glass frontend.
Do not reuse another SquadVM home or cache.

Use a single-platform archive without BuildKit provenance. The explicit
platform descriptor is required by cc's archive importer, and the
`docker-archive:` source scheme tells SquadVM to import the archive rather than
treating its path as the name of an image that was already imported:

```sh
docker buildx build \
    --platform linux/arm64 \
    --provenance=false \
    --output type=docker,dest=/home/joshua/vmsh-gpu-cts.docker.tar \
    images/gpu-cts

build/gpu-cts/run/squadvm \
    -name gpu-cts-development \
    -cache-dir "$PWD/build/gpu-cts/cache" \
    -storage "$PWD/build/gpu-cts/shared" \
    -memory-mb 6144 \
    "docker-archive:$PWD/build/gpu-cts/vmsh-gpu-cts.docker.tar"
```

The official runner needs more than the normal 4 GiB SquadVM default. Use at
least 6144 MiB; an interrupted 4 GiB run was killed by the guest OOM path before
it could write complete QPAs.
