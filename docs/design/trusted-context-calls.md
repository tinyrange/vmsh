# Trusted context calls

## Product contract

Trusted context calls let an agent in one vmsh context request a whitelisted
operation in another context with an explicit privilege set. The first workflow
lets a deliberately trusted development VM run selected commands on its host so
the VM can dogfood full vmsh and cc development without giving every VM a host
shell.

The primitive is context-to-context invocation, not VM-specific WSL emulation:

```text
source context -- capability grant --> target context action
```

The first source is a named VM and the first target is its local host. Later
sources and targets may be other VMs or authenticated nodes in a remote vmsh
group. Every hop preserves the original source identity and intersects, rather
than expands, its privileges.

The default remains unchanged: ordinary VMs, SSH clients, and guest processes
have no host execution capability. A context gateway is not even attached until
the owner explicitly grants a profile to a stable VM identity.

## Honest trust levels

Command names alone do not create a sandbox. `go test`, build systems, package
managers, interpreters, editors, and `git` hooks can execute repository code.
Allowing those actions against a writable workspace is effectively arbitrary
code execution as the target user. vmsh must label that fact rather than imply a
safe boundary because an executable was on a list.

Profiles declare one of three risk classes:

- **delegated**: narrow structured operations whose arguments and resources can
  be meaningfully constrained, such as reading repository status or invoking a
  fixed formatter on a granted tree;
- **workspace-code**: build/test/tool actions that may execute code from the
  granted workspace and therefore have the target user's authority within that
  target sandbox; and
- **target-user**: an explicit shell/interpreter or unconstrained command action
  equivalent to full command execution as the target user.

The initial dogfood profile is `workspace-code`. It improves isolation by
keeping unrelated host paths, credentials, devices, environment, and daemon
control out of the guest, but it does not claim that malicious repository code
cannot act with the target development user's granted workspace authority.

vmsh and vmshd remain security-critical. A compromised guest without a grant,
or with only a delegated grant, must not cross into `workspace-code` or
`target-user` behavior.

## User experience

Security-sensitive profiles live in owner-only configuration files. A CLI
selects an existing profile; it does not accept an inline command allowlist,
secret, or ad-hoc policy expression.

The planned controls are:

```text
@trust work --grant development
@permissions work
@trust work --revoke development
```

Granting is a one-time explicit action for a stable named VM ID. It is not a
prompt before every command. The selected VM and prompt/status data visibly
carry a trusted-development indicator. Replacing or recreating a VM produces a
new ID and does not inherit the old grant merely because it has the same name or
image tag.

Persistent grants record the VM ID, source generation, target context ID,
profile name and digest, granted workspace roots, issuer, creation time, and
revocation generation. Changing a profile file changes its digest and requires
explicit re-grant before the new privileges apply. Deleting the VM or profile
revokes its grants.

The guest uses a small `vmsh-call` client or library. Agent tools can invoke it
directly, while an optional guest shell integration may expose approved actions
as normal-looking commands. The protocol remains structured even when the UI
looks shell-like:

```text
vmsh-call host go test ./...
vmsh-call host git status --short
```

The target action ID is `go` or `git`; the executable path and policy come from
the target profile. vmsh never joins the arguments into `sh -c`. Pipelines,
redirections, globbing, command substitution, and shell functions require a
separately granted shell action and therefore the `target-user` risk class.

## Profile model

A profile is a signed or owner-only canonical document containing:

- profile ID, version, digest, and risk class;
- allowed source context type and optional VM/image identity constraints;
- allowed target context type and stable target ID;
- workspace roots and read/write modes;
- named action definitions;
- environment, network, credential, terminal, detached-job, and child-process
  privileges;
- resource and concurrency policy;
- audit redaction rules; and
- revocation behavior for running calls.

An action contains:

- stable action ID and description;
- absolute target executable identity;
- executable replacement policy, optionally including a file digest or trusted
  installation root;
- argument grammar and maximum metadata size;
- allowed cwd roots and guest-to-target cwd mapping;
- target-constructed environment keys and source-overridable keys;
- stdin mode, terminal permission, signal policy, and output mode;
- network and credential broker IDs;
- child-process/sandbox policy; and
- progress/deadline policy.

Executable identity is resolved without searching a guest-provided `PATH`.
Profiles may point at a managed toolchain directory whose version can change
under target policy, but a guest cannot replace the action through its writable
workspace or a symlink.

Argument grammars should constrain real privilege boundaries, not preserve a
convenience choice. A delegated formatter can require paths beneath one root. A
workspace-code `go` action may allow normal Go arguments because repository code
already has that risk class. Tokens that turn an action into a shell, load an
arbitrary plugin, select an alternate executable/configuration, or escape a
sandbox must either be denied or move the action to the honest higher risk
class.

## Privilege vocabulary

Privileges are typed and independently intersected:

- `action.invoke:<id>`
- `filesystem.read:<root-id>`
- `filesystem.write:<root-id>`
- `network:<policy-id>`
- `credential.use:<broker-id>`
- `terminal`
- `signal`
- `job.detach`
- `target.env:<key>`
- `context.call:<target-id>`

Filesystem roots are stable target-side objects, not guest-provided strings.
The evaluator resolves symlinks and platform path semantics before execution
and prevents later path replacement from moving access outside the root. A
write grant does not imply executable search, credential access, or host daemon
control.

Network policy names destinations/protocols or grants normal target-user
networking. Credential brokers expose a narrow operation rather than copying a
secret into the guest or command environment. For example, the existing Codex
auth proxy is a useful pattern because it permits a small upstream API surface;
it should become a named broker capability rather than a general-purpose proxy.

Some development tools inherently consume host credentials. A profile that
runs the host `gh` or `git` with the target user's normal credential helpers
must say so and is at least `workspace-code`. Where practical, separate brokers
for Git signing, GitHub API operations, package registries, and agent API access
should narrow exposure and keep long-lived secrets outside both guest and child
environment.

## Transport and source identity

When a grant is active, vmshd attaches a context gateway through the cc guest
control/vsock plane and exposes a guest Unix socket such as
`/run/vmsh/context.sock`. Isolated VMs use the same channel; no host TCP port is
opened.

The gateway derives source VM ID, VM generation, session, and grant set from the
authenticated vsock/control attachment. Request fields cannot claim a different
source. A random connection token may protect the guest Unix socket from other
guest users, but it is not the host authorization decision.

Each request contains:

- protocol version;
- random call ID and monotonically increasing source sequence;
- source context and generation as observed by the gateway;
- target context ID;
- profile digest and action ID;
- argv as an array, logical cwd root/path, and allowed environment overrides;
- stdin/terminal declaration; and
- an explicit deadline or renewable progress lease.

The gateway returns an accepted job ID followed by typed stdout, stderr,
terminal, progress, exit, error, and audit-reference events. Clean end-of-stream
without one terminal exit/error event is a protocol failure. Duplicate terminal
events and replayed call IDs with different request digests are rejected.

Input, output, and terminal resizing use backpressure and bounded buffers.
Important command output is streamed to its consumer; only replay/log caches are
size-bounded and report truncation structurally. A slow guest cannot make vmshd
buffer unlimited host output or block unrelated daemon work.

## Target execution

The target resolves the profile and grant again when accepting the call. It
does not trust an earlier frontend check. Execution occurs in a fresh process
with:

- the fixed executable identity;
- argv validated by the action grammar;
- cwd mapped beneath an authorized target root;
- a minimal target-built environment;
- only named credential brokers;
- platform sandbox/resource controls required by the profile; and
- process-group/job ownership so cancellation reaps descendants.

The dogfood development profile maps a chosen host repository root to the same
workspace visible in the VM, permits normal build/test tools for that tree, and
keeps the rest of the host filesystem out of the process sandbox where the host
OS supports it. The profile does not mount general home, SSH, cloud, package,
browser, or daemon credentials. Tools that need a credential use an explicit
broker or an explicitly declared target-user environment.

Guest cwd mapping is only automatic for a path within a granted shared
workspace. A cwd in the guest's private filesystem has no host equivalent and
is rejected unless the request chooses another granted target cwd. Windows path
comparison is case-insensitive and volume-aware; Unix path checks use actual
resolved filesystem identities.

Target stdout/stderr bytes and exit status are returned exactly. A PTY is
allocated only when the action grants `terminal`; terminal size and signal
events then follow the same out-of-band contracts as other vmsh contexts.

## Lifecycle, cancellation, and time

The target daemon owns each accepted job and records call ID, request digest,
source/target identity, profile digest, process identity, and terminal result.
A source reconnect can query the same call; it cannot accidentally start a
second process. Detached execution requires `job.detach`; otherwise losing the
source session cancels the job.

Every call has bounded waits, but there is no arbitrary universal command
timeout. Actions select one of:

- an evidence-based maximum duration for a known bounded operation;
- a caller-supplied deadline required by the profile; or
- a renewable progress lease for builds/tests whose duration depends on host
  and repository size.

Progress leases are renewed only while the authenticated source remains
attached and the target process is alive or producing an action-defined
heartbeat. Transport reads/writes, process cancellation, and child reaping have
their own bounded contexts based on observed local behavior. Profiles may set a
hard maximum when facts justify it; they must not inherit a meaningless global
default.

Cancellation signals the owned process group according to platform policy,
waits a measured grace period where supported, escalates, and always completes
the terminal protocol. If a target cannot prove descendants are contained, an
action capable of spawning persistent children requires `job.detach` and an
explicit lifecycle contract.

Revocation prevents new calls immediately. A profile chooses whether existing
jobs are cancelled or allowed to finish; `target-user` and credential-bearing
actions cancel by default. VM stop/delete closes the gateway, revokes its
generation, and cancels non-detached jobs. Daemon restart does not recreate a
lost process, but its small durable record prevents an ambiguous caller retry
from running the same mutation again.

## Auditing

Every decision records:

- call and job IDs;
- source VM/context ID and generation;
- target context/node ID;
- profile/action ID and digest;
- authorized cwd root ID and relative path;
- start/end time, terminal result, resource summary, and cancellation source;
- evaluator rule that denied a request; and
- credential broker IDs used.

Arguments are logged only according to per-position redaction rules. Environment
values, stdin/stdout/stderr, credential contents, guest memory, and unrestricted
host paths are not audit payloads. The user can correlate a failed or running
call through the audit reference without exposing secrets in normal UI output.

Audit logs are owner-only, bounded by retention/size policy, and never the sole
record of an active job or grant. Dropping old audit data cannot grant access or
change process ownership.

## Generalizing beyond VM to host

The protocol names source and target contexts from the start, but implementation
enables pairs deliberately:

- VM to its local host: first dogfood target;
- VM to another local VM: useful for build/test service separation;
- host/frontend to VM: aligns existing structured guest execution;
- group node/VM to an authenticated remote context: depends on the remote-group
  mTLS identity and capability model; and
- SSH: remains separate until it can convey an authenticated context identity
  without trusting arbitrary remote shell input.

A relay cannot mint permissions. It forwards the original source identity,
profile digest, requested target, and call digest, and every relay/target
intersects its own policy. Remote calls require both the source grant and remote
group `context.call`/action authorization. A local `workspace-code` grant does
not automatically become remote host execution.

## Delivery plan

### Phase 0: evaluator and protocol

- Define canonical profiles, grants, typed privilege vocabulary, risk classes,
  request/events, and structured errors.
- Implement pure policy evaluation, root/path resolution, executable identity,
  request digest, replay, and revocation generation.
- Add owner-only profile/grant storage and audit records.

Exit gate: hostile profile/request fixtures cannot widen an action through argv,
environment, cwd, symlink replacement, source spoofing, replay, or profile
mutation.

### Phase 1: VM-to-host delegated actions

- Attach the vsock context gateway only to a granted named VM.
- Implement non-terminal structured execution, streaming, cancellation, and
  terminal events for narrow delegated actions.
- Keep shell, network, credentials, detached jobs, and arbitrary child process
  behavior disabled.

Exit gate: an untrusted VM cannot detect a usable gateway; a granted VM can run
one filesystem-scoped action and receives exact output/exit status; revocation,
VM replacement, cancellation, and daemon restart fail closed.

### Phase 2: trusted development dogfood

- Add the explicit `workspace-code` development profile for a selected vmsh/cc
  checkout.
- Support `git`, Go toolchain, repository build helper, terminal, and bounded
  credential brokers needed by the real agent workflow.
- Integrate `@agent codex` without copying general host auth into the VM.

Exit gate: from a Darwin/arm64 or Linux/amd64 guest, an agent can inspect and
edit the mounted checkout, run vmsh and cc tests/builds in the granted host
workspace, inspect structured results, and publish through explicitly granted
brokers. Attempts to read unrelated home credentials, invoke an unlisted tool,
escape cwd, or call vmshd control fail.

### Phase 3: all local platforms and context pairs

- Implement equivalent process containment, path, cancellation, and terminal
  behavior on Linux/arm64 and Windows/amd64/arm64.
- Enable VM-to-VM and host-to-VM pairs using the same evaluator and events.
- Document platform sandbox differences honestly in profile capability data.

Exit gate: the behavior and adversarial workload passes on every supported host
platform. A platform without a required containment primitive rejects that
profile instead of silently weakening it.

### Phase 4: remote context calls

- Bind grants to remote-group identities and short-lived mTLS certificates.
- Forward narrow context calls without exposing a general remote host route.
- Preserve source identity, request idempotency, cancellation, audit references,
  and privilege intersection across the relay.

Exit gate: a granted VM can invoke one remote action, while a normal group
certificate, stale resource generation, revoked node, or relay compromise
cannot broaden the target action.

## Validation

Policy tests assert structured decisions for:

- no grant, wrong VM generation, wrong target, stale profile digest, and revoked
  grant;
- executable replacement, symlink and path traversal, Windows volume/case, and
  workspace rename races;
- forbidden argv capability switches, environment keys, network, credentials,
  terminal, signal, detached jobs, and child behavior;
- source spoofing, duplicate/replayed call IDs, and changed request digests;
- stream backpressure, exactly one terminal event, cancellation, child reaping,
  lease expiry, and daemon restart; and
- audit redaction and bounded retention without authorization side effects.

End-to-end tests protect real user workflows. They boot a guest, prove an
ungranted call is impossible, grant a profile, execute a target action that
changes a file beneath the approved workspace, observe exact bytes and exit
status, revoke the profile, and prove the next call cannot start. The dogfood
test runs repository code and checks build/test artifacts and structured exit
status rather than matching UI prose or exact tool argv construction.

Security testing supplies malicious repository scripts, hooks, symlinks,
environment values, oversized metadata, stalled input/output, forked children,
and forged protocol frames. For `workspace-code`, tests verify containment to
the privileges the profile honestly declares; they do not pretend the build
itself is trusted.

## Required hardware

The initial development milestone requires either:

- an Apple Silicon Mac with Hypervisor.framework for Darwin/arm64; or
- an x86_64 Linux KVM host with user access to `/dev/kvm`.

Before the complete feature merges, validation also requires:

- Linux/arm64 KVM on a Raspberry Pi 5;
- Windows amd64 with Windows Hypervisor Platform; and
- Windows arm64 with Windows Hypervisor Platform.

Reports record host OS/build, architecture/backend, vmsh and cc commits, profile
digest/risk class, source and target context types, sandbox capabilities, tool
versions, and test outcome. They never record private keys, credential values,
guest auth files, or unredacted command arguments.

## Non-goals for the first release

- Treating all VMs or all agents as trusted.
- Claiming a build/test allowlist prevents malicious workspace code execution.
- Mounting general host credentials into a guest.
- Guest-selected executables, host paths, environment, or privilege profiles.
- A shell-compatible parser in the protocol.
- Prompts before every permitted command.
- SSH or remote-host execution before authenticated context identities land.
- Silently weakening containment on a platform missing required primitives.
- A universal command timeout unrelated to the action and observed system.
