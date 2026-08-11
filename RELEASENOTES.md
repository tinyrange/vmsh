# vmsh v0.8.1

## Highlights

- Fixed NeurodeskAppX startup on Linux when a persistent home contains entries
  created by an early-boot root process.
- Added an on-demand JupyterLab launcher to NeurodeskAppX, with the browser
  opening only after the guest reports that JupyterLab is ready.
- Recovered daemon watchdog leases automatically after host sleep, preventing
  repeated warnings from corrupting an interactive vmsh prompt.

## NeurodeskAppX

- Mapped persistent-home entries to the Neurodesktop session account so
  `/home/jovyan` remains writable as UID 1000/GID 100, including when existing
  entries were originally owned by root.
- Kept guest callback configuration alive beyond the systemd readiness window,
  avoiding false startup failures while a healthy Linux VM is still booting.
- Forwarded JupyterLab through a random loopback-only host port and replaced
  host-side readiness polling with a tokenized guest-to-host callback.
- Reopened the existing JupyterLab session on later desktop-icon clicks without
  starting another server.
- Updated the Glass-only Neurodesktop image from the upstream Neurocontainers
  recipe, retained passwordless sudo, and published conventional OCI and
  enhanced eStargz variants for AMD64 and ARM64.

## Reliability

- Recreated expired daemon watchdog leases after sleep and continued feeding
  the replacement lease without emitting a warning when recovery succeeded.
- Reported an unrecoverable watchdog episode at most once and stopped background
  lease diagnostics from writing asynchronously into the interactive editor.
- Released the current replacement lease when the shell exits.

## Release artifacts

- SquadVM for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- vmsh command-line builds for all supported release platforms.
- SHA-256 checksums for every published artifact.
