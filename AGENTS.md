# Repository Guidance

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
