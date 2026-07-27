# vmsh v0.5.0

## Highlights

- Added **NeurodeskAppX**, a native Neurodesktop application for Linux AMD64,
  Windows AMD64, and Apple Silicon macOS. Opening the app starts Neurodesktop in
  a resizable Gowin window with keyboard, pointer, and bidirectional clipboard
  support; VNC remains available as an explicit fallback.
- Added a polished dark-mode startup experience with the official Neurodesk
  artwork, HiDPI-aware rendering, parallel OCI downloads, speed and ETA
  reporting, preparation progress, and a clean transition to the ready desktop.
- Added session-scoped MCP workflows for creating and controlling isolated VMs,
  including persistent shell contexts, asynchronous commands, cancellation,
  binary-safe artifacts, and VM-to-VM copying without exposing the host.
- Added transactional `@upgrade`, which downloads the appropriate GitHub
  release, verifies its checksum and build identity, and atomically updates both
  the shell and daemon with rollback on failure.

## NeurodeskAppX

- Uses the published multi-architecture Neurodesktop image and persists the
  user's home on Linux and macOS.
- Exposes the host's `~/neurodesktop-storage` directory inside the guest at
  `/neurodesktop-storage`, with the matching home-directory link configured by
  default.
- Supports passwordless sudo, systemd, cgroup v2, managed commands, CVMFS, and
  nested container workloads in the Neurodesktop guest.
- Includes native application packaging, signing, notarization, and Neurodesk
  icons on macOS, plus a GUI-subsystem executable and embedded icon on Windows.
- Boots through WHP on Windows with working graphics and safe display-input
  handling. Image expansion now uses the correct filesystem capacity check.
- Scales both the startup UI and guest viewport correctly on HiDPI Linux
  displays.

## vmsh

- Sparse startup snapshots now scale with populated guest memory on KVM and
  Hypervisor.framework. In measured tests, warm context activation fell to
  roughly 25 ms on KVM while large, lightly populated guests no longer consume
  their full configured RAM on disk.
- The new MCP security boundary limits credentials and VM access to the owning
  shell session, excludes host shares and trusted host calls, and cleans up
  resources when the session ends.
- Persistent MCP contexts preserve cwd, environment, aliases, functions, and
  exact terminal bytes while keeping command admission, output accounting, and
  cancellation bounded.
- Wrapped history and completion rendering now account for physical terminal
  rows, preventing long entries and completion lists from corrupting the prompt.
- Cross-platform archive extraction, process containment, daemon-state updates,
  and shell recovery were hardened for Linux, macOS, Windows, and BSD guests.

## Release artifacts

- `NeurodeskAppX` for Linux AMD64 and Windows AMD64.
- Signed and notarized `NeurodeskAppX.app` for Apple Silicon macOS.
- `vmsh` executables for Linux AMD64/ARM64, Windows AMD64/ARM64, and Apple
  Silicon macOS.
- SHA-256 checksums for every published artifact.

## Validation

- Passed the unit, race, vet, Staticcheck, OpenAPI, cross-platform build, guest
  payload, and real-KVM integration suites used by vmsh and cc.
- Verified the signed Apple Silicon app through native Neurodesktop boot,
  resize, keyboard, pointer, clipboard, persistent storage, CVMFS, and nested
  container workflows.
- Verified the Linux AMD64 app through native KVM boot and HiDPI desktop input,
  resize, storage, and guest scaling checks.
- Verified the Windows AMD64 release candidate through native WHP boot to the
  complete LXDE desktop with systemd as PID 1, cgroup v2 mounted, zero failed
  units, working managed commands, passwordless sudo, storage sharing, and
  Gowin display input.
