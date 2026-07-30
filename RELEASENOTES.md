# vmsh v0.7.0

## Highlights

- Added native high-resolution trackpad scrolling to SquadVM and
  NeurodeskAppX, including fractional movement and calibrated macOS
  sensitivity.
- OCI layers now download and index in one bounded streaming pipeline, reducing
  temporary storage and allowing indexing to begin during downloads.
- Added the vmsh downloads website with automatically refreshed release data.

## SquadVM

- Fixed XFCE startup crashes by isolating desktop processes from the unstable
  GVFS backend without breaking trusted launchers.
- Enabled Firefox's native XInput 2 path for smooth scrolling.
- Improved ARM64 startup validation for x86-64 QEMU and binfmt support.
- Preserved the shared-folder desktop shortcut during clean image migrations.

## Release artifacts

- SquadVM for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- vmsh command-line builds for all supported release platforms.
- SHA-256 checksums for every published artifact.
