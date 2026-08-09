# GPU acceleration progress and handoff

Last updated: 2026-08-09 (Australia/Brisbane)

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

Do not raise a capset field merely to make Mesa report a newer version. A
capability is advertised only after the decoder, host execution, resource
transfer, shader translation, and behavior tests all support it.

## Repository and remote state

### vmsh root

- Working directory: `/Users/joshua/dev/projects/vmsh`
- Branch: `agent/virtio-gpu-darwin`
- Last application-workload commit: `549c007f3a3e6b0e34be9fd05124ec30c4ebc178`
- Remote branch: `origin/agent/virtio-gpu-darwin`
- Draft PR: [tinyrange/vmsh#244](https://github.com/tinyrange/vmsh/pull/244)
- PR title: `Add Darwin guest GPU acceleration`
- PR base: `main`

This report is published in the next GPU conformance commit after `549c007`.
After that commit is pushed, the root branch and remote branch should agree and
the working tree should be clean. The publication commit contains:

```text
GPU_PLAN.html
GPU_PROGRESS.md
cc -> 219c723
images/gpu-cts/
```

The pushed GPU history at the root is:

```text
(current) Advance GPU conformance work
549c007 Add Firefox WebGL GPU fixture
bf0a13e Run OpenArena through native GPU acceleration
5a6fc8c Run full glmark2 through native Glass
7c02010 Add Darwin guest GPU acceleration
```

### cc submodule

- Path: `cc`
- Branch: `vmsh-virtio-gpu-3d`
- HEAD: `219c723` (`Advance Darwin VirGL conformance support`)
- Tracking branch: `origin/vmsh-virtio-gpu-3d`
- The branch and its remote are synchronized at that commit.
- Working tree: clean at publication time.

Committed GPU history at the cc boundary is:

```text
219c723 Advance Darwin VirGL conformance support
f62caea Run Firefox WebGL through Darwin VirGL
ac116cb Run OpenArena through Darwin VirGL
f47e443 Complete glmark2 and native scanout
d3ed559 Add Darwin VirGL renderer
```

The modern conformance commit changes:

```text
M cmd/virgl-replay/main.go
M internal/virgl/capset.go
M internal/virgl/capset_test.go
M internal/virgl/framebuffer_darwin_test.go
M internal/virgl/gl_darwin.go
M internal/virgl/host_darwin.go
M internal/virgl/renderer.go
M internal/virgl/renderer_test.go
M internal/virgl/replay_darwin.go
M internal/virgl/tgsi.go
M internal/virgl/tgsi_darwin_test.go
M internal/virgl/tgsi_test.go
M internal/virtio/gpu.go
```

Commit `219c723` contains approximately 3,435 insertions and 763 deletions. It
was pushed before the root submodule pointer was updated, so the root commit
references a durable cc commit.

### Gowin submodule

- Path: `gowin`
- Branch: `vmsh-gpu-shared-context`
- HEAD: `25d29bd2a831417328ea1abfdce1c50799851390`
- Tracking branch: `origin/vmsh-gpu-shared-context`
- Working tree: clean

Relevant Gowin commits are:

```text
25d29bd Expose native OpenGL share groups
b8857c7 Add Darwin shared OpenGL contexts
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

## Modern renderer work in cc commit 219c723

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

This is a compatibility implementation using native uniform arrays, not yet a
native GL uniform-buffer-object binding path. It is behaviorally useful for the
current TGSI protocol but should be revisited when CTS coverage demands native
UBO reflection, binding, size, or update semantics.

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

The relevant committed capset values are:

| Capability | Advertised value | Important qualification |
| --- | ---: | --- |
| GLSL level | 150 | Does not by itself make Mesa expose GL 3.x |
| Max 2D texture size | 4096 | Measured/implemented bound |
| Max texture-array layers | 256 | 2D array path implemented |
| Max render targets | 4 | MRT clear behavior tested |
| Max uniform blocks | 12 | TGSI constant-block compatibility path |
| Max streamout buffers | 0 | Transform feedback is not implemented |
| Max samples | 1 | Multisampling is not implemented |
| Max TBO size | 0 | Texture buffer objects are not implemented |
| Max viewports | 1 | Multi-viewport is not implemented |
| Texture gather components | 0 | Gather is not implemented |
| Dual-source render targets | 0 | Not implemented |
| Host feature fallback version | 0 | Intentionally disabled |

The capset advertises only a small set of color, depth/stencil, and vertex
formats. This is currently the principal reason modern Mesa remains at OpenGL
2.0/GLES 2.0 despite arrays, MRT, instancing, and uniform buffers being present.

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

Artifacts:

```text
build/gpu-cts/shared-mesa26-modern2/gpu-cts/
```

Identity:

```text
renderer: virgl
OpenGL:   2.0 Mesa 26.1.6
GLSL:     1.20
```

The GLES3 and GLES3.1 probes currently fail with:

```text
FATAL ERROR: Matching EGL config not found
```

The GL30, GL31, GL32, GL33, GL40, and GL41 probes currently fail context
creation with:

```text
FATAL ERROR: Got EGL_BAD_MATCH: eglCreateContext()
```

That is an honest API-ceiling result, not a harness failure. Earlier
`CTS-Configs` desktop probes were misleading because they could pass without a
relevant WGL configuration. The runner now executes real `KHR-GL30.info.*`
through `KHR-GL41.info.*` cases through EGL, so passing requires creation of the
requested context.

The modern result tree has only 12 of 45 ES2 groups and must not be used as the
primary ES2 baseline. It exists mainly as the current Mesa 26 context-probe
record.

### Current image archive

Local archive:

```text
build/gpu-cts/vmsh-gpu-cts-mesa26-modern.docker.tar
```

Size is approximately 226 MiB. SHA-256:

```text
98985f3304cc11a2273a700df3f1f55bf1520d62a460b180c234d20246283f66
```

A matching archive was built natively on `astra-pi5` at:

```text
/home/joshua/vmsh-gpu-cts-mesa26-modern.docker.tar
```

Verify the checksum before replacing the local archive.

## Fresh verification performed for this handoff

The following passed on 2026-08-09 with a repository-local Go build cache:

```sh
cd /Users/joshua/dev/projects/vmsh/cc
mkdir -p ../build/go-cache
GOCACHE="$PWD/../build/go-cache" \
  go test ./internal/virgl ./internal/virtio -count=1
git diff --check

cd /Users/joshua/dev/projects/vmsh
mkdir -p build/go-cache
GOCACHE="$PWD/build/go-cache" \
  go test ./cmd/squadvm ./internal/... -count=1
git diff --check
sh -n images/gpu-cts/run-gpu-cts.sh
```

Observed package results included:

```text
ok j5.nz/cc/internal/virgl
ok j5.nz/cc/internal/virtio
ok github.com/tinyrange/vmsh/cmd/squadvm
ok github.com/tinyrange/vmsh/internal/...
```

This was not a new live VM or full CTS run. The modern capset still reports GL
2.0, so repeatedly rebooting the fixture after each isolated field change would
not provide new information. Batch a coherent capability cluster, run behavior
tests, and then reboot the Mesa 26 probe image.

## Important behavior tests in cc commit 219c723

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

### Format coverage is the next primary blocker

Host texture allocation currently maps nearly every non-depth texture to
RGBA8, with only narrow BGRA transfer distinctions. The capset therefore
advertises only a small safe format set. Do not broadly add format bits until
allocation, upload, readback, framebuffer use, sampling, and relevant vertex
conversion are correct for each format.

Mesa 26 moved its format definitions. Use upstream files from the exact Mesa
release as the enumeration source:

```text
src/util/format/u_formats.h
src/util/format/u_format.yaml
```

Do not rely on recalled numeric `PIPE_FORMAT` values or an old Mesa header.

The first useful format cluster should cover the GLES3 gates, including at
least the required subset of:

- R8 and RG8;
- sRGB8 / sRGB8-alpha behavior;
- R/RG/RGBA half-float and float formats;
- signed normalized R/RG formats;
- packed 2_10_10_10 formats;
- depth32f where the Darwin host supports it;
- matching external format, component type, byte width, aspect, and swizzle.

A table-driven mapping should describe, for each supported pipe format:

```text
native internal format
transfer external format
transfer data type
bytes per pixel/block
color/depth/stencil aspects
channel order and swizzle
renderability
sampler support
vertex support where applicable
```

Add real pixel/vertex behavior tests before setting the corresponding capset
mask bits. Xorg/Mesa logs previously showed `GL_R8` and 2_10_10 formats falling
back, which is direct evidence that this cluster matters.

### Transform feedback / streamout is absent

`max_streamout_buffers` remains zero. Mesa's GLES3 version computation requires
transform feedback.

The protocol work is expected to include:

- VirGL streamout-target objects;
- `SET_STREAMOUT_TARGETS` command handling;
- resource offset/size validation and lifetime;
- parsing the shader stream-output metadata rather than rejecting nonzero
  stream-output declarations;
- calling native transform-feedback varying setup before program link;
- buffer-range binding;
- begin/end transform feedback around supported draws;
- behavior tests that read back captured vertices.

Confirm all payload layouts against the matching VirGL/Mesa protocol source
before implementation. Do not infer offsets from an application trace alone.

### Queries are absent

Modern Mesa also needs query objects and result semantics. Implement bounded
query creation/destruction, begin/end, result retrieval, wait/no-wait behavior,
and guest-buffer result writes. Start with the exact query types required by
the GLES3/GL3.0 version gate and add behavior-level tests.

### Later GL features are intentionally absent

The following are later rungs, not reasons to overstate the current capset:

- geometry shaders and layered rendering;
- multisampling and sample resolves;
- full sampler-object and GLSL 3.30 behavior;
- texture buffer objects;
- tessellation shaders;
- indirect draw;
- cube-map arrays;
- sample shading;
- FP64;
- multiple viewports and texture gather.

### Program-memory behavior needs a long-run check

The 128-entry LRU bounds program objects, but the official monolithic ES2
runner has not yet completed cleanly under the new policy. Once a coherent
modern feature batch is committed, run a long memory observation and record:

- host resident memory over time;
- guest memory and OOM state;
- number of cached host programs;
- whether all official QPAs end in `#endSession`;
- whether macOS terminates any development process.

Do not reintroduce compile-context rotation or unconditional `glFinish` as a
substitute for understanding ownership.

### Darwin OpenGL has a hard ceiling

The NSOpenGL backend can target OpenGL 4.1 core / GLSL 4.10, but not newer
desktop OpenGL. After honest 4.1 conformance, modern Steam/Proton workloads need
a Vulkan-over-Metal design, likely with newer OpenGL implemented over Vulkan.
Do not encode fake GL 4.2+ capabilities into the current backend.

## Prioritized continuation plan

### Shift 0: preserve and reproduce the current state

1. Read this report and `GPU_PLAN.html` M6A.
2. Confirm all three branch/commit identities listed above.
3. Confirm the working tree is clean before editing.
4. Run the fresh verification commands.
5. Do not rebase or update submodules until commit `219c723` and its root
   pointer are confirmed on their remotes.
6. Check free disk space and active processes before building or starting a VM.

### Shift 1: implement a truthful modern format table

1. Obtain `u_formats.h` and `u_format.yaml` from Mesa 26.1.6.
2. Map the exact GLES3-required formats to Darwin OpenGL storage and transfer
   tuples.
3. Refactor texture allocation, transfer-to-host, transfer-from-host,
   framebuffer attachment, readback, and vertex interpretation to use the same
   checked format descriptor.
4. Add behavior tests for every newly advertised format family.
5. Add only the proven format mask bits.
6. Run cc tests and replay representative OpenArena/Firefox captures to catch
   regressions without booting a VM.

### Shift 2: implement transform feedback

1. Verify protocol object and command layouts from upstream source.
2. Implement bounded streamout objects and resource lifetime.
3. Parse shader stream-output metadata.
4. Bind native transform-feedback varyings and buffers.
5. Execute and read back a minimal captured vertex stream.
6. Add the first nonzero streamout cap only after the pixel/buffer behavior
   test passes.

### Shift 3: implement required query semantics

1. Identify the smallest GLES3/GL3.0 query set from current Mesa's version and
   extension computation.
2. Implement lifecycle and result transfer with strict validation.
3. Add behavior tests for result values and wait modes.
4. Advertise only the supported query subset.

### Shift 4: rerun the modern context ladder

After formats, transform feedback, and queries form a coherent cluster:

1. rebuild the cc/SquadVM development binaries;
2. start the existing Mesa 26 image with a unique development name and
   repo-local cache/storage;
3. inspect `manifest.json` first;
4. inspect GLES3, GLES3.1, and GL30-GL41 probe QPAs/logs;
5. if a new context is exposed, begin that API's small info/capability slices;
6. only then launch the relevant long CTS corpus;
7. preserve every structured result and exact renderer identity.

If Mesa still reports GL2/GLES2, inspect Mesa's version computation and Xorg
logs for the next missing implemented capability. Never force an override just
to begin the suite.

### Shift 5 and beyond: climb one API rung at a time

- Earn GLES3 and GL3.0/3.1 with passing feature slices.
- Add GL3.2/3.3 geometry, multisample, sampler, and GLSL behavior.
- Add GL4.0 tessellation, indirect draw, cube arrays, sample shading, and FP64.
- Complete the remaining GL4.1 slices.
- Run selected Piglit coverage alongside Khronos CTS.
- Keep the full existing application corpus green at every rung.

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
  --output type=docker,dest=/home/joshua/vmsh-gpu-cts-mesa26-modern.docker.tar \
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
  -name gpu-cts-mesa26-dev-01 \
  -cache-dir "$PWD/build/gpu-cts/cache" \
  -storage "$PWD/build/gpu-cts/shared-mesa26-next" \
  -memory-mb 6144 \
  "docker-archive:$PWD/build/gpu-cts/vmsh-gpu-cts-mesa26-modern.docker.tar"
```

Use at least 6144 MiB. A 4 GiB official runner was killed by the guest OOM path
before it could produce complete QPAs.

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

Disk space is tight and can change while other terminals run. At report time
the host had approximately 6.2 GiB free. Check `df -h .` before image import,
compilation, or a long capture.

Preserve these artifacts:

```text
build/gpu-cts/vmsh-gpu-cts-mesa26-modern.docker.tar
build/gpu-cts/shared-mesa25/gpu-cts/
build/gpu-cts/shared-mesa26-modern2/gpu-cts/
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
| Mesa 25 result tree | 134 MiB | Authoritative evidence; keep |
| Mesa 26 modern probe tree | 28 MiB | Current API-ceiling evidence; keep |

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

M6A is complete only when all of the following are true:

- Mesa 26 creates GLES3 and requested GL3.x/GL4.x contexts without a version
  override;
- the GLES3, GL30, GL31, GL32, GL33, GL40, and GL41 CTS profiles pass their
  required slices;
- selected Piglit coverage passes;
- the existing kmscube, glmark2, OpenArena, and Firefox workloads remain
  correct and performant;
- the official GLES2 must-pass artifacts complete without unsupported-command
  suppression, environment-gated tests, or a fake capset;
- long-run host program and VM memory are bounded;
- native shared-texture scanout remains the normal presentation path.

The end goal is not a high version string. It is a first-party, debuggable,
truthful guest graphics stack that runs real applications correctly and can be
advanced deliberately toward modern APIs.

## First response checklist for the next system

```text
[ ] Read AGENTS.md and this report completely.
[ ] Confirm vmsh, cc, and Gowin branches and commits.
[ ] Confirm the published working tree is clean before making new changes.
[ ] Check disk space and exact active VM/process command lines.
[ ] Run the fresh targeted test commands with repo-local caches.
[ ] Inspect Mesa 26.1.6 u_formats.h and u_format.yaml.
[ ] Implement and behavior-test the first truthful GLES3 format cluster.
[ ] Keep streamout/query caps at zero until their command paths exist.
[ ] Replay existing captures before spending a VM boot on visual regression.
[ ] Reboot the Mesa 26 probe fixture only after a coherent capability batch.
[ ] Preserve manifests, QPAs, summaries, checksums, and exact renderer identity.
[ ] Commit cc before updating the root submodule pointer when publication is requested.
```
