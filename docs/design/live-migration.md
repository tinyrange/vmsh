# Live migration between vmsh hosts

## Status and dependencies

This document plans the vmsh user experience, remote orchestration, and resource
continuity for `@migrate <host>`. The portable CPU, memory, platform, device, and
single-owner transaction are defined by
[tinyrange/cc#45](https://github.com/tinyrange/cc/issues/45) and
[tinyrange/cc#161](https://github.com/tinyrange/cc/pull/161).
vmsh must not create a second snapshot format or translate hypervisor state.

The product targets are:

- an arm64 VM on a MacBook/HVF source moving to an arm64 Raspberry Pi/Linux KVM
  destination; and
- an amd64 VM on a Windows/WHP source moving to an amd64 Linux/KVM destination.

The remote-host prerequisite is the decentralized, Tailscale-connected vmsh
group described by [tinyrange/vmsh#4](https://github.com/tinyrange/vmsh/issues/4).
Migration uses the same peer identity, short-lived mTLS certificates, discovery,
and authorization model. SSH is not a migration transport.

This is a large feature and lands in measured phases. The first end-to-end pair
is Windows/WHP to Linux/KVM because cc's first portable checkpoint targets that
pair. Both product targets remain release gates for the complete feature.

## User contract

`@migrate <host>` migrates the currently selected VM context to a named host in
the current vmsh group:

```text
@work --from ubuntu:24.04
cd /workspace/project
make test
@migrate build-linux
make test
```

The command is invalid when the selected context is `@host`, SSH, or an image
that has not become a running named VM. `<host>` is a canonical node name, not a
URL or an SSH destination. The frontend resolves it against authenticated group
membership and shows the canonical source and destination before freeze.

A successful migration preserves the selected context identity. The next
ordinary command runs in the same VM on the destination without an implicit
context switch. The guest's running processes, memory, shell process, working
directory, environment, and guest-visible device identity are preserved subject
to the resource contracts below. `@status` reports the new owning host and guest
generation.

Only one migration can own a VM at a time. Starting, stopping, resizing,
snapshotting, or launching a new command against that VM while migration is in
its freeze/commit window returns a structured conflict. Other VM and host
contexts remain usable.

An active foreground command must finish or be cancelled before freeze. vmsh
does not silently migrate the frontend's PTY/WebSocket stream. A long-running
process wholly inside the guest may continue, but an active daemon-owned host or
SSH job is not guest state and is never implied to move with the VM.

The operation reports structured phases:

- `preflight`
- `staging`
- `copying`
- `freezing`
- `restoring`
- `committing`
- `complete`
- `rolled_back`
- `reconciliation_required`

Human progress text is informational. Scripts and the UI consume the phase,
migration ID, VM ID, generation, source, destination, bytes, measured rate,
pause duration, and typed failure from the daemon API.

## Trust and authorization

Both daemons authenticate each other with short-lived mTLS certificates issued
after local or YubiKey-backed authentication. Tailscale provides reachability
and group discovery but is not, by itself, migration authorization.

The source certificate must authorize `vm.migrate.send` for the selected VM and
the destination certificate must authorize `vm.migrate.receive` for the target
group. A receiving daemon also applies local policy for maximum reservable CPU,
memory, storage, allowed image digests, network attachment, and filesystem
capabilities. Peer names, certificate identities, and group membership must all
agree; forwarded identity headers are not accepted.

Certificates and private keys live in an owner-only configuration/state file,
not environment variables or command arguments. Each migration stream binds its
random migration ID, source and destination node IDs, VM ID, generation, and
requested capability set into the authenticated protocol. Resource tunnels use
independent, migration-scoped channel keys derived inside the mTLS session and
cannot be replayed for another VM or generation.

A certificate expiring during a copy is renewed before freeze. If renewal is no
longer possible, migration aborts while the source still owns the running VM.
The implementation never extends trust by disabling verification or falling
back to plaintext.

## Preflight

Preflight runs while the guest is still fully usable. It gathers:

- source and destination vmsh/daemon/cc protocol ranges;
- host OS, architecture, hypervisor backend, and portable CPU profile support;
- free memory and storage reservations;
- the VM's image digests, memory layout, vCPU count, devices, and active
  operations;
- network, filesystem, block-storage, vsock, forwarding, and host-job resource
  classifications; and
- measured transport RTT and throughput from a bounded probe.

The destination returns one acceptance record listing every supported feature
and every rejected resource. A mixed or incomplete platform match fails here.
For example, an amd64 guest cannot move to arm64, and a destination that cannot
implement the guest's synthetic CPU profile cannot request that unsupported
registers be dropped.

The destination reserves memory and temporary storage before bulk transfer.
Important guest memory has no arbitrary size cap beyond destination capacity
and configured host policy. Transfer buffers, queued chunks, logs, and caches
are bounded aggressively so a migration cannot exhaust either daemon.

Image layers identified by digest are reused if present or staged before guest
freeze. Mutable resources must select one of the explicit continuity strategies
below; preflight rejects any resource with no strategy.

## Resource continuity

### Memory, CPU, and cc devices

vmsh delegates capture, compatibility, transfer ordering, restore, and
single-owner commit to cc's portable migration transaction. vmsh supplies the
authenticated stream, target capacity, policy, and observable progress.

The correctness path is stop-and-copy. Pre-copy is used only when source dirty
tracking and measured link throughput predict that caller downtime policy can
be met. There is no universal default number of rounds or migration timeout.

### Root image and mutable storage

Immutable OCI/image content is addressed by digest and staged on the
destination before freeze. The destination verifies every digest; matching an
image name is not sufficient.

A writable block device uses storage pre-copy: copy a base generation while the
guest runs, track source writes, then transfer the final dirty extents during
freeze. The destination cannot attach the writable copy until commit. Before
commit, failure discards the candidate and the source remains authoritative.
After commit, the source record is sealed against further writes.

The first implementation may support immutable-root guests only. It must reject
a writable block device in preflight rather than copy it inconsistently. Adding
writable storage is a distinct milestone with crash-consistency and rollback
fault injection.

### Host filesystem shares

vmsh normally exposes the source host at `/host`; changing that mount to the
destination host would silently change the selected context and could expose
unapproved data. Migration therefore never rebinds `/host` by pathname alone.

The first share-continuity implementation leaves the authorized filesystem
backend on the source and creates a migration-scoped mTLS filesystem tunnel from
the destination. Open handles and future requests continue to refer to the same
source files and permissions. The tunnel carries typed filesystem operations,
not a general remote command capability. Its grant contains the exact exported
root, access mode, VM identity, generation, peer, and expiry.

The filesystem tunnel is established and exercised before freeze. If it cannot
be verified, migration does not start. After commit the source daemon remains a
filesystem relay for that migrated share; losing it produces filesystem I/O
failure in the guest instead of falling back to a destination path. A later
re-home operation may copy and atomically transfer filesystem ownership, but it
is not part of the base memory migration.

For the first cc portability milestone, where host-bound shares are rejected,
vmsh exposes migration only for VMs whose required filesystems are immutable and
already available at the destination. `@migrate` becomes a normal vmsh workflow
only after the `/host` tunnel milestone passes, because `/host` is part of the
normal selected-context model.

### Networking

The initial continuity strategy anchors the guest's user-mode network backend
on the source. Before freeze, vmsh establishes a packet tunnel between the
destination cc device and the source network backend. After restore, the guest
keeps its MAC/IP and existing connection-tracking state because that state did
not move; destination virtio packets traverse the authenticated tunnel.

The source continues to own external port bindings until a separate network
handoff completes. Destination networking remains isolated before commit. A
failed migration simply removes the unused tunnel. Tunnel loss after commit is
reported as network degradation and does not cause the old VM generation to
resume.

This source-anchored mode favors correctness and smaller state translation over
independence from the old host. A later re-home can transfer user-mode network
state or deliberately reconnect individual flows, but must retain the same
single-owner and rollback rules. Recreating networking at the destination and
silently dropping established connections is allowed only as an explicit
future policy, never as the meaning of live migration.

### Vsock, shell, and operations

The cc control vsock listener is reconstructed at the destination. After guest
resume, the guest agent reconnects and proves the VM ID and generation before
the destination reports `ready`.

The persistent shell process lives inside guest memory and survives. Its
frontend control stream does not: vmsh reconnects through the new daemon after
commit and reattaches using the shell session identity. The implementation must
prove that shell cwd, variables, aliases/functions, exit status, and subsequent
terminal bytes remain correct rather than assuming memory restoration makes the
control plane correct.

New guest operations are blocked from freeze through commit. In-flight
frontend operations prevent freeze. Background processes already detached
inside the guest continue. Daemon-owned host and SSH jobs remain on their
original host and are reported separately by `@jobs`; migration neither kills
nor claims to move them.

## Ownership, rollback, and restart behavior

The source owns the runnable VM through destination `ready`. The destination
candidate is externally isolated. The source durably records the commit
decision before authorizing the destination to publish network ownership, and
never resumes that generation after recording commit.

Failure before commit destroys destination state and resource tunnels, releases
reservations, resumes the source, and reports `rolled_back`. Failure after an
ambiguous commit reports `reconciliation_required`. It does not guess and start
both copies. `@status` queries both peers by migration ID and generation; an
explicit repair command can destroy a proven stale candidate after comparing
the committed record and state digest.

Daemon restarts do not attempt transparent continuation of a bulk copy. A small
owner-only journal records migration ID, VM generation, peer, phase, state
digest, resource grants, and commit decision. Before commit, a restarted
coordinator aborts temporary work and preserves or resumes source ownership when
the backend proves the original VM is still present. After commit, the journal
prevents source restart from recreating the old generation and lets tunnels be
re-established only for the committed destination identity.

## Timing and cancellation

All phases are context-cancellable and individual I/O waits are bounded. The
coordinator does not use an arbitrary global default timeout. It derives a
deadline proposal from:

- bytes that remain after staging;
- measured effective throughput and RTT;
- observed guest dirty rate;
- destination restore measurements for the negotiated machine profile;
- certificate validity remaining; and
- an explicit user or group downtime policy, if configured.

The predicted duration and freeze time are shown before freeze. If facts cannot
support the requested policy, migration fails while the source runs normally.
An interactive user may cancel at any pre-commit phase; cancellation during
freeze follows the same rollback path. Once commit is recorded, cancellation
means wait for or reconcile the ownership decision, never resume the source.

## Delivery phases

### Phase 0: remote and cc prerequisites

- Land the authenticated remote-host/group connection design and implementation.
- Land cc's portable checkpoint model and WHP-to-KVM offline restore.
- Expose typed capability, resource, phase, and failure records through daemon
  APIs.

Exit gate: vmsh can preflight a named peer and explain every incompatibility
without pausing the VM.

### Phase 1: constrained stop-and-copy

- Add `@migrate <host>` for one-vCPU amd64 immutable-root VMs with no host-bound
  shares, forwards, or active operations.
- Use the cc single-owner transaction over mTLS.
- Reconnect the guest control channel and selected vmsh context after commit.

Exit gate: Windows/WHP to Linux/KVM preserves an in-guest state workload and the
next ordinary command runs in the same selected context on the destination.
Failure injection at each phase leaves exactly one runnable generation.

### Phase 2: normal vmsh filesystem and network continuity

- Add migration-scoped `/host` filesystem tunneling.
- Add source-anchored packet tunneling and preserve existing guest connections.
- Preserve persistent shell attachment while rejecting active frontend command
  streams during freeze.

Exit gate: the normal interactive workflow shown above continues across
migration, including host-workspace access and a long-lived network connection.
Killing either tunnel produces the documented degraded behavior without
starting a second VM.

### Phase 3: measured pre-copy

- Add dirty-memory and dirty-storage generations where backend APIs support
  them.
- Select freeze using measured convergence and caller downtime policy.
- Publish transfer bytes, dirty rate, throughput, and pause duration.

Exit gate: at least ten reference-workload migrations demonstrate lower
downtime than stop-and-copy on the required amd64 hardware, with no lost writes.
Any shipped threshold is justified by those measurements.

### Phase 4: arm64 HVF to KVM

- Land cc's common arm64 CPU/GIC profile and HVF/KVM adapters.
- Validate MacBook/HVF to Raspberry Pi 5/KVM through the same vmsh transaction,
  filesystem, network, shell, and ownership contracts.

Exit gate: the full behavior workload passes on arm64 without architecture- or
backend-specific branches in the vmsh protocol.

### Phase 5: re-home anchored resources

Add explicit storage, filesystem, and network ownership handoff so the source
host can be shut down after migration. Each resource class ships independently
with its own consistency and rollback tests. Until then, `@status` clearly lists
which committed VM resources remain anchored to another host.

## Validation

Unit and protocol tests cover structured behavior rather than progress wording:

- command routing and rejection outside a selected running VM;
- target resolution and authorization scopes;
- capability negotiation and complete resource classification;
- transaction state transitions and generation checks;
- deadline calculation from measured inputs;
- cancellation and deterministic failure at every phase; and
- journal reconciliation without dual ownership.

End-to-end tests run a guest workload that maintains a monotonic in-memory
counter, persistent shell state, checksummed file updates, a long-lived network
stream, and repeated commands. After migration they verify the counter never
regresses, file checksums agree, the network stream has no duplicate sequence,
the selected context and cwd survive, and a new command runs on the destination.

The fault matrix disconnects the bulk stream, filesystem tunnel, and packet
tunnel; kills the destination candidate; expires credentials before freeze;
corrupts a memory chunk; exhausts destination capacity; and restarts each daemon
before and after commit. The invariant is always either a successful destination
or a resumed source, never two runnable generations.

## Required hardware

The complete acceptance matrix requires:

- an x86_64 Windows 11 machine with Windows Hypervisor Platform enabled;
- an x86_64 Linux machine with KVM access;
- an Apple Silicon MacBook using the existing HVF backend; and
- a Raspberry Pi 5 with 8 GB RAM, 64-bit Linux, and KVM access.

Each pair needs direct Tailscale connectivity and enough free memory for the
same guest on both machines. Reports record CPU model, firmware, OS/kernel,
vmsh and cc commits, negotiated CPU profile, guest memory, resource anchors,
link throughput, dirty rate, total bytes, and pause duration. The arm64 and
amd64 pairs are separate compatibility targets; no cross-architecture migration
is implied.

## Non-goals for the first release

- Cross-architecture migration.
- SSH as transport or authentication.
- Arbitrary host CPU pass-through.
- Nested virtualization or passthrough devices.
- Post-copy memory migration.
- Silent replacement of source `/host` paths with destination paths.
- Transparent continuation of active frontend commands.
- Automatic conflict resolution after an ambiguous commit.
- A universal timeout or downtime number unsupported by hardware measurements.
