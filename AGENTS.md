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
