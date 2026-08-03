# vmsh v0.7.2

## Highlights

- Added a complete SquadVM boot console and actionable, scrollable startup
  diagnostics.
- Improved Windows display fidelity and desktop integration with guest cursors,
  native-resolution rendering, and reliable minimize/restore behavior.
- Made the shared host folder selectable from the startup screen with native
  directory pickers on Windows, macOS, and Linux.

## SquadVM

- Showed complete preflight and startup failures instead of truncating them,
  and added a full boot console with ANSI colors and terminal control handling.
- Kept serial capture off the VM's boot path, retained the console until a
  complete desktop frame arrives, and removed duplicated serial lines.
- Routed systemd manager and service status to the boot console for more useful
  failure diagnostics.
- Rendered the guest directly at the Windows client area's physical resolution
  and adopted the guest-provided cursor image and hotspot as the native cursor.
- Preserved the display mode while minimized and requested a complete
  framebuffer after restoration, avoiding a blank desktop until manual resize.
- Enabled XFCE window tiling at the screen edges and bound the captured
  Windows/Super key to the applications menu.
- Added a persistent shared-folder setting and modern directory picker so any
  host folder can be selected before boot.
- Used a writable application-specific cache for portable macOS installations.

## Release artifacts

- SquadVM for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- vmsh command-line builds for all supported release platforms.
- SHA-256 checksums for every published artifact.
