# vmsh v0.7.3

## Highlights

- Unified SquadVM and NeurodeskAppX on one shared desktop runtime so display,
  input, clipboard, folder sharing, startup, and update fixes reach both apps.
- Fixed shared-folder rename and directory-refresh behavior that could leave
  moved files visible in their original location.
- Improved macOS input and HiDPI behavior with working Caps Lock and
  normal-sized guest desktops on Retina displays.

## Desktop applications

- Brought NeurodeskAppX onto SquadVM's complete native desktop behavior,
  including configurable shared folders, guest cursors, startup diagnostics,
  minimize/restore recovery, and release notifications.
- Kept product configuration independent, including VM images, storage paths,
  SSH identities, update assets, branding, and distinct SquadVM purple and
  Neurodesk green themes.
- Sized guest desktops in logical display points while keeping the native UI at
  full backing resolution, avoiding tiny controls and text on HiDPI hosts.
- Reconciled host and guest clipboard generations to prevent concurrent
  pasteboard updates from overwriting newer content.
- Resolved each guest SSH user's actual primary group and surfaced clean guest
  errors instead of internal command-stream framing.
- Simplified image startup to one download progress bar with indexing shown as
  concurrent status.

## Reliability

- Forwarded a complete Caps Lock press to guests from macOS modifier-change
  events.
- Restarted shared-folder directory enumeration from a fresh cursor and
  hardened rename replacement semantics, preventing stale entries and
  duplicate-looking files after moves.

## Release artifacts

- SquadVM for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- vmsh command-line builds for all supported release platforms.
- SHA-256 checksums for every published artifact.
