# Shell Execution Model

This document describes the shell/terminal problem vmsh is running into, the
constraints that make it tricky, and the low-level direction for fixing it.

## Core Principle

Separate the terminal plane from the control plane.

The terminal plane should be opaque PTY bytes only:

- no command status markers,
- no cwd sentinels,
- no prompt sniffing as the correctness boundary,
- no vmsh protocol embedded in the foreground terminal stream.

The control plane should carry structured vmsh metadata:

- command nonce,
- command lifecycle,
- exit status,
- current working directory,
- shell/session identity,
- selected vmsh context,
- backend-specific bookkeeping.

The current persistent-shell problem comes from making one stream serve both
roles. vmsh forwards terminal-like output, but also parses that same output for
in-band protocol markers. That is the source of most of the weirdness around
editors and terminal UI programs.

The long-term design should not be "vmsh learns how to identify TTY programs."
It should be "vmsh stops treating the terminal byte stream as a command
protocol."

## The Problem

vmsh wants commands to feel like they run in a normal interactive shell while
also preserving shell state across commands.

Those goals are not fundamentally in tension. A normal terminal session already
has both:

- the shell process is persistent and stateful,
- each foreground job gets the terminal.

The problem in vmsh is an artifact of the current implementation. vmsh keeps
persistent shell sessions warm, but it also sends commands into those shells and
then parses stdout for markers that report readiness, exit status, and current
working directory.

That is fine for simple command output, but it is not a real terminal
emulator. Full-screen terminal programs such as `vim`, `nvim`, `less`, `top`,
and similar tools expect raw terminal semantics. They switch terminal modes,
use the alternate screen, react to resize signals, read raw bytes, and may open
`/dev/tty` directly. They must be allowed to treat the terminal byte stream as
opaque.

The fix is not to choose between warm shell state and real TTY semantics. The
fix is to move command metadata out of the terminal stream.

## How Normal Shells Work

A conventional terminal session has a kernel pseudoterminal:

- The terminal emulator owns the PTY master.
- The shell and commands use the PTY slave as their controlling terminal.
- Bytes written to the PTY master are delivered as terminal input to the
  slave-side process.
- The shell is a foreground process group while it is reading commands.
- For a foreground job, the shell forks the command, puts it in a process
  group, gives that process group foreground ownership of the terminal with
  `tcsetpgrp`, then waits.
- The foreground program reads and writes the terminal directly.
- Control characters such as Ctrl-C are interpreted by the terminal line
  discipline and delivered as signals to the slave-side foreground process
  group, unless the foreground program has put the terminal in raw mode.
- When the job exits or stops, the shell takes terminal foreground ownership
  back and restores terminal modes.

The shell does not need to know that `vim` is an editor. It gives every
foreground job the terminal.

vmsh should preserve this property. It should not need to guess command names in
order to decide whether a command deserves terminal semantics.

## Two Execution Products

vmsh should distinguish two products explicitly.

### Terminal Execution

Terminal execution gives normal interactive behavior:

- the command runs in a PTY-backed terminal session,
- output is a terminal transcript,
- stdout/stderr are not cleanly separable in the way pipes expect,
- programs may change behavior because `isatty(0/1/2)` is true,
- signals mostly flow through terminal bytes and terminal-driver behavior,
- editors, pagers, REPLs, nested shells, and TUIs should work.

This is the mode users expect for foreground interactive shell use.

### Captured Execution

Captured execution gives structured process I/O:

- clean stdout,
- clean stderr,
- exit status,
- predictable behavior for scripts, pipelines, automation, and UI capture.

Captured execution may use pipes, a non-PTY exec path, or a persistent
non-interactive protocol. It should not promise perfect TUI behavior.

A PTY is a terminal abstraction, not a clean process I/O abstraction. Trying to
make one stream provide both terminal semantics and structured capture is the
core design mistake.

## True PTY Bridge

The principled long-term target is:

> persistent interactive shell on a real PTY, opaque terminal forwarding,
> out-of-band command completion reporting.

vmsh does not need to become a full shell. The real shell should remain the
job-control authority. vmsh should act as:

- PTY owner,
- byte forwarder,
- resize forwarder,
- state/control collector.

In the host case, vmsh owns the PTY master. A persistent interactive shell owns
the PTY slave as its controlling terminal. The shell launches foreground jobs
normally. The kernel, PTY, and shell handle process groups, terminal modes,
signals, `fg`, `bg`, and Ctrl-Z.

The control plane is separate. A shell wrapper or hook reports status and cwd to
a private control FD, pipe, Unix socket, or daemon-side channel after the
command returns.

Conceptually:

```sh
{
  <user command>
  __vmsh_status=$?
  __vmsh_cwd=$PWD
  printf '<length-prefixed-json-or-binary-record>' >&$__VMSH_CONTROL_FD
}
```

The exact shell syntax is not the important part. The important part is:

- the user command runs in the same interactive shell process,
- it has a real controlling PTY,
- metadata is emitted out-of-band after the shell regains control.

If the user runs `vim`, `less`, `top`, `fzf`, `git commit`, `sudo`, a nested
shell, a REPL, or a shell function that eventually opens `/dev/tty`, vmsh should
not need to know. The shell and PTY decide.

## Command Injection Trap

Do not send "command, then marker" as separate terminal input after the command
starts.

For example, this is unsafe:

```sh
python
printf status-marker
```

If `python` starts a REPL, it may consume the second line. The postlude has been
pushed into the terminal input queue and no longer belongs to the shell.

The shell must parse the whole wrapper before it starts the foreground job. A
safer shape is a syntactic wrapper such as a brace group, or a sourced temporary
command file containing prelude, user command, and postlude.

A sourced command file is attractive because vmsh does not push postlude bytes
into the terminal input queue while a TTY program is active. It still has shell
semantic edge cases:

- `return`,
- `exit`,
- traps,
- aliases,
- parse errors,
- commands that `exec` the shell.

Those are bridge design problems, but they are better than corrupting the
terminal stream.

## Prompt Detection and OSC Markers

Terminal shell-integration sequences are useful prior art:

- OSC 133 / FinalTerm semantic prompt sequences,
- OSC 7 for current working directory reporting,
- shell integrations used by iTerm2, VS Code, Warp, WezTerm, and kitty.

They show that shells can report prompt and cwd metadata to terminal-adjacent
programs.

However, prompt detection should not be the primary correctness boundary for
vmsh. Prompts are user-configurable, multiline, theme-driven, asynchronous, and
can be duplicated by program output. Even a nonce in the terminal stream is only
"unlikely to collide," not a semantic guarantee. It also does not protect vmsh
from terminal-control effects, alternate-screen behavior, or programs that
intentionally print arbitrary byte sequences.

OSC/private markers may still be useful as:

- a degraded fallback when no sideband is available,
- a shell-integration compatibility layer,
- a recovery heuristic,
- a way to track prompt/cwd when exact command lifecycle is not needed.

If vmsh uses terminal-stream markers, it should use private OSC sequences with a
per-session nonce:

- generate a random nonce in vmsh,
- store it in an unexported shell variable when possible,
- include it in every vmsh marker,
- ignore markers with the wrong nonce.

This prevents accidental marker collisions. It is not a same-user security
boundary.

## Signals, Resize, and Terminal Modes

If vmsh forwards raw bytes through a PTY, signal handling becomes simpler.

- Put the user's outer terminal in raw mode.
- Forward bytes from the outer terminal to the PTY master.
- Ctrl-C arrives as byte `0x03`.
- If the foreground process left the PTY slave in cooked mode, the kernel line
  discipline delivers `SIGINT` to the foreground process group.
- If a program such as `vim` set raw mode, it receives the byte and handles it
  itself.

Resize handling is similarly direct:

- watch `SIGWINCH` on the outer terminal,
- update the PTY size with `TIOCSWINSZ`,
- let the kernel and shell/program receive the resize notification normally.

This is why normal foreground terminal programs work in ordinary terminal
emulators.

vmsh can also do better-than-shell cleanup. On command completion, it can inspect
or reset terminal state if a crashed TUI left the terminal in raw mode or failed
to leave the alternate screen.

Ctrl-Z needs a product decision. Forwarding it inward is closer to terminal
emulator behavior: the inner foreground job stops, and the inner shell regains
control. Suspending vmsh itself is probably less useful.

## What This Preserves

With a real persistent shell on a PTY, the usual "shell state is not
serializable" problem mostly disappears during a live session.

The same shell process keeps:

- local variables,
- aliases,
- functions,
- traps,
- file descriptors,
- shell options,
- jobs,
- current directory,
- exported environment,
- sourced side effects.

`fg`, `bg`, and Ctrl-Z can work because job control belongs to the shell and the
kernel PTY, not to a vmsh side protocol.

This is the strongest argument for a PTY bridge: it preserves more shell state
than snapshot/restore can, while also giving foreground jobs correct terminal
semantics.

## Per-Transport Model

### Host

The host case uses the standard PTY model directly:

- vmsh owns the PTY master,
- the persistent host shell owns the PTY slave,
- vmsh forwards raw bytes,
- vmsh forwards resize events,
- shell status/cwd reports use an out-of-band control FD.

This answers the main host question: yes, a persistent shell can expose the PTY
while a command is active and still recover status/cwd, but status/cwd reporting
should not depend on parsing the terminal stream.

### ccvm / Guest

The guest protocol models a PTY session plus an explicit control channel for
vmsh metadata.

It carries:

- PTY allocation with terminal type, initial size, and terminal modes,
- terminal byte stream in both directions,
- window-change messages,
- EOF semantics,
- lifecycle events,
- exit status,
- a sideband control channel for per-command status/cwd via `control_fd`.

The guest agent should allocate the PTY, make the shell a session leader with
that PTY as controlling terminal, and let the guest kernel/shell handle process
groups. vmsh should not invent job-control messages; there is nothing to
forward if the PTY pair is on the correct side of the boundary.

### SSH

SSH already has session channel concepts such as:

- PTY allocation,
- window-change,
- signal requests,
- channel EOF,
- channel exit-status.

Those are channel-level features, not per-command introspection inside a
long-lived remote shell. The persistent SSH shell uses:

- a helper-based sideband channel where available,
- a documented degraded fallback using in-band markers when the helper cannot
  start.

The terminal plane should still be raw. SSH can carry PTY bytes, resize events,
and signals natively. The hard part is reliable per-command metadata from inside
the long-lived remote shell.

The helper sideband is intentionally dependency-free: vmsh opens a second
non-PTY SSH session, creates a private FIFO on the remote host, reads that FIFO
over the second channel, and gives the persistent shell fd 3 connected to the
FIFO. The shell writes ready/done/cwd records to fd 3 while foreground terminal
output stays on the PTY channel.

## Current Implementation

The implemented persistent-terminal path now follows the terminal/control split
for the primary transports:

- Host: persistent shell on a PTY, command metadata on fd 3.
- ccvm: managed exec supports `control_fd`; guest init exposes fd 3 and returns
  `control` events separately from terminal output.
- SSH: persistent shell on a PTY, command metadata over a helper FIFO read by a
  second SSH session when available, with compatibility fallback.

All three persistent paths send wrapped commands as a single shell-level
invocation so the shell parses the postlude before the foreground job starts.
The postlude is no longer pushed into the terminal input queue after a command
has already taken over the terminal.

## Graceful Degradation

Hooks or sidebands may disappear. For example:

- the user runs `bash --norc`,
- the user starts a nested shell,
- the user SSHes somewhere from inside a session,
- a remote shell does not support vmsh integration,
- a helper channel cannot be created.

With raw PTY forwarding, this is still okay. The terminal stream remains valid,
and interactive programs still work. vmsh merely loses metadata until the
control plane becomes active again.

That is a much better failure mode than the current cooked marker protocol,
where losing marker discipline can corrupt the terminal stream or confuse
command completion.

## Session Lifetime

Who owns the PTY master determines session lifetime.

If vmsh owns the PTY master and vmsh dies, the shell/session will usually get
SIGHUP and die. If sessions should survive vmsh restarts, there must be a holder
process that owns the PTY master independently of the vmsh UI.

This is where tmux is worth considering.

## Buy-vs-Build: tmux Control Mode

Before building a custom bridge, it is worth spiking tmux control mode
(`tmux -CC`), which is what iTerm2 uses.

Running persistent sessions inside tmux could provide:

- PTY correctness,
- shell persistence,
- job control,
- structured metadata/control,
- survival across vmsh restarts.

Costs:

- tmux dependency in every context,
- another escape-sequence/control layer,
- extra moving parts in guests and remote hosts,
- possible mismatch with vmsh's desired UX.

Even if tmux is not the final answer, it clarifies the central design question:
who owns the PTY master, and should sessions outlive the vmsh frontend?

## Role of Snapshot/Restore

Partial snapshot/restore still has a role, but not as the per-command execution
model.

It is useful for:

- bootstrapping a new session,
- seeding cwd/env/context into a fresh shell,
- crash recovery after a session dies,
- backends that do not yet have a persistent PTY bridge,
- migrating minimal state across transports.

The state worth making contractual across new sessions is probably:

- cwd,
- exported environment,
- selected vmsh context,
- vmsh-owned aliases/functions/macros.

Shell-native aliases, functions, options, jobs, local variables, traps, file
descriptors, and sourced-script side effects should be preserved when a real
persistent shell remains alive. They should not become universal cross-mode
promises.

Within a live PTY session, the bridge preserves real shell state by keeping the
shell process alive. That is better than trying to serialize shell state after
every command.

## Explicit Modes

Even if the long-term default becomes correct, explicit execution modes are
useful.

Examples:

```sh
@tty nvim file
@raw top
@capture make test
@exec printf '%s\n' data
```

`@tty` / `@raw` can mean:

> run this as an attached terminal job; do not promise clean stdout/stderr
> capture.

`@capture` / `@exec` can mean:

> run this through the captured command protocol; do not promise full terminal
> behavior.

These modes are not hacks if they are documented as semantic choices. They are
also valuable as debugging paths for the bridge.

## Avoid Shell-Mediated Handoff as Core Architecture

A shell-mediated handoff can be an optimization, but it should not be the core
architecture.

There is no reliable "will this command need a TTY?" oracle:

- `git commit` may launch an editor depending on config and environment,
- `man` may invoke `less`,
- a shell function can call `fzf`,
- a script can open `/dev/tty` halfway through,
- a program can switch to alternate screen only under certain runtime
  conditions.

Detection will always lag reality. The default foreground path should already
have correct terminal semantics.

## Testing Matrix

The bridge should be tested brutally, especially on host first:

- `vim`,
- `nvim`,
- `less`,
- `man`,
- `top`,
- `htop`,
- `fzf`,
- `ssh` inside vmsh,
- `sudo`,
- `git commit`,
- Python, Node, and Ruby REPLs,
- nested `bash` and `zsh`,
- Ctrl-C,
- Ctrl-Z,
- `fg`,
- `bg`,
- background jobs,
- terminal resize,
- alternate screen,
- commands that write without a trailing newline,
- commands that print marker-looking bytes,
- commands that leave raw/no-echo modes behind,
- commands that kill or `exec` the shell.

If these pass on host first, the design is probably sound enough to carry into
`ccvm` and SSH.

## Recommended Path

The main implementation path is now:

1. Keep the default foreground path on the persistent PTY bridge.
2. Keep captured execution as the explicit structured-output product.
3. Use the sideband control plane on host, ccvm, and SSH when available.
4. Keep the SSH in-band marker path only as a degraded compatibility fallback.
5. Continue hardening job-control edge cases with the testing matrix above.

Foreground terminal correctness should rank above alias/function fidelity. If
aliases and functions are preserved because a real persistent shell remains
alive behind a PTY, great. If preserving them forces vmsh to corrupt terminal
semantics, terminal correctness wins.

## Open Questions

- Should vmsh depend on tmux control mode, use it opportunistically, or build a
  bespoke PTY bridge?
- Should sessions survive vmsh frontend restarts? If so, what process owns the
  PTY master?
- What should the explicit capture syntax look like?
- How should vmsh expose command status/cwd when sideband control is
  temporarily absent?
- How much shell integration should be attempted for bash, zsh, fish, and plain
  POSIX sh?
- Should outer Ctrl-Z suspend vmsh itself or be forwarded inward to the
  foreground job? Forwarding inward is closer to terminal-emulator behavior.
