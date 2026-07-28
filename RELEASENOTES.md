# vmsh v0.6.0

## Highlights

- Added **SquadVM**, a native Cyber Squad desktop application for Linux AMD64,
  Windows AMD64, and Apple Silicon macOS.
- Added a curated multi-architecture Kali image for SquadVM with XFCE, security
  tools, Binwalk 3, Burp Suite, persistent storage, host file sharing, and an
  isolated SSH service.
- Added image update checks before startup. SquadVM shows the compressed
  download size and time estimate, then lets the user update or start the
  installed image.

## SquadVM

- Uses a fully branded dark interface and Cyber Squad desktop with native
  resize, keyboard, pointer, and clipboard integration.
- Persists the guest home and maps `~/squadvm-shared` to `/shared`.
- Can configure loopback-only, key-based SSH access as `ssh squadvm` without
  exposing the guest service externally.
- Runs host virtualization, disk-space, and image preflight checks on first
  launch. Returning users remain on the compact startup screen unless an
  update, error, or explicit settings request needs attention.
- Allows Escape during startup to cancel cleanly and return to settings.

## Image delivery and persistence

- Resolves mutable OCI tags to manifest digests and reports the exact remaining
  compressed bytes before downloading.
- Downloads layers in parallel while streaming the active layer directly into
  the filesystem index. Separate download and indexing bars report bounded,
  independent progress.
- Reuses shared OCI cache content without duplicating cache state.
- Repairs affected persistent filesystem checkpoints containing dangling inode
  references and prevents new checkpoints from recording inconsistent
  namespaces.

## Release artifacts

- `SquadVM` for Linux AMD64 and Windows AMD64.
- Signed and notarized `SquadVM.app` for Apple Silicon macOS.
- `NeurodeskAppX` for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- `vmsh` executables for Linux AMD64/ARM64, Windows AMD64/ARM64, and Apple
  Silicon macOS.
- SHA-256 checksums for every published artifact.

## Validation

- Passed the focused SquadVM, OCI, OpenAPI, persistent filesystem, and race
  suites.
- Built the complete local release payload, including the signed-development
  SquadVM executable on macOS.
- The release workflow verifies Linux and Windows desktop payloads, Windows GUI
  resources, macOS hardened-runtime signatures and hypervisor entitlements,
  Developer ID notarization, stapling, and artifact checksums.
