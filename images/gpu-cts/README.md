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

The pinned CTS release is built with `cts-gl41-harness.patch`. The patch only
guards test-harness use of APIs newer than the requested context: it does not
waive cases, change expected rendering results, or override the reported GL
version. Its SHA-256 is recorded in `manifest.json` and the patch itself is
stored in the image beside the CTS revision.

For an isolated manually driven suite, create `.vmsh-gpu-cts-manual` at the
root of the VM's shared storage before boot. The systemd unit then remains
inactive, allowing one authoritative Xorg/CTS process to start without first
creating and killing the automatic runner's GL context. Normal fixture boots
remain unchanged when the sentinel is absent.

With that sentinel present, `/usr/local/bin/run-gl41-cts` runs the complete
exported `KHR-GL41.*` case set into `/shared/gpu-cts/gl41` by default. It uses a
fresh CTS process for each top-level group, finer shards for
`texture_swizzle`, and per-format shards for `packed_pixels`. The upstream
process otherwise retains enough memory to exceed an 8 GiB guest, while Mesa
26's packed-pixel variant accumulation can wedge Apple's native GL queue. The
runner is resumable at completed shards and writes `summary.json`, exact
expected/observed case lists, and missing/extra lists; success requires
complete QPA sessions, zero failures, and exact case-set equality. An alternate
result directory may be supplied as its sole argument.

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
    -memory-mb 8192 \
    "docker-archive:$PWD/build/gpu-cts/vmsh-gpu-cts.docker.tar"
```

Use 8192 MiB for comparison with the GL 4.1 baseline, but do not substitute
memory for sharding: monolithic runs were OOM-killed with both 6 GiB and 8 GiB
guests.
