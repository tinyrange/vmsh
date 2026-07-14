# vmsh v0.3.0

## Added

- Added `vmsh --version` and `@version` with build identity metadata.
- Added Windows ARM64 WHP support and release artifacts.
- Added Linux AMD64 daemon memory balloon policy support.
- Added release notes as the GitHub Release body.

## Changed

- Completion menus now switch to a vertical layout on narrow terminals.
- Release and local helper builds now stamp `vmsh` with version metadata.
- Release binaries now use the embedded vmshd payload path directly.
- `install.sh` now supports Windows AMD64 and Windows ARM64 `.exe` assets.

## Fixed

- Fixed path completion escaping for shell metacharacters such as parentheses.
- Fixed terminal paste and path completion marker handling.
- Hardened completion and host shell lookup across platforms.
- Stabilized the persistent host interrupt unit test.
