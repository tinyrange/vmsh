# vmsh v0.4.2

## Fixed

- Restored eager VM activation when selecting a guest context such as
  `@alpine`. The VM now boots before the guest prompt is shown instead of
  waiting for the first command.
- Preserved reusable startup snapshots across `vmshd` restarts, avoiding an
  unnecessary cold boot after the daemon is replaced.
- Fixed snapshot restores when the requested memory target differs from the
  target stored in the snapshot. KVM and Hypervisor.framework now restore the
  saved device state and apply the current balloon target correctly.
- Made Hypervisor.framework wait for the balloon to reach its target before
  taking a startup snapshot, preventing partially converged memory state from
  being captured.

## Validation

- Added a real-KVM regression test for warm interactive context activation.
- Re-ran the unit, race, static-analysis, cross-platform, payload, OpenAPI, and
  real-KVM integration suites for cc and vmsh.
- Verified signed native Apple Silicon boots at about 691 ms cold and 57–62 ms
  warm, with an instrumented backend snapshot restore of 89.5 ms.
