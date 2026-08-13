# GPU acceleration progress and handoff

Last updated: 2026-08-12 (Australia/Brisbane)

This document is the operational and engineering handoff for the first-party
virtio-gpu/VirGL acceleration work in vmsh. It is intended for a system that
can keep the work running continuously. Read this document before changing the
GPU code, starting a long conformance run, cleaning build data, or updating the
draft pull request.

The detailed target architecture and milestone definitions remain in
[`GPU_PLAN.html`](GPU_PLAN.html). This report records the actual state of the
code, branches, test artifacts, known failures, and the safest next steps.

## Executive status

The complete guest-to-host accelerated path works on a Darwin/ARM64 host:

```text
Linux ARM64 application
  -> Mesa GL/GLX/EGL/Gallium
  -> Mesa VirGL command stream
  -> virtio-gpu 3D transport in cc
  -> first-party bounded VirGL decoder in cc
  -> TGSI-to-GLSL translation and native NSOpenGL execution
  -> fenced shared textures
  -> native Glass/SquadVM presentation through Gowin
```

This is not a host-side imitation of guest rendering. Ordinary unmodified
guest applications submit OpenGL work through Mesa and virtio-gpu. The host
owns and executes the resulting VirGL protocol without linking virglrenderer or
rutabaga_gfx.

The currently pushed stack is a strong OpenGL 2.0 / GLSL 1.20 / WebGL 1
implementation. It has successfully run:

- a normal guest accelerated spinning cube and kmscube;
- all 33 built-in glmark2 ES2 scenes;
- stock OpenArena 0.8.8 with a deterministic 1,201-frame benchmark;
- Firefox ESR with four distinct WebGL scenes and guest-side pixel readbacks;
- native shared-texture presentation through Glass without routine CPU
  framebuffer readback.

The next active milestone is an honest modern API ladder:

1. GLES 3.0 and desktop GL 3.0/3.1;
2. desktop GL 3.2/3.3;
3. desktop GL 4.0;
4. desktop GL 4.1, which is the ceiling of the Darwin NSOpenGL backend;
5. a future Vulkan-over-Metal renderer for APIs beyond that frozen ceiling.

### Neurodesk application sweep (2026-08-12)

The experimental NeurodeskAppX integration was exercised unattended against
the ARM64 Neurodesktop Glass image and the live CVMFS application catalogue.
The VM used 16 GiB RAM and 8 vCPUs. Every run recorded the transient service
state, process tree, X11 window list, application output, and an automation API
framebuffer capture. The captures came from the guest display session; no host
desktop automation was used.

The following CVMFS applications started, mapped windows, and produced clean
captures while the container environment selected `virpipe`:

- MRView 3.0.8, which explicitly reported `virgl (vmsh Darwin VirGL)`, OpenGL
  4.1 Core, and Mesa 25.0.7;
- ITK-SNAP 4.4.0, FreeView 8.2.0, AFNI 26.0.07 and `SUMA_glxdino`;
- Anatomist 6.0.16, QuPath 0.7.0, and DSI Studio 2024.06.12;
- TrackVis reached its license dialog.

Slicer 5.10.0 remained alive and initialized its Welcome module, but did not
map a window during the 75-second automated interval. The FSLeyes 6.0.7.22 and
Blender 5.0.1 crashes are amd64-emulation/allocator failures, not renderer
regressions. Both fail under `virpipe` and `llvmpipe`, and both fail before an
OpenGL context is created. QEMU 11.0.1 maps allocator arenas in the AArch64
host's `0x0000ffff...` range; the emulated x86_64 program then dereferences the
same low address with `0xffffffff...` sign extension. For example, Blender maps
`0x0000ffff68a00000` and faults at `0xffffffff68a0a440`, while the FSLeyes
dependency chain maps `0x0000ffff23c00000` and faults at
`0xffffffff23c16500`.

FSLeyes itself is not the crashing native component. FSLeyes 1.18.0 imports
pandas 2.3.3, pandas imports PyArrow 23.0.1, and PyArrow's bundled
Arrow-prefixed jemalloc crashes during initialization. `import pandas` and
`import pyarrow` are minimal reproducers; NumPy, wxPython, PyOpenGL, FSLeyes GL
modules, and a live wx GL context all succeed. Blender is linked with its own
malloc replacement and even `blender --version` crashes during allocator
initialization, before its dynamic loader finishes X11 initializers. A native
arm64 application build, an amd64 build without jemalloc, or corrected QEMU
guest address-space handling is required; VirGL changes cannot fix either
failure.

BrainSuite's tested GUI entrypoint was missing `libmwlaunchermain.so`. The
binary declares it as a required dependency and its RPATH names the expected
bundled directories, but the library and MATLAB Runtime tree are both absent.
This packaging defect is tracked as NeuroContainers issue 3020.

A second catalogue pass covered additional GUI families. BIDSvue
0.1.20260704 mapped a clean Electron/Chromium window, although Chromium logged
permission failures while trying to open `/dev/dri/card0`; the usable window is
therefore not yet evidence that Chromium's own compositor selected virgl.
BrainVISA 6.0.38 completed toolbox and workflow-controller initialization, but
its window was hidden by a zero-byte persistent
`~/.config/openbox/lxde-rc.xml`; restoring `/etc/xdg/openbox/LXDE/rc.xml`
removed that unrelated desktop-state error.

Voreen 5.3.0 started with Mesa 25.2.8 and explicitly reported
`virgl (vmsh Darwin VirGL)`, then failed to compile its render-target viewer
shader. The generated first line is `#version 130 core`; GLSL 1.30 does not
accept a profile suffix, so Mesa reports `illegal text following version
number`. This is an application/container shader-generation defect exposed by
the modern renderer, not a VM crash. MINC Register 1.9.18 reproduces the old
vtest disconnect below and aborts with SIGABRT. MIPView 0.1.4 reached a mapped
Qt 6 window, but the same persistent Openbox dialog initially obscured it.
VesselVio 1.1.2 loaded VTK 9 and then exited silently before mapping a window.
MIPAV 11.3.3 loaded its Java runtime and began extracting the Linux OpenGL
Java3D native library, while ilastik 1.4.0 remained in cold CVMFS page reads;
both need warm-cache follow-up before assigning a graphics verdict.

A repeatable compatibility gap exists in the container-side vtest path. The
following applications abort with `lost connection to rendering server on 8
read -1 22` under `virpipe`, but start successfully with `llvmpipe`:

- Connectome Workbench 2.1.0 (Mesa 22.3.6);
- MRIcroGL and Surfice (Mesa 21.0.3);
- MITK Diffusion (Mesa 21.2.6);
- SlicerSALT (Mesa 20.2.6).

Removing the Mesa GL/GLSL version overrides does not change those failures.
Conversely, tested containers with Mesa 23.2.1, 25.0.7, and 25.2.8 connect to
the same Ubuntu `virgl_test_server` 1.0.0 bridge. The current evidence therefore
locates this gap at old-Mesa vtest compatibility, before commands reach the
vmsh virtio-gpu/VirGL decoder. Software rendering is a valid per-application
fallback, but it must not be presented as accelerated rendering.

The sweep also found a boot-order integration bug: Ubuntu's
`systemd-binfmt.service` can replace the early guest-init `qemu-x86_64`
registration. NeurodeskAppX now waits for that service and then restores the
registration before launching CVMFS amd64 applications. A separate image fix
is staged in NeuroContainers PR 3019 to keep `/home/jovyan/.local` owned by the
desktop user when Glass prepares a fresh persistent home.

Evidence is under `build/ndappx-gpu/logs/newimg-*.txt` and
`build/ndappx-gpu/captures/newimg-*-presentation/`. These are local runtime
artifacts and are intentionally not committed.

### OpenGL 4.1 continuation (2026-08-10)

The checkout now truthfully exposes OpenGL 4.1 / GLSL 4.10 and completes the
entire exported `KHR-GL41.*` case set on the intended Mesa 26.1.6 guest runtime.
The renderer work is published in cc commit `6b01aff`; the vmsh publication
commit records the matching submodule pointer, fixture, and evidence.

Authoritative result:

```text
VK-GL-CTS:       opengl-cts-4.6.8.1 (067e8832315e79817ede1c4863804e440f5d1c80)
Guest Mesa:      26.1.6 from /opt/mesa
Renderer:        virgl (vmsh Darwin VirGL)
OpenGL / GLSL:   4.1 core / 4.10
Exported cases:  11,884
Observed cases:  11,884
Pass:            10,245
NotSupported:    1,639
Fail:            0
Missing / extra: 0 / 0
Complete QPAs:   379 / 379
```

`NotSupported` is the CTS verdict for cases gated by optional extensions or
invalid target/format combinations; no required/applicable case failed. This
is a complete clean implementation result, not a claim of formal Khronos
product certification.

Artifacts are under:

```text
build/gpu-cts/shared-mesa26-gl41-88/gpu-cts/mesa26/manifest.json
build/gpu-cts/shared-mesa26-gl41-88/gpu-cts/mesa26/summary.json
build/gpu-cts/shared-mesa26-gl41-88/gpu-cts/mesa26/cases.txt
build/gpu-cts/shared-mesa26-gl41-88/gpu-cts/mesa26/shards/
```

Two runner constraints are part of the result. A monolithic CTS process grows
past both 6 GiB and 8 GiB after approximately 8,300 completed cases. Mesa 26
also accumulates enough packed-pixel shader variants in one process to wedge
Apple's native GL command queue. The checked-in `run-gl41-cts.sh` therefore
uses fresh processes for top-level groups, finer texture-swizzle shards, and
per-format packed-pixel shards. It verifies exact case-set equality and that
every QPA ends with `#endSession`.

The final renderer fixes include:

- hybrid TGSI constants: ordinary uniforms below 1,024 registers and native
  `std140 uvec4` blocks for larger declarations, preserving both normal draws
  and the FP64 maximum-uniform cases;
- tessellation-control/evaluation handles in program-cache invalidation;
- bounded stale OpenGL-error draining around checked RGB9_E5 readback and
  conversion paths, with red-only shared-exponent and pending-error behavior
  regressions;
- all earlier texture swizzle, multisample typed-view, UNORM24 conversion,
  indirect draw, shader subroutine, FP64, tessellation, and transform-feedback
  fixes retained.

The manual runner originally inherited the distro Mesa 25.0.7 because the
`/opt/mesa` environment lived only in the systemd unit. Both fixture runners
now set `LD_LIBRARY_PATH=/opt/mesa/lib` and
`LIBGL_DRIVERS_PATH=/opt/mesa/lib/dri` themselves. The authoritative result
above was rerun from a clean VM after `glxinfo` confirmed Mesa 26.1.6.

Do not raise a capset field merely to make Mesa report a newer version. A
capability is advertised only after the decoder, host execution, resource
transfer, shader translation, and behavior tests all support it.

## Repository and remote state

### vmsh root

- Working directory: `/Users/joshua/dev/projects/vmsh`
- Branch: `agent/virtio-gpu-darwin`
- Last application-workload commit: `3f45b92` (`Add Firefox WebGL GPU fixture`)
- Remote branch: `origin/agent/virtio-gpu-darwin`
- Draft PR: [tinyrange/vmsh#244](https://github.com/tinyrange/vmsh/pull/244)
- PR title: `Add Darwin guest GPU acceleration`
- PR base: `main`

The OpenGL 4.1 publication commit contains:

```text
GPU_PLAN.html
GPU_PROGRESS.md
cc -> 12b1389
images/gpu-cts/
```

The GPU history at the root includes:

```text
(current) Publish OpenGL 4.1 conformance support
3f45b92 Add Firefox WebGL GPU fixture
81c3568 Run OpenArena through native GPU acceleration
98e93ea Run full glmark2 through native Glass
040b42b Add Darwin guest GPU acceleration
```

### cc submodule

- Path: `cc`
- Branch: `vmsh-virtio-gpu-3d`
- HEAD: `12b1389` (`Skip native VirGL tests without accelerated OpenGL`)
- Tracking branch: `origin/vmsh-virtio-gpu-3d`
- The branch and its remote are synchronized at that commit.
- Working tree: clean at publication time.

Committed GPU history at the cc boundary is:

```text
12b1389 Skip native VirGL tests without accelerated OpenGL
479d72d Add non-Darwin VirGL replay stubs
6b01aff Complete Darwin VirGL OpenGL 4.1 support
1d2b156 Advance Darwin VirGL conformance support
a8c37a2 Run Firefox WebGL through Darwin VirGL
0e5176b Run OpenArena through Darwin VirGL
158f5f3 Complete glmark2 and native scanout
51b77df Add Darwin VirGL renderer
```

The OpenGL 4.1 conformance commit changes:

```text
M cmd/virgl-replay/main.go
M internal/virgl/capset.go
M internal/virgl/capset_test.go
A internal/virgl/formats.go
A internal/virgl/formats_darwin_test.go
M internal/virgl/capture.go
M internal/virgl/framebuffer_darwin_test.go
M internal/virgl/gl_darwin.go
M internal/virgl/host_darwin.go
A internal/virgl/query_darwin_test.go
M internal/virgl/renderer.go
M internal/virgl/renderer_test.go
M internal/virgl/replay_darwin.go
A internal/virgl/streamout_darwin_test.go
M internal/virgl/tgsi.go
M internal/virgl/tgsi_darwin_test.go
M internal/virgl/tgsi_test.go
```

Commit `6b01aff` contains 10,738 insertions and 1,083 deletions. It was pushed
before the root submodule pointer was updated, so the root commit references a
durable cc commit.

### Gowin submodule

- Path: `gowin`
- Branch: `vmsh-gpu-shared-context`
- HEAD: `3f02e7d42a2d3443dd6a4dde89e76110534eee1d`
- Tracking branch: `origin/vmsh-gpu-shared-context`
- Working tree: clean

Relevant Gowin commits are:

```text
3f02e7d Skip shared-context test without accelerated OpenGL
5ccdc25 Expose native OpenGL share groups
c1ff06a Add Darwin shared OpenGL contexts
```

## Product intent and constraints

The following decisions are intentional and should be preserved:

- Darwin/ARM64 is the first host target. The development machine is an Apple
  M4 MacBook Air.
- SquadVM and native Glass are the current integration surface. Do not move the
  experiments into NeurodeskAppX yet.
- The long-term application goals include neuroimaging applications in
  Neurodesk and Steam games in SquadVM or a future dedicated product.
- vmsh is an interactive, session-oriented shell. When shell state matters,
  tests and examples should select a system context and then run ordinary
  commands rather than treating vmsh as only a one-shot image runner.
- This project owns the VirGL protocol decoder. Do not replace it with
  virglrenderer or rutabaga_gfx, and do not hide missing behavior behind one of
  those libraries.
- Mesa is the unmodified guest driver and protocol producer. The conformance
  fixture builds a current upstream Mesa release, but this project should not
  maintain a Mesa fork to work around renderer gaps. Fix the first-party host
  implementation and keep moving the fixture to modern upstream Mesa versions.
- Capsets must be truthful. Missing features should keep Mesa at a lower API
  version until the behavior exists.
- Important tests run normally. Never gate them behind an environment variable
  on the grounds that they are too important or expensive.
- Tests should protect user-visible behavior and structured protocol contracts,
  not exact prose, helper shape, fallback order, or incidental argv details.
- All development VM caches, runtime state, captures, and storage belong under
  this repository. Do not use or clean the user's production/global caches.
- The user has other terminals and a production vmsh/ccvm instance. Never stop
  a process by a broad name match. Resolve an exact development VM name and PID
  before terminating it.

## Architecture and code map

### Guest transport and protocol boundary

`cc/internal/virtio/gpu.go` implements the virtio-gpu device surface used by
the guest. It handles capset discovery, context lifecycle, 3D resource
creation, attach/detach, transfers, opaque 3D submissions, fences, scanout, and
native-frame acquisition.

`cc/internal/virgl/renderer.go` is the platform-independent ownership and
validation layer. It tracks contexts and resources, bounds allocations, parses
the VirGL command framing, and delegates validated host work through
`hostBackend`. `maxResourceBytes` is currently 256 MiB.

`cc/internal/virgl/capset.go` publishes legacy VirGL and VirGL2 capsets. VirGL2
exists so modern fields can be added incrementally without pretending that the
current implementation already supports a modern API.

### Darwin execution backend

`cc/internal/virgl/host_darwin.go` owns the NSOpenGL execution context and all
host-side VirGL object/state tables. It implements resource allocation and
transfer, framebuffer state, fixed-function Gallium state, draw dispatch,
blits/copies, shader program construction, resource lifetime, and native
scanout.

`cc/internal/virgl/gl_darwin.go` dynamically loads the required host OpenGL
entry points. This is the binding layer to extend when a new renderer feature
requires another OpenGL 4.1 function.

`cc/internal/virgl/tgsi.go` parses Mesa's TGSI text and translates it to GLSL.
Unsupported syntax and instructions must return explicit errors. Do not silently
drop instructions or manufacture values.

`cc/internal/virgl/host_other.go` preserves a clear unsupported boundary on
non-Darwin hosts. Linux GLX and Windows WGL are later portability milestones.

### Presentation

The Darwin renderer can join the NSOpenGL share group exposed by Gowin. It
publishes frames from a bounded shared-texture pool with producer/consumer
fences, generation tracking, damage, release, and resize-safe teardown.
SquadVM's display path samples these textures in Glass. CPU readback is lazy and
should occur only for VNC or an explicit diagnostic snapshot.

The key integration points are:

- `cc/internal/virgl/host_darwin.go`
- `cc/internal/virtio/gpu.go`
- `cmd/squadvm/backend.go`
- `cmd/squadvm/display.go`
- `cmd/squadvm/main.go`
- Gowin's shared-context implementation on `vmsh-gpu-shared-context`

### Capture and replay

`cc/internal/virgl/capture.go` records a bounded, gzip-compressed `VIRGLCAP`
stream. Set these only for a diagnostic development VM:

```sh
VMSH_VIRGL_CAPTURE="$PWD/build/gpu-cts/problem.cap"
VMSH_VIRGL_CAPTURE_FRAMES=240
```

The capture variables enable diagnostics; they do not gate a test. A capture
contains resource/context lifecycle, transfers, command execution, and scanout
checkpoints, allowing host-only deterministic replay without repeatedly booting
the guest.

`cc/cmd/virgl-replay` and `cc/internal/virgl/replay_darwin.go` support:

```sh
build/gpu-cts/run/virgl-replay -summary capture.cap
build/gpu-cts/run/virgl-replay -frame 120 capture.cap frame.png
build/gpu-cts/run/virgl-replay -frame 120 -trace-draws capture.cap frame.png
build/gpu-cts/run/virgl-replay -frame 120 -resource 42 capture.cap texture.png
build/gpu-cts/run/virgl-replay -find-projective capture.cap
build/gpu-cts/run/virgl-replay -find-shader TEX capture.cap
```

The replay implementation can select a resource, mip level, or draw within a
frame and can trace all draw state. Prefer replay for visual fine-tuning and
isolating texture, projection, ordering, and shadow defects after one useful
guest capture has been obtained.

## Completed and pushed functionality

The code already pushed to PR #244 includes the following.

### Virtio-gpu 3D device and owned VirGL protocol

- Capset discovery and negotiation.
- Context create/destroy and resource attach/detach.
- 3D resource creation and bounded backing storage.
- Transfers to and from the host across fragmented virtio guest backing.
- Opaque command submission with strict framing validation.
- Fence identity and completion.
- Explicit unsupported-command failures.
- Renderer-to-framebuffer scanout without requiring host GL in protocol tests.

### Darwin VirGL execution

- Dedicated NSOpenGL context lifecycle.
- Bounded context, object, resource, shader, and state tables.
- Buffers, 2D textures, cube maps, mip levels, depth/stencil targets, surfaces,
  sampler views, sampler states, and framebuffer state.
- Indexed and non-indexed draws, base vertex, multiple vertex buffers, sparse
  buffer slots, client and VBO updates, and all primitive modes exercised by
  the current application corpus.
- Viewport, scissor, culling, front-face orientation, depth/stencil, blend,
  color mask, clear, point size, point sprites, primitive restart, blits, and
  resource copies.
- Real transfer-from-host and framebuffer readback.
- Resource retention while surfaces, sampler views, draw bindings, or other
  objects refer to the resource.
- Shader submission reassembly with bounded sizes and offsets.

### TGSI compiler

The compiler covers the declarations, varyings, immediates, constants,
swizzles, write masks, arithmetic, dot products, reciprocal/square-root
operations, comparisons, integer operations, indirect addressing, discard,
structured control flow, system values, and texture operations required by
kmscube, glmark2, OpenArena, Firefox WebGL, and the completed ES2 baseline.

### Native scanout

- Shared NSOpenGL contexts through Gowin.
- Bounded three-texture presentation pool.
- Producer and consumer synchronization fences.
- Generation, damage, release, and resize-safe ownership.
- Zero routine CPU readbacks on the native path.
- One lazy readback when the first explicit CPU snapshot is requested.

## Modern renderer work through cc commit 6b01aff

This commit extends the application-proven implementation substantially. Treat
the items in this section as the first coherent modern-GL/conformance batch.

### Conformance-driven ES2 corrections

- Full independent front/back stencil state and stencil references.
- Clear operations temporarily override and then restore depth, stencil, and
  color write masks as required by Gallium semantics.
- Correct programmable point size and point-sprite coordinate origin.
- Blend dithering and sampler object behavior.
- Correct scaled and normalized byte vertex formats.
- Sparse vertex buffer slots and zero-stride constant attributes.
- Correct retirement of previously enabled vertex attributes.
- Correct base-vertex indexed draws.
- Cube-map transfers, sampling, blits, face selection, and mip selection.
- Improved projected, biased, and explicit-LOD texture translation.
- More complete integer TGSI behavior.

### Instancing and system values

- Per-attribute instance divisors.
- Instanced indexed and non-indexed draw calls.
- TGSI `INSTANCEID` and `VERTEXID` translation.
- `startInstance != 0` remains explicitly unsupported instead of being
  mis-rendered.

### Multiple render targets

- Up to eight host color attachment slots are represented internally.
- The currently advertised capset limit is four render targets.
- Framebuffer commands parse every color surface, bind or detach every native
  attachment, and call `glDrawBuffers`.
- Attachment identity is part of the framebuffer binding cache.
- `TestFramebufferClearUpdatesEveryColorTarget` proves one VirGL clear updates
  two independently attached textures.

### Layered transfer and 2D texture arrays

- Transfer layout honors row stride, layer stride, depth, overflow bounds, and
  packed staging.
- `PIPE_TEXTURE_2D_ARRAY` resources allocate through `glTexImage3D` and upload
  through `glTexSubImage3D`.
- Up to 256 array layers are currently advertised.
- Surface and sampler-view objects retain mip level and first/last layer.
- Array layers can be framebuffer attachments through
  `glFramebufferTextureLayer`.
- Blits and multi-layer resource copies preserve array layer selection.
- TGSI `SVIEW 2D_ARRAY`, `sampler2DArray`, `TEX`, `TXB`, and `TXL` are
  translated.

Behavior tests include:

- `TestArrayTextureTransferPreservesLayerStride`
- `TestArrayTextureLayersCanBeRenderedAndReadBack`
- `TestResourceCopyRegionCopiesMultipleArrayLayers`
- `TestArraySamplerTGSICompilesInDarwinHostContext`

Layered rendering across a first/last layer range still requires geometry
shader support. The current implementation accepts a single selected layer and
returns an explicit error for a multi-layer framebuffer surface.

### Uniform buffers

- VirGL opcode 27, `VIRGL_CCMD_SET_UNIFORM_BUFFER`, is implemented.
- Stage, slot, resource, alignment, offset, and length are validated.
- Bound guest buffers participate in resource retain/release lifetime.
- Draws upload the selected guest range with `glUniform4fv`.
- TGSI accepts dimensional constant declarations such as
  `DCL CONST[1][0..N]`, direct dimensional sources, and indirect dimensional
  constant sources.
- GLSL constant arrays are stage and block specific.
- The capset currently advertises 12 uniform blocks.
- `TestUniformBufferSuppliesFragmentConstants` renders a pixel from an actual
  `CONST[1]` source rather than merely checking command parsing.

The published GL 4.1 continuation supersedes the original compatibility-only
path. TGSI blocks below 1,024 registers use native uniform arrays; larger
declarations use real `std140 uvec4` uniform-buffer bindings so ordinary draws
remain stable while maximum-uniform FP64 cases fit Apple's limits.

### Shader program lifetime

The official monolithic ES2 runner exposed unbounded host-program accumulation
and eventually caused macOS to terminate the VM path under memory pressure. The
renderer now uses a tested least-recently-used program cache capped at 128 host
programs. Shader deletion and context teardown invalidate the relevant cache
entries.

`TestProgramCacheEvictsLeastRecentlyUsedProgram` protects the bounded behavior.
Experimental compile-context rotation, autorelease-pool manipulation, and
forced `glFinish` diagnostics were removed; they are not part of the intended
solution.

### Current honest capset

Commit `1d2b156` was the last low-capability boundary. Commit `6b01aff` now
advertises the complete set needed for Mesa 26.1.6 to create real
OpenGL 4.1 core contexts. Geometry/tessellation shaders, multisampling,
streamout/query semantics, sampler and texture-buffer behavior, indirect draw,
FP64, multiple viewports, texture gather, and the required format families are
implemented behind those fields and covered by behavior tests plus the exact
CTS result. Optional GL 4.2+ or extension-only fields remain disabled. Host
feature fallback/version overrides remain zero; the 4.1 identity is earned by
capabilities, not forced.

## Application evidence

### kmscube / first guest cube

The Debian ARM64 guest identifies renderer `virgl`, continuously page flips,
and previously sustained roughly 440 frames/second through the native run. The
visible output has correct orientation, clipping, culling, and face ordering.

### glmark2

Fixture: `images/gpu-glmark2`

All 33 stock `glmark2-es2-drm` scenes complete. This covers build, texture,
shading, bump, effects, desktop, buffer updates, complex models, conditionals,
functions, loops, VBO/client arrays, render targets, shadow/depth passes,
dynamic uploads, and repeated context cycles.

### OpenArena

Fixture: `images/gpu-openarena`

Stock OpenArena 0.8.8 runs as an ordinary GLX/OpenGL 2.0 application. The
deterministic benchmark loads a full map, models, textures, HUD, 114 GLSL
shaders, animated bot combat, and repeated dynamic buffers. Its final recorded
1440x900 result was 1,201 frames in 54.5 seconds, or 22.0 fps, improved from the
initial 15.1 fps baseline.

Capture/replay work corrected orientation, geometry order, texture black
strips, outdoor shadow corruption, and mountain-edge artifacts. Preserve the
large captures until equivalent smaller reproductions exist.

### Firefox WebGL

Fixture: `images/gpu-firefox`

Firefox ESR runs four loopback-hosted WebGL 1 scenes:

- indexed, depth-tested geometry;
- render-to-texture plus post-processing;
- dynamic additive point sprites;
- a procedural fragment shader.

The final run sustained approximately 60 fps at 1440x900. Structured telemetry
and guest `gl.readPixels` PNGs were distinct and non-black, proving the browser
actually rendered through the guest VirGL path.

## Conformance fixture

The committed `images/gpu-cts` directory contains:

```text
images/gpu-cts/Dockerfile
images/gpu-cts/README.md
images/gpu-cts/run-gpu-cts.sh
images/gpu-cts/vmsh-gpu-cts.service
images/gpu-cts/xorg.conf
```

The fixture builds:

- unmodified upstream Mesa 26.1.6 from the official release archive;
- Mesa tarball SHA-256
  `5296b88a0f1e012e2cb9ada150a2bbadf728ca81e5a4fb2ab43c83a4d2158606`;
- one cohesive `/opt/mesa` containing EGL, GL, GLES1, GLES2, GBM, and the
  VirGL DRI driver;
- Khronos VK-GL-CTS tag `opengl-cts-4.6.8.1`;
- CTS commit `067e8832315e79817ede1c4863804e440f5d1c80`;
- `deqp-gles2`, `deqp-gles3`, `deqp-gles31`, `glcts`, and `cts-runner`.

Building EGL/GL/GLES/GBM/VirGL together avoids distribution-private DRI ABI
mismatches. GLES1 is deliberately enabled because the official ES2 runner
creates every advertised EGL client API. This is not a Mesa patch or fork.

At service startup the fixture:

1. starts Xorg and refuses to continue unless `glxinfo` selects `virgl`;
2. writes an exact renderer/version manifest;
3. runs GLES2, GLES3, GLES3.1, and real EGL desktop-context probes for GL 3.0,
   3.1, 3.2, 3.3, 4.0, and 4.1;
4. runs dEQP-GLES2 in 45 restart-safe groups;
5. atomically updates structured progress after every group;
6. runs Khronos's official `cts-runner --type=es2` configuration;
7. considers the official run complete only when every QPA ends with
   `#endSession`.

Completed group status files are the resume boundary. Do not erase a result
tree merely because its service or VM stopped.

### Authoritative Mesa 25 ES2 baseline

Artifacts:

```text
build/gpu-cts/shared-mesa25/gpu-cts/
```

Identity:

```text
renderer:         virgl
OpenGL:           2.0 Mesa 25.0.7-2+deb13u1
GLSL:             1.20
CTS tag:          opengl-cts-4.6.8.1
completed groups: 45 / 45
```

Result counts:

| Status | Count |
| --- | ---: |
| Pass | 17,699 |
| Fail | 18 |
| NotSupported | 303 |
| CompatibilityWarning | 14 |
| QualityWarning | 2 |

`summary.json` has `complete: false` because the official monolithic runner did
not finish cleanly, even though all 45 grouped dEQP slices completed. Do not
misreport this as full Khronos conformance.

The remaining 18 failures are exactly:

```text
dEQP-GLES2.functional.shaders.scoping.valid.local_variable_hides_function_parameter_fragment
dEQP-GLES2.functional.shaders.scoping.valid.local_variable_hides_function_parameter_vertex
dEQP-GLES2.functional.texture.mipmap.2d.projected.linear_linear_clamp
dEQP-GLES2.functional.texture.mipmap.2d.projected.linear_linear_mirror
dEQP-GLES2.functional.texture.mipmap.2d.projected.linear_linear_repeat
dEQP-GLES2.functional.texture.mipmap.2d.projected.linear_nearest_clamp
dEQP-GLES2.functional.texture.mipmap.2d.projected.linear_nearest_mirror
dEQP-GLES2.functional.texture.mipmap.2d.projected.linear_nearest_repeat
dEQP-GLES2.functional.texture.mipmap.2d.projected.nearest_linear_clamp
dEQP-GLES2.functional.texture.mipmap.2d.projected.nearest_linear_mirror
dEQP-GLES2.functional.texture.mipmap.2d.projected.nearest_linear_repeat
dEQP-GLES2.functional.texture.mipmap.2d.projected.nearest_nearest_clamp
dEQP-GLES2.functional.texture.mipmap.2d.projected.nearest_nearest_mirror
dEQP-GLES2.functional.texture.mipmap.2d.projected.nearest_nearest_repeat
dEQP-GLES2.functional.texture.mipmap.cube.projected.linear_linear
dEQP-GLES2.functional.texture.mipmap.cube.projected.linear_nearest
dEQP-GLES2.functional.texture.mipmap.cube.projected.nearest_linear
dEQP-GLES2.functional.texture.mipmap.cube.projected.nearest_nearest
```

Interpretation:

- two failures are guest GLSL compiler scoping behavior;
- sixteen failures are projected mipmap seam/LOD behavior;
- all previously observed clipping failures are fixed.

Continue running ES2 regressions in parallel, but do not let these two narrow
clusters prevent progress on GLES3/desktop GL feature implementation.

### Current Mesa 26 probe baseline

The current live identity and complete GL 4.1 result supersede the earlier
GL 2.1 probe snapshots:

```text
renderer: virgl (vmsh Darwin VirGL)
OpenGL:   4.1 (Core Profile) Mesa 26.1.6
GLSL:     4.10
GLES:     3.0 Mesa 26.1.6
```

See the authoritative artifact paths and counts in the executive status. The
older `shared-mesa26-streamout-01` and `shared-mesa26-formats2-01` trees remain
useful only as historical capability-ladder evidence.

### Current image archive

Local archive:

```text
build/gpu-cts/vmsh-gpu-cts-mesa26-gl41.docker.tar
```

SHA-256:

```text
77811a7525759ccd724921007a92eb13891196b88666be30269165f0890615ad
```

The archive was built natively on `astra-pi5` and pulled into the repository.
It contains Mesa 26.1.6, the pinned CTS revision above, and
`cts-gl41-harness.patch` with SHA-256
`6328a57739263c06cc9e05386f0a847ec477c1f3933ee9d0b209c461f96d5a1c`.
The patch guards a CTS deinitializer's use of a GL 4.2 entry point in a GL 4.1
context; it does not waive cases or change expected results.

## Fresh verification performed for this handoff

The following passed on 2026-08-10 after the final Mesa 26 conformance run:

```sh
cd /Users/joshua/dev/projects/vmsh/cc
go test ./... -count=1
go vet ./...
git diff --check

cd /Users/joshua/dev/projects/vmsh
go test ./... -count=1
go vet ./...
git diff --check
sh -n images/gpu-cts/run-gpu-cts.sh images/gpu-cts/run-gl41-cts.sh
```

Observed package results included:

```text
ok j5.nz/cc/internal/virgl
ok j5.nz/cc/internal/virtio
ok github.com/tinyrange/vmsh/cmd/squadvm
ok github.com/tinyrange/vmsh/internal/...
```

The full Mesa 26 GL 4.1 result was produced in repository-local VMs 88 and 89.
VM 89 stopped cleanly after the final recovery shards. The user's production
`ccvm` using `/Users/joshua/Library/Caches/ccx3` was not touched.

## Important behavior tests through cc commit 6b01aff

New or substantially relevant behavior tests include:

- `TestCapsetAdvertisesImplementedArrayTexturesAndMultipleRenderTargets`
- `TestCapsetV2KeepsModernFeatureFallbacksDisabled`
- `TestRendererPublishesBothVirGLCapsetGenerations`
- `TestFixedPointVertexFormatRendersThroughDarwinHost`
- `TestVertexElementPreservesInstanceDivisor`
- `TestProgramCacheEvictsLeastRecentlyUsedProgram`
- `TestSparseVertexBufferSlotsRenderMixedAttributes`
- `TestFramebufferClearUpdatesEveryColorTarget`
- `TestArrayTextureLayersCanBeRenderedAndReadBack`
- `TestUniformBufferSuppliesFragmentConstants`
- `TestClearIgnoresBoundDepthWriteMask`
- `TestClearIgnoresBoundStencilWriteMask`
- `TestLogicalStencil8ControlsAColorDraw`
- `TestBlendAndScissorAffectRenderedPixels`
- `TestRasterizerWindingAccountsForHostFramebufferOrigin`
- `TestDrawDisablesRetiredVertexAttributes`
- `TestZeroStrideVertexBufferSuppliesAConstantAttribute`
- `TestPointSpriteCoordinateModeControlsVerticalOrigin`
- `TestIndexedDrawAppliesBaseVertex`
- `TestSamplerViewAndStateAffectRenderedPixels`
- `TestIndependentSamplerStatesCanShareOneTexture`
- `TestGalliumMipFilterNoneDoesNotSelectMipLevels`
- `TestCubeMapTransfersAndSamplingRenderEveryFace`
- `TestBlitPopulatesRequestedCubeFaceAndMipLevel`
- `TestResourceCopyRegionCopiesMultipleArrayLayers`
- `TestStencilTransferPreservesOneBytePixelsAndGuestRows`
- `TestArrayTextureTransferPreservesLayerStride`
- `TestCubeSamplerTGSICompilesInDarwinHostContext`
- `TestArraySamplerTGSICompilesInDarwinHostContext`
- `TestVertexCubeExplicitLODUsesESMagnificationCrossover`
- `TestTranslateTGSIUsesHostInstancingSystemValues`

These are valuable because they validate pixels, transfers, resource lifetime,
structured capset data, or shader behavior. Continue this pattern: for every
new advertised format or command family, identify the user-visible bug the test
would catch and assert the actual outcome.

## Known gaps and risks

### Conformance runner process bounds

The renderer completes every GL 4.1 case, but upstream CTS process lifetime is
not bounded enough for a monolithic run on this fixture. Use the checked-in
sharded runner. Do not raise guest memory again and call an eventual OOM a
renderer failure. Exact expected/observed case equality and complete per-shard
QPAs are the authoritative contract.

### Packed-pixel native queue pressure

Mesa 26's monolithic `packed_pixels` group accumulates enough shader variants
to wedge Apple's GL command queue. The exact stalled case passes in a fresh
process, and all 4,732 packed-pixel cases pass when split by format family.
Keep that subdivision unless a future native-driver or renderer lifetime fix
is proven by a complete monolithic run.

### Optional extension results remain honest

The 1,639 `NotSupported` results are dominated by optional extensions such as
fragment shading rate, clip control, texture barrier, and invalid
target/format combinations. Do not expose those extensions merely to reduce
the count. Add them only with the same end-to-end implementation and behavior
evidence used for the GL 4.1 core.

### Program and native-driver lifetime still matter

The 128-entry host-program LRU bounds renderer-owned programs. The sharded CTS
run also bounds CTS and native-driver variant lifetime. Preserve both layers;
do not reintroduce unconditional `glFinish`, fake capability overrides, or a
larger VM as substitutes for explicit ownership and completion boundaries.

### Darwin OpenGL has a hard ceiling

The NSOpenGL backend can target OpenGL 4.1 core / GLSL 4.10, but not newer
desktop OpenGL. After honest 4.1 conformance, modern Steam/Proton workloads need
a Vulkan-over-Metal design, likely with newer OpenGL implemented over Vulkan.
Do not encode fake GL 4.2+ capabilities into the current backend.

## Prioritized continuation plan

### Shift 0: preserve the conformance boundary

1. Preserve the Mesa 26 manifest, summary, case list, QPAs, image checksum, and
   CTS patch checksum listed above.
2. Run both modules' full Go tests and vet before publication.
3. Confirm the root and cc branches remain synchronized with their remotes and
   the published submodule pointer resolves to `12b1389`.
4. Keep all development caches and VM storage repository-local and leave the
   production/global `ccvm` untouched.

### Shift 1: publish intentionally

1. Review the large cc renderer diff as one coherent GL 4.1 capability change.
2. Commit and push the cc submodule before updating the root pointer.
3. Include the CTS harness/runner and this report in the root publication.
4. Do not publish, commit, or open/update a PR unless the user requests it.

### Shift 2: keep the result reproducible

1. Rebuild future ARM64 images on `astra-pi5` with provenance disabled.
2. Verify `/opt/mesa` identity in `glxinfo` before starting long tests.
3. Use `run-gl41-cts`; do not return to a monolithic process.
4. Require exact expected/observed case equality, zero failures, and complete
   QPA sessions before replacing this baseline.

### Shift 3: recheck applications

Replay the representative OpenArena and Firefox captures, then run an
interactive context transition followed by repeated guest commands to protect
the session-oriented vmsh workflow, daemon reuse, and native presentation path.

### Shift 4: optional extensions only when valuable

Treat every optional extension as a new end-to-end capability. Implement it
only for a real application need, add behavior evidence, and rerun the affected
CTS slice plus the full exact suite before changing the capset.

### Shift 5 and beyond: move past Darwin's frozen OpenGL

Further core desktop API progress belongs in a Vulkan-over-Metal backend, with
newer OpenGL layered over that renderer if needed. Keep the NSOpenGL backend at
its truthful 4.1 ceiling and preserve this conformance corpus as its regression
boundary.

## Build and run procedures

### Build local development binaries

Use repository-local Go caches:

```sh
cd /Users/joshua/dev/projects/vmsh
mkdir -p build/go-cache build/gpu-cts/run

(
  cd cc
  GOCACHE="$PWD/../build/go-cache" go build \
    -o ../build/gpu-cts/run/cc ./cmd/cc
  GOCACHE="$PWD/../build/go-cache" go build \
    -o ../build/gpu-cts/run/ccvm ./cmd/ccvm
  GOCACHE="$PWD/../build/go-cache" go build \
    -o ../build/gpu-cts/run/glass ./cmd/glass
  GOCACHE="$PWD/../build/go-cache" go build \
    -o ../build/gpu-cts/run/virgl-replay ./cmd/virgl-replay
)

GOCACHE="$PWD/build/go-cache" go build \
  -o build/gpu-cts/run/squadvm ./cmd/squadvm
```

The existing `build/gpu-cts/run` directory also contains other development
binaries. Rebuild only what the current change requires.

### Build the conformance image on astra-pi5

Build ARM64 natively. Keep BuildKit provenance disabled so cc's archive importer
sees the expected single-platform descriptor:

```sh
docker buildx build \
  --platform linux/arm64 \
  --provenance=false \
  --output type=docker,dest=/home/joshua/vmsh-gpu-cts-mesa26-gl41.docker.tar \
  images/gpu-cts
```

The source directory must be present on `astra-pi5`; synchronize only the
fixture or an intentional repository checkout. Copy the resulting archive to
`build/gpu-cts/`, verify its SHA-256, and retain only the latest known-good
archive plus any image needed to reproduce an authoritative result.

### Start the Mesa 26 fixture

Use a unique VM name. The following name is an example; increment it rather
than colliding with an existing session:

```sh
cd /Users/joshua/dev/projects/vmsh

build/gpu-cts/run/squadvm \
  -name gpu-cts-mesa26-gl41-next \
  -cache-dir "$PWD/build/gpu-cts/cache" \
  -storage "$PWD/build/gpu-cts/shared-mesa26-next" \
  -memory-mb 8192 \
  "docker-archive:$PWD/build/gpu-cts/vmsh-gpu-cts-mesa26-gl41.docker.tar"
```

For a manual GL 4.1 run, create `.vmsh-gpu-cts-manual` in the shared storage
before boot, then run `/usr/local/bin/run-gl41-cts`. The runner sets the
`/opt/mesa` library/driver environment itself. Verify the manifest reports Mesa
26.1.6; a manual shell without those paths silently selects distro Mesa 25.

Use 8192 MiB for comparison with the authoritative run, but retain sharding.
A monolithic process was OOM-killed at both 6 GiB and 8 GiB.

For a resumable run, reuse the same storage directory with the same intended
fixture. For a clean comparison, use a new explicitly named storage directory.
Never point a new experimental run at the authoritative Mesa 25 result tree.

### Monitor structured progress

```sh
jq . build/gpu-cts/shared-mesa26-next/gpu-cts/manifest.json
jq . build/gpu-cts/shared-mesa26-next/gpu-cts/summary.json
tail -f build/gpu-cts/shared-mesa26-next/gpu-cts/Xorg.log
```

The manifest is the API identity. A passing enumeration case is not an API
claim. A result is official only when the required QPA artifacts are complete
and the runner records a complete session.

### Stop safely

Before stopping anything:

1. resolve the exact development VM name;
2. inspect the exact PID and full command line;
3. confirm its cache and storage paths are under this repository;
4. stop that one session cleanly;
5. leave any process using `/Users/joshua/Library/Caches/ccx3` alone.

Do not use broad `pkill`, `killall`, or name-only matching. Other vmsh, ccvm,
terminal, and production sessions are expected to be present.

## Artifact and disk policy

Disk space is tight and can change while other terminals run. Check `df -h .`
before image import, compilation, or a long capture.

Preserve these artifacts:

```text
build/gpu-cts/vmsh-gpu-cts-mesa26-gl41.docker.tar
build/gpu-cts/shared-mesa26-gl41-88/gpu-cts/mesa26/
build/gpu-cts/shared-mesa25/gpu-cts/
build/gpu-cts/texture-full.cap
build/gpu-cts/texture-pot.cap
build/gpu-cts/fbo-full.cap
build/gpu-cts/vertex-arrays-full.cap
build/gpu-cts/clipping-full.cap
```

The largest current repo-local items include approximately:

| Path | Size | Policy |
| --- | ---: | --- |
| `build/gpu-cts/cache` | 995 MiB | Current OCI/runtime cache; keep while used |
| `build/gpu-cts/texture-full.cap` | 273 MiB | Keep until a smaller equivalent replay exists |
| Mesa 26 image archive | 226 MiB | Keep and checksum |
| `build/gpu-cts/run` | 217 MiB | Rebuildable binaries |
| Mesa 26 GL 4.1 result tree | varies | Current authoritative evidence; keep |
| Mesa 25 result tree | 134 MiB | Historical ES2 evidence; keep |

Safe cleanup candidates, after confirming no development build uses them, are:

- `build/go-cache` and other explicitly repo-local Go build caches;
- obsolete repo-local Go module caches;
- superseded development binaries;
- incomplete result trees that are neither authoritative nor the active resume
  target;
- duplicate OCI imports when the retained archive can recreate them;
- obsolete E2E scratch images.

Go module downloads are often read-only. If removing an explicitly
repo-local module cache, its files may need user-write permission first. Never
clean the global Go cache or any production vmsh/cc cache for this task.

Material cleanup is not directly recoverable even when artifacts are
reproducible. Record what was removed and which retained archive can recreate
it.

## Commit and PR procedure

Do not commit or push merely because a long-running system reached the end of a
shift. Commit when a coherent, tested capability batch is ready or when the
user explicitly asks for publication.

When publication is requested, the dependency order is:

1. inspect the cc diff and ensure it contains only intended GPU work;
2. run cc tests and `git diff --check`;
3. commit inside `cc` on `vmsh-virtio-gpu-3d`;
4. push `cc` to `origin/vmsh-virtio-gpu-3d`;
5. return to the vmsh root;
6. stage the updated cc submodule pointer, `GPU_PLAN.html`, `images/gpu-cts`,
   this report, and any other intentional root changes;
7. run root tests, runner syntax validation, and `git diff --check`;
8. commit on `agent/virtio-gpu-darwin`;
9. push to `origin/agent/virtio-gpu-darwin`;
10. confirm draft PR #244 shows both the root changes and the new cc commit.

Never commit a root submodule pointer to a cc commit that has not been pushed.
Never force-push without explicit authorization.

## Completion criteria for the current milestone

The OpenGL 4.1 conformance milestone is complete in the working tree:

- Mesa 26.1.6 creates a real GL 4.1 core context without a version override;
- every exported `KHR-GL41.*` case has one complete structured result;
- every applicable case passes, with zero failures and zero missing/extra
  cases;
- all QPAs end cleanly and the runner bounds CTS/native-driver lifetime;
- renderer behavior tests, full Go tests, vet, script syntax, and diff checks
  pass.

Publication and broader application/Piglit regression runs are follow-on work,
not evidence gaps in the completed GL 4.1 case corpus.

The end goal is not a high version string. It is a first-party, debuggable,
truthful guest graphics stack that runs real applications correctly and can be
advanced deliberately toward modern APIs.

## First response checklist for the next system

```text
[ ] Read AGENTS.md and this report completely.
[ ] Confirm vmsh, cc, and Gowin branches and commits.
[ ] Confirm the published GL 4.1 boundary and cc pointer before new edits.
[ ] Check disk space and exact active VM/process command lines.
[ ] Run go test ./... and go vet ./... in both root and cc.
[ ] Verify the authoritative Mesa 26 manifest, summary, and exact case counts.
[ ] Use run-gl41-cts; never substitute a monolithic CTS process.
[ ] Confirm glxinfo selects /opt/mesa 26.1.6 before accepting a new run.
[ ] Replay existing captures before spending a VM boot on visual regression.
[ ] Preserve manifests, QPAs, summaries, checksums, and exact renderer identity.
[ ] Commit cc before updating the root submodule pointer when publication is requested.
```
