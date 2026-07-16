# vmsh v0.4.0

## Added

- Added native PTY ownership, terminal emulation, session snapshots, input
  driving, and asciicast recording, plus the experimental `@mux` frontend.
- Added secure read-only remote group inventory and context selection with
  `@connect`.
- Added profile-bound trusted VM-to-host calls with explicit grants,
  revocation, audit records, and `@trust`/`@permissions` controls.
- Added custom VM-kernel selection and design documentation for live migration.
- Added an explicit hard-interrupt path: after two `Ctrl+C` presses arm the
  command, `Ctrl+\` terminates an unresponsive host, VM, or SSH command.

## Changed

- Reworked vmshd lifecycle ownership, authenticated daemon discovery, startup
  serialization, watchdog leases, session publication, and cleanup recovery.
- Made terminal streams exclusive and lossless, observers read-only, resize
  propagation explicit, and event gaps visible to clients.
- Streamed guest and SSH downloads into extraction instead of buffering whole
  archives, while preserving atomic destination publication.
- Updated the embedded cc runtime with reliable startup snapshots, high-density
  copy-on-write VM clones, bounded parallel fleet behavior, networking fixes,
  and Linux, NetBSD, FreeBSD, and OpenBSD guest hardening.
- Added continuous gofmt, `go vet`, Staticcheck correctness, race, OpenAPI,
  cross-platform, and KVM integration gates to CI.

## Fixed

- Fixed stale shared-daemon state repeatedly creating private daemons and made
  concurrent launches converge on one generation-specific credential set.
- Fixed VM cleanup ownership being discarded before backend shutdown succeeds.
- Fixed prompt input, terminal escape handling, paste, resize, interrupt, and
  PTY teardown edge cases across host, VM, and SSH contexts.
- Fixed leaked vmshd child processes and host-shell descendant processes.
- Hardened archive extraction against traversal, symlink, hardlink, and
  destination-symlink escapes.
- Fixed SSH diagnostic races, known-hosts update races, malformed export
  reporting, wrapped structured errors, and tar finalization errors.
- Fixed trusted-call stdout or stderr occasionally being dropped when a
  short-lived action exited before its pipe readers drained the final bytes.
- Fixed snapshot-fleet out-of-memory and lost-wakeup failures and stabilized BSD
  guest PTYs under command stress.

## Security model

- vmshd authenticates local clients, but all vmsh frontend processes running as
  the same operating-system account currently share one daemon security
  principal. Do not run an untrusted vmsh frontend under the same account.
  Frontend-scoped credentials are tracked in
  [vmsh#112](https://github.com/tinyrange/vmsh/issues/112).
