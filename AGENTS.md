# Repository Guidance

## vmsh Workflow Model

`vmsh` is an interactive, session-oriented shell. Do not reason about it as a
one-shot CLI whose primary workflow is `vmsh @image command`. A normal shell's
context is mostly "the current working directory"; `vmsh` extends that context
to "the selected system plus that system's working directory." Shell semantics
should feel seamless across host, VM, and SSH systems.

The ordinary workflow is:

- Start `vmsh` as an interactive shell.
- Use `@<image>` or `@<name> --from <image>` to select or create a VM-backed
  system context.
- Run ordinary command lines in the selected context.
- Use `@host` to return to the host context.

For example, `@alpine` by itself is a context transition. The VM starts lazily
when the user runs the first guest command after selecting that context.
`@alpine uname -a` is a supported one-shot command line inside a vmsh session,
but it is not the mental model for the product. Prefer examples and tests that
exercise context selection followed by ordinary commands when the behavior under
test depends on shell state, session state, daemon reuse, host/guest cwd
mirroring, aliases, exports, or repeated commands.

When discussing VM-host reliability, model the system as a long-lived
conversation between the frontend, vmshd/ccvm, the VM backend, guest init, and
the selected shell context. Avoid reducing failures to a single command
invocation. The important contracts are the allowed state transitions and
communication guarantees between those actors.

Tests should be useful, accurate, and focused on objective behavior. Avoid tests
that make incidental user-facing wording a strict contract; those tests block
reasonable copy changes without proving that behavior is broken. Use exact
assertions for structured state, parsed fields, exit codes, command routing,
files, protocol data, and other real contracts. Do not use `strings.Contains`
in tests unless the surrounding text is intentionally flexible and substring
matching is the only practical assertion. If a test primarily verifies UI prose,
remove it or replace it with a behavior-level check. If a test matches backend
errors, assert the structured error information instead of broad text. If a
test depends on strings from an unusual environment, treat it as flaky and
prefer rewriting or removing it.

Tests should primarily protect users from real bugs, not check that the code
still has a particular shape. Favor tests for end-to-end or behavior-level
outcomes that would matter to a user: a VM boots, a command runs in the right
place, copying works, terminal bytes are preserved, a documented protocol is
parsed, or a dangerous operation is blocked. Avoid adding tests whose main value
is preserving a convenience choice, recently chosen default, helper output,
exact argv construction, fallback order, or other implementation-adjacent
behavior unless failing that test would correspond to a real user-facing bug. A
compatibility guarantee invented during the current change is not automatically
worth testing; only keep it if it protects an important user workflow or
documented interface.

When deciding whether to add a test, ask: "What user bug would this catch?" If
the answer is mostly "it tells us the code changed," do not add the test. Prefer
no test over a low-value test that makes future useful changes harder.
