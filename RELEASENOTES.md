# vmsh v0.7.1

## Highlights

- Redesigned SquadVM's native setup and startup experience for clearer,
  denser, responsive presentation across display sizes.
- Fixed Windows keyboard capture, scaling, pre-start resizing, and desktop
  wallpaper refresh behavior in SquadVM.
- Added shared-memory domains for long-lived Linux KVM system contexts.

## SquadVM

- Added a smoothly scrolling startup checklist, readable proportional text,
  supplied SquadVM branding, hover and keyboard states, vector checkmarks, and
  a shader-rendered background.
- Forwarded Windows and Alt system-key chords without duplicating ordinary
  typing or leaving modifiers stuck, and prevented Ctrl+C from terminating the
  guest X server.
- Started the guest framebuffer at the resized window dimensions and refreshed
  XFCE through its live session bus so the wallpaper fills the desktop.
- Made Escape wait for orderly VM teardown, avoiding the previous device and
  guest-memory shutdown race.
- Declared macOS 15 as the minimum for SquadVM and NeurodeskAppX and added an
  actionable message for direct launches on older macOS versions.

## vmsh

- Added `--shmem domain,physaddr` for selecting multiple persistent Linux KVM
  systems into the same shared-memory domain.
- Preserved shared-memory configuration through status, context selection, and
  restart flows while rejecting incompatible reuse of a running system.

## Release artifacts

- SquadVM for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- vmsh command-line builds for all supported release platforms.
- SHA-256 checksums for every published artifact.
