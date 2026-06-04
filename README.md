# vmsh

`vmsh` is an interactive shell for running host commands and VM-backed Linux
commands from one prompt. It is a product shell around the `ccvm` daemon: OCI
images become selectable command contexts, and `cc` remains the underlying VM
runtime, image importer, and debug command repository.

The repository is intended to be published as `github.com/tinyrange/vmsh`.

## What It Does

- Runs ordinary shell commands on the host by default.
- Switches to VM-backed command execution with `@<image>`.
- Keeps host and guest shell state warm when possible, so `cd`, aliases,
  functions, and exported variables survive across commands.
- Mounts the host root into guests at `/host` and mirrors the current host
  directory into the guest working directory.
- Supports named VMs, memory/CPU sizing, sudo/root execution, networking
  toggles, and architecture-specific image aliases.

Example session:

```sh
@alpine
cat /etc/alpine-release

@ubuntu:24.04 --vm work --memory 2g --cpus 4
python3 --version

@host git status
@alpine --no-network sh -lc 'uname -m && whoami'
@ --sudo apk add curl
```

## Requirements

- Go 1.25 or newer, matching `go.mod`.
- A checked-out `cc` submodule.
- A supported virtualization host when running VM commands:
  - `linux/amd64` with KVM and user access to `/dev/kvm`.
  - `windows/amd64` with Windows Hypervisor Platform enabled.
  - `darwin/arm64` with Hypervisor.framework.
  - `linux/arm64` with KVM.
- Network access when downloading kernels or pulling OCI images.

Fast parser, shell-state, and client tests do not need VM support.

## Repository Layout

- `cmd/vmsh`: the `vmsh` shell and tests.
- `cc`: git submodule containing `ccvm`, VM backends, image import, and the
  lower-level `cc` CLI.
- `tools/build_vmsh.sh`: local Unix build helper for `cc`, `ccvm`, and `vmsh`.
- `tools/run_vmsh.sh`: local development runner that builds guest init payloads,
  builds `ccvm` from the submodule, builds `vmsh`, signs `ccvm` on macOS, and
  launches `vmsh -ccvm build/vmsh/ccvm`.
- `.github/workflows/ci.yml`: portable Go tests plus opt-in live VM smoke tests
  for KVM and WHP runners.
- `.github/workflows/release.yml`: tag-triggered single-binary releases for
  Linux, Windows, and signed macOS ARM64.

## Getting Started

Clone with submodules:

```sh
git clone --recurse-submodules https://github.com/tinyrange/vmsh.git
cd vmsh
```

If the repository was cloned without submodules:

```sh
git submodule update --init --recursive
```

Run the shell locally:

```sh
./tools/run_vmsh.sh
```

Run an existing `ccvm` binary instead:

```sh
go build -o build/vmsh/vmsh ./cmd/vmsh
./build/vmsh/vmsh -ccvm /path/to/ccvm
```

Run a non-interactive script, which is useful for CI and smoke tests:

```sh
./tools/build_vmsh.sh
./build/vmsh/cc -ccvm ./build/vmsh/ccvm pull alpine ./cc/fixtures/alpine.simg

cat > /tmp/vmsh-smoke <<'EOF'
@alpine --vm smoke --memory 256 --no-network sh -lc 'whoami; uname -m'
EOF

./build/vmsh/vmsh -ccvm ./build/vmsh/ccvm -script /tmp/vmsh-smoke
```

## Command Syntax

`vmsh` treats ordinary lines as commands in the current context. Lines beginning
with `@` are `vmsh` control lines:

```sh
@<oci-image> [vmsh-options] [--] [command...]
```

Common forms:

```sh
@alpine                         # select an image; VM starts lazily
@alpine uname -a                # run one command in alpine
@host pwd                       # run one command on the host
@ --vm work --memory 4g         # update the current VM context
@ --sudo whoami                 # run as root in the current VM
@jobs                           # list background jobs
@status                         # show selected context and VM status
@stop --vm work                 # stop a named VM
```

Supported options:

```sh
--vm <id>
--cwd <guest-path>
--user <user>
--sudo
--memory <n|nM|nG>
--memory-mb <n>
--cpus <n>
--network
--no-network
--nested
--no-nested
--arch <amd64|arm64>
```

Use `--` when the guest command itself begins with an option:

```sh
@alpine -- --help
```

## Development

Run top-level tests:

```sh
go test ./...
```

Run the embedded `cc` test suite:

```sh
(cd cc && go test ./...)
```

Run tests that intentionally avoid live VM booting:

```sh
go test -short ./...
(cd cc && go test -short ./...)
```

Run selected Linux KVM boot probes on a `linux/amd64` host with `/dev/kvm`:

```sh
cd cc
CCX3_KVM_BOOT=1 go test ./internal/hv/kvm ./internal/vm \
  -run 'Test(KernelBootSerial|InitramfsBootReadyMarker|RuntimeBackendRunCommand)$' \
  -count=1 -v
```

Run selected Windows Hypervisor Platform probes on a `windows/amd64` host:

```powershell
cd cc
$env:CCX3_WHP_BOOT = "1"
go test ./internal/hv/whp ./internal/vm `
  -run 'Test(WindowsRuntimeBackendRunCommand|RunManagedExecWithAlpineRootFS)$' `
  -count=1 -v
```

## Continuous Integration

The workflow is split by capability:

- Hosted `ubuntu-24.04-arm` and `macos-15` jobs run `go test -short` for this
  module and for the `cc` submodule. These jobs cover code paths that should not
  require live VM support.
- A hosted `ubuntu-24.04` job runs normal Go tests, makes `/dev/kvm`
  accessible to the runner user, enables `CCX3_KVM_BOOT=1` for selected live
  boot probes, and executes a `vmsh` script against the tracked Alpine SIMG
  fixture.
- A hosted `windows-2025` job checks WHP availability and boots an Alpine
  kernel far enough to observe serial output. The current `cc` submodule notes
  that managed guest command progress on GitHub Windows runners is not yet
  reliable, so the full guest command smoke currently runs on Linux AMD64.

The live jobs use the checked-in `cc/fixtures/alpine.simg` fixture so they can
boot and run simple guest commands without depending on an external image pull.

## Releases

Pushing a version tag matching `v*` runs the release workflow:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The workflow builds one standalone `vmsh` binary per target:

- `linux/amd64`
- `linux/arm64`
- `darwin/arm64`

Windows release binaries are temporarily disabled while Windows-specific
single-binary validation is handled on a Windows system.

Release binaries are built with `embed_ccvm` and `embed_guestinit`. That compiles
the `ccvm` daemon entrypoint into the same Go executable as `vmsh` and embeds
the static Linux guest init payloads for amd64 and arm64 guests. At runtime,
`vmsh` re-execs itself with `VMSH_INTERNAL_CCVM=1` when it needs to start the
daemon, so release assets do not need a `ccvm` sidecar.

The macOS binary is built on `macos-15` and codesigned with the Hypervisor
entitlement from `tools/entitlements.xml`. Configure these repository secrets
for Developer ID signing and notarization:

- `MACOS_CERTIFICATE`: base64-encoded `.p12` signing certificate.
- `MACOS_CERTIFICATE_PWD`: password for the `.p12` certificate.
- `MACOS_DEVELOPER_ID`: Developer ID Application identity. The workflow also
  accepts `DEVELOPER_ID` for compatibility with older `cc` release settings.
- `APPLE_ID`: Apple ID used by `notarytool`.
- `APPLE_ID_PASSWORD`: app-specific password for `notarytool`, or
  `@keychain:<profile>` to use a preconfigured notary keychain profile.
- `TEAM_ID`: Apple Developer Team ID.

The workflow signs the binary with hardened runtime, submits a temporary ZIP
containing that binary to Apple's notary service, and publishes the single
signed binary as the release asset.
