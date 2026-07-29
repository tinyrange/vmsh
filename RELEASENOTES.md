# vmsh v0.6.2

## Highlights

- Added automatic SquadVM image and application update checks. Available
  updates appear as dismissible desktop notifications and in startup settings.
- Made SquadVM portable by default, keeping its cache beside the application.
  Existing system caches are reused, and system installs remain available.
- Added the vmsh landing page with current NeurodeskAppX, SquadVM, and vmsh
  downloads.

## SquadVM

- Added `pwninit` and refreshed the bundled Pwndbg dependencies.
- Added x86-64 execution on arm64 through QEMU, `binfmt_misc`, and the required
  amd64 runtime libraries.
- Added `qemu-gdb` for debugging x86-64 binaries with Pwndbg on arm64.
- Fixed Pwndbg ownership and dependency-cache warnings.
- Fixed desktop keyboard input reaching the guest console and terminating Xorg.
- Improved desktop readiness and startup behavior on arm64.

## Reliability

- OCI image downloads now resume after interruption and validate partial data
  before reuse.

## Release artifacts

- SquadVM is available for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX and vmsh artifacts are included for their supported platforms.
- Windows ARM64 SquadVM remains unsupported.
- SHA-256 checksums are included for every published artifact.
