# Remote vmsh groups

## Product contract

Remote vmsh is a decentralized Single System Image across machines owned by one
operator. A group gives the shell one authenticated inventory of nodes, VMs,
images, and capabilities while each resource still has one authoritative owner.
Tailscale supplies reachability and node addresses. vmsh supplies identity,
mutual TLS, authorization, protocol negotiation, and resource routing.

The representative workflow is:

```text
@connect prod
@work --from ubuntu:24.04
make test
@status
```

An alias such as `prodaccess=@connect prod` may activate the group. Connecting
does not turn vmsh into an SSH shell and does not grant arbitrary host command
execution. SSH remains an independent context and deployment model.

`@connect <group>` changes the active cluster scope, not the selected system.
`@host` continues to mean the frontend's local host. VM names resolve across the
active group; an explicit `node/name` form resolves ambiguity and selects a
known owner. The prompt and `@status` expose the active group and owning node so
a command is never silently routed to an unexpected host.

When no group is active, today's local-only behavior remains unchanged. Local
daemon traffic continues to use its private loopback channel and token. Remote
traffic is a separately configured mTLS listener and never makes a local token
valid over the network.

## Decentralized identity

Each group has an owner root public key and a signed membership manifest. The
manifest is a security-sensitive file distributed to enrolled nodes and
frontends; it is not fetched from an unauthenticated discovery service. It
contains:

- group ID and human name;
- monotonically increasing manifest generation;
- owner root public key and signature;
- enrolled node IDs, stable device-issuer public keys, Tailscale DNS names, and
  allowed service roles;
- enrolled user/device issuer keys allowed to create frontend certificates;
- revoked issuer and node IDs; and
- protocol policy and maximum certificate lifetime.

The owner root can live on a YubiKey and need not be online during normal use.
An enrolled machine holds its device issuer key in the operating-system secure
store or an owner-only key file. That issuer creates short-lived node service
certificates locally. A frontend obtains a short-lived client certificate after
local OS authentication or a YubiKey-backed signing flow. Peers verify the
short-lived certificate chain against issuer keys in the signed manifest.

This model has no online central CA or management server. Tailscale name/address
lookup is convenient discovery, but a Tailscale identity cannot add a node to a
vmsh group and does not replace manifest verification.

Certificates bind:

- group ID and manifest generation;
- node or user/device identity;
- public key and serial number;
- not-before and not-after times;
- capability scopes; and
- an optional node/resource constraint.

Certificates are short-lived and are renewed before starting an operation that
cannot finish within their remaining validity. A peer rejects a certificate
whose issuer or node is revoked in its newest manifest, even before expiry.
Manifest updates are signed offline and replicated directly between owned
machines. If two manifests have the same generation but different digests, the
group enters a conflict state and remote mutation stops until the owner signs a
new generation.

Private keys and manifests live in owner-only configuration/state directories.
They are never accepted through environment variables, process arguments,
forwarded HTTP headers, or group aliases. Non-sensitive display names and node
preferences may have CLI overrides.

## Network listeners

vmshd exposes two independent listener classes:

- **local**: private loopback or local IPC, existing local token/session model;
- **group**: explicitly configured address, mandatory TLS 1.3 mutual
  authentication, group authorization middleware on every route.

Wildcard or non-loopback binds are group listeners and fail startup without a
complete group identity and TLS configuration. Hostnames are resolved at
startup; any non-loopback result makes the listener remote. A reverse proxy is
not a trust boundary in the first implementation, and proxy-provided identity
headers are ignored. Health, discovery, debug, HTTP streaming, and WebSocket
routes all pass through the same authentication and authorization layer.

The remote listener should normally bind the machine's Tailscale address. It
must still fail closed if the operating-system firewall or Tailscale policy is
misconfigured. No `--insecure-remote` compatibility path is part of the product
contract before v1.

ccvmd remains a local implementation detail behind vmshd unless a deployment
explicitly uses cc's own authenticated remote listener. vmshd never forwards a
remote bearer credential to a plaintext ccvmd TCP endpoint.

## Authorization

Capabilities are typed protocol data, not route-name strings embedded in
certificates. The initial scopes are:

- `group.read`: node inventory and non-sensitive capability summaries;
- `vm.read`: VM inventory and status;
- `vm.create`: create a VM from allowed image digests and resource limits;
- `vm.control`: start, stop, resize, and delete an allowed VM;
- `vm.exec`: open shell/exec streams inside an allowed VM;
- `image.read`: inspect image metadata;
- `image.stage`: request or transfer an allowed image digest;
- `copy`: copy through an explicitly named host/VM path capability;
- `forward`: create an allowed VM port forward;
- `migration.send` and `migration.receive`: move one allowed VM generation;
  and
- `host.exec`: execute a whitelisted host-context command through the separate
  trusted-context feature.

`host.exec` is never implied by group membership, `vm.exec`, remote daemon
access, or ownership of a VM. Existing vmshd host-job routes must be unreachable
to a normal group certificate. Debug/profiling routes require a separate local
administrative scope and are disabled on group listeners by default.

Authorization intersects three sources: certificate scopes, group/node policy,
and the requested resource's owner policy. Absence at any layer denies the
operation. Resource constraints include stable VM/image IDs, node IDs, path
roots, port ranges, CPU/memory ceilings, and certificate expiry. A request
cannot widen its grant by following redirects, changing a WebSocket route, or
continuing a stream after certificate/session expiry.

Every accepted mutation gets an audit record containing time, peer identity,
manifest generation, capability, resource ID, request ID, result, and typed
failure. Audit records omit command payloads, environment values, file contents,
tokens, and private paths unless a path is itself the authorized resource
identifier.

## Group and resource model

There is no distributed database or consensus service. Each node is
authoritative for resources it owns and publishes signed, expiring inventory
observations. The frontend combines observations into a group view.

Every resource has a globally random ID, owner node ID, owner generation,
human name, type, and capability/version summary. Names are conveniences, not
distributed locks. If two nodes publish the same VM name, unqualified lookup
returns an ambiguity error listing `node/name` candidates. vmsh never resolves
the conflict with last-writer-wins behavior.

VM ownership changes only through an explicit generation-bearing transaction,
such as live migration. Stale inventory cannot authorize a command: the owner
checks the current resource generation on every mutation and stream open.

Group activation concurrently contacts all configured nodes. It succeeds when
at least one authenticated compatible node responds and reports unavailable,
revoked, incompatible, or failed nodes separately. It does not block all shell
use on one offline laptop. A command aimed at an unavailable owner fails rather
than being routed to another same-named VM.

The frontend caches inventory only for responsiveness. Cache entries carry the
publisher, generation, digest, and expiry and are never sufficient to authorize
a mutation. Important VM/image data has no arbitrary group-wide size limit;
per-node capacity is authoritative. Inventory, logs, connection pools, queued
events, transfer buffers, and image metadata caches have explicit bounds.

## Placement

Creating a named VM in a group uses client-coordinated placement, not a central
scheduler:

1. Fetch current authenticated node capabilities and capacity offers.
2. Filter by guest architecture, backend features, requested resources, policy,
   and image availability.
3. Prefer an existing owner for that VM ID; otherwise rank eligible nodes by
   data locality and measured available capacity.
4. Ask one node for a short placement reservation tied to the VM ID and request
   digest.
5. Create the VM only after the reservation succeeds.

The ranking algorithm is deterministic and reported in structured diagnostics,
but it is not a long-term compatibility contract. Users can select an explicit
node when placement matters. If the preferred node loses the reservation, vmsh
re-probes rather than blindly replaying creation on multiple nodes.

Image names are resolved to immutable digests before placement. The selected
node may fetch the digest from its normal source or receive it through an
authenticated content stream. A matching mutable tag is not proof that two
nodes have the same image.

## Paths and copy semantics

Host paths are always owned by a node. `@host` means the frontend host;
`@node:<name>:` is required for a remote host path. A VM's `/host` mapping means
its owner host unless a documented filesystem tunnel gives a different explicit
origin. vmsh must not reinterpret a local path as a remote path because the VM
happens to run remotely.

Remote copies keep the existing explicit endpoint model. The coordinator opens
separately authorized source and destination streams and relays bounded chunks
when direct peer transfer is unavailable. Temporary staging files are owner-only
and removed after success or failure. The destination validates its path grant
after symlink resolution and before every filesystem operation that could leave
the authorized root.

Working directories remain part of each selected system context. Moving from a
local VM to a remote VM does not copy cwd state. Reconnecting to the same remote
VM/shell session restores that context's recorded guest cwd.

## Sessions, streams, and cancellation

The frontend creates a group session with a random ID and short lease bounded by
its certificate validity. Each node creates only its local session record. The
frontend renews active leases after renewing credentials; it cannot extend a
session beyond the authorization represented by the new certificate.

Long-running mutations are daemon-owned jobs with stable request IDs and typed
status. A frontend disconnect does not duplicate them. On reconnect, the
frontend queries the authoritative owner and reattaches where the protocol
supports it. Operations that are intentionally frontend-owned are cancelled on
disconnect and say so in their capability metadata.

Terminal and copy streams bind the group session, resource ID/generation,
request ID, and monotonically increasing stream sequence. HTTP and WebSocket
paths use the same configured mTLS dialer and cancellation context. A stream
cannot be resumed on another resource merely by presenting the same request ID.

All waits accept cancellation and are bounded from facts. Connection deadlines
use measured RTT and certificate validity; transfer progress uses measured
throughput and remaining bytes; session leases use the certificate expiry.
There is no universal group-operation timeout. A caller may provide an explicit
policy for an unattended job, and a stalled authenticated transport still has
bounded read/write waits and liveness probes.

## Protocol negotiation

The TLS handshake negotiates identity, not application compatibility. The first
authenticated request exchanges:

- protocol name, current version, and minimum compatible version;
- daemon build/platform identity;
- supported route and feature IDs;
- backend/architecture capabilities;
- manifest generation and digest; and
- maximum frame/stream metadata limits.

A group can contain mixed daemon versions. Read-only inventory uses the common
protocol range, while each operation requires its exact feature set on every
participating node. An older peer cannot cause the frontend to downgrade TLS,
authentication, authorization, resource generations, or structured terminal
events. If a feature is absent, vmsh explains which node/capability blocks it.

Breaking protocol changes are acceptable before v1, but a daemon must reject a
message outside its advertised range rather than decoding it as the newest
shape. Unknown required fields and features are fatal; unknown optional fields
can be ignored.

## Restart and failure behavior

Remote connectivity is expected to come and go. An unavailable node remains in
the group inventory with its last authenticated observation clearly marked
stale; it is not removed and its resources are not reassigned automatically.

Daemon restart invalidates in-memory streams and connection pools. Durable VM
ownership, daemon-owned job records, audit logs, membership manifest generation,
and placement/migration decisions are small owner-only records and survive.
The frontend reconnects, renegotiates, and re-queries those records. It does not
attempt to reconstruct terminal byte streams or transparently replay mutations.

If a request outcome is unknown, retry uses the original request ID and digest.
The owner returns the recorded result or a structured unknown state; it never
runs a second mutation solely because the connection ended. This reduces the
impact of restarts without adding a general distributed recovery service.

Revocation or authorization failure closes new streams and prevents renewal.
Existing streams end no later than the certificate/session expiry encoded when
they were opened. Security failure never falls back to a local token, SSH, an
unauthenticated ccvmd route, or another group node.

## Delivery plan

### Phase 0: identity and configuration

- Define canonical membership manifest, certificate claims, signatures,
  capability scopes, and owner-only storage.
- Implement local/YubiKey-backed short-lived certificate issuance behind one
  interface.
- Verify manifest generation/conflict and revocation behavior without a
  network listener.

Exit gate: two enrolled test identities authenticate and derive the same group,
node, and scopes; expired, revoked, wrong-group, conflicting-manifest, and
over-scoped credentials fail with structured reasons.

### Phase 1: authenticated remote inventory

- Add the fail-closed group listener and complete route middleware.
- Implement `@connect <group>`, protocol negotiation, concurrent node health,
  and merged read-only inventory.
- Keep every mutation and host route disabled remotely.

Exit gate: a frontend connects over Tailscale/mTLS to macOS, Linux, and Windows
nodes, reports capabilities and unavailable peers, and unauthenticated clients
cannot reach health, debug, WebSocket, or inventory routes.

### Phase 2: explicit remote VM workflows

- Add scoped VM create/control/exec, image staging, copy, forwarding, and
  daemon-owned job APIs.
- Preserve selected-context shell, cwd, terminal, cancellation, and exit-status
  behavior through the remote owner.
- Keep host execution unavailable without an explicit `host.exec` grant and
  trusted-context policy.

Exit gate: selecting a node-qualified VM and running ordinary repeated commands
behaves like the local session model; credential revocation, daemon restart,
duplicate request, and stream failure cannot duplicate a command or cross a
resource boundary.

### Phase 3: cluster namespace and placement

- Add unqualified group VM lookup, explicit ambiguity behavior, node capacity
  offers, short reservations, and deterministic client placement.
- Resolve image tags to digests and report the owner in prompt/status data.
- Make aliases able to activate a named group.

Exit gate: a normal `@work --from ...` selection chooses one eligible node,
keeps one authoritative VM ID, and reconnects to that owner. Duplicate names,
stale inventory, and reservation loss are observable and never create two VMs.

### Phase 4: migration and resource federation

- Provide the authenticated peer streams, node/resource scopes, capacity
  reservation, and reconciliation queries needed by `@migrate`.
- Add filesystem/network tunnel grants as narrow capabilities rather than
  general host access.

Exit gate: the live-migration plan can use the same group identities and
sessions without introducing a second trust or discovery system.

## Validation

Behavior tests protect protocol and security contracts:

- certificate chain, lifetime, group, manifest generation, scope, and
  revocation validation;
- every local and group route in a table with its required capability;
- health, debug, HTTP stream, and WebSocket denial before authentication;
- host job and host filesystem denial to normal group certificates;
- resource ID/generation enforcement on mutations and streams;
- ambiguous names and stale inventory without fallback routing;
- idempotent request behavior across disconnect/restart;
- mixed-version negotiation with no security downgrade; and
- bounded queues and cleanup under slow or malicious peers.

End-to-end tests use real daemon/frontend processes and local mTLS sockets for
most coverage. They select a remote VM context, run multiple ordinary commands,
change cwd and shell state, copy both directions, cancel a command, detach,
restart the owner daemon, reconnect, and continue without duplicating work.
Assertions target selected context, owner/resource IDs, exit status, files,
protocol events, and forbidden operations rather than UI wording.

Release smoke tests use a real Tailscale network because route/firewall behavior
cannot be proven by loopback tests. The matrix requires:

- Apple Silicon macOS/HVF;
- Linux amd64/KVM;
- Linux arm64/KVM on a Raspberry Pi 5;
- Windows amd64/WHP; and
- Windows arm64/WHP.

The virtualization hardware is required for remote VM workflows; identity and
inventory tests also run without it. Reports record host OS/build, architecture,
backend, vmsh/cc commits, group manifest digest, certificate issuer type
(local or YubiKey), negotiated protocol/features, RTT, and throughput. Private
keys, certificate serials, Tailscale addresses, and host paths are redacted.

## Non-goals for the first release

- An online central controller, scheduler, CA, or database.
- Treating Tailscale membership as vmsh authorization.
- SSH transport or SSH deployment automation.
- Automatic remote daemon installation or enrollment.
- Arbitrary remote host shell access.
- Transparent reassignment of resources from an offline node.
- Global uniqueness through distributed consensus.
- Cross-node transaction recovery beyond explicit idempotency and ownership
  records.
- A fixed timeout unrelated to measured work and credential validity.
