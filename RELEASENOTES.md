# vmsh v0.6.1

## Highlights

- Added Windows AMD64 support for SquadVM with four virtual CPUs.
- Added multi-vCPU support to the Windows Hypervisor Platform backend.
- Improved Windows ConPTY test reliability.

## Reliability

- Guest-initiated poweroff now ends the VM session cleanly and releases active
  commands.
- Fixed a virtual socket shutdown leak that could make later VM boots take much
  longer.
- Stabilized terminal input, resize, exit, and alternate-screen behavior tests
  without depending on PowerShell startup time.

## Release artifacts

- `SquadVM_v0.6.1_windows_amd64.exe` is a native Windows GUI application with
  the SquadVM icon and embedded cc runtime.
- SquadVM remains available for Linux AMD64 and Apple Silicon macOS.
- Windows ARM64 SquadVM is not supported in this release.
- SHA-256 checksums are included for every published artifact.
