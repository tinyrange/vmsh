# Plan: Finish Issue #57

Goal: remove fragile `strings.Contains` assertions from tests unless substring
matching is genuinely unavoidable. Tests should block changes only when behavior
is objectively broken.

## Testing Principles

Follow `AGENTS.md`:

- Tests must be useful, accurate, and focused on objective behavior.
- Do not make incidental user-facing prose a strict contract.
- Prefer exact assertions for structured state, parsed fields, exit codes,
  command routing, files, protocol data, and other real contracts.
- Do not use `strings.Contains` in tests unless the text is intentionally
  flexible and substring matching is the only practical assertion.

## Buckets

Sort every current `strings.Contains` use in tests into one of these buckets:

1. **User-interface prose**
   - If the test exists mainly to match help text, status prose, prompt prose,
     friendly diagnostics, or other copy, remove it.
   - Keep a test only if the exact text is the feature. That should be rare and
     explicit.

2. **Backend/error semantics hidden in strings**
   - Replace with useful assertions against real information.
   - Examples: exit code, wrapped error identity, context mode, VM ID, SSH host,
     command arguments, parsed endpoint, job ID, state transition, file result.
   - If the production code only exposes prose, consider adding a small typed
     helper or parser in the test rather than pinning the whole sentence.

3. **Weird environment / generated shell / flaky text**
   - Prefer removing the test if it only checks that some generated shell text
     contains a fragment.
   - If the behavior matters, rewrite the test to observe the behavior: command
     executed, file copied, env applied, stream completed, status changed.
   - Keep substring checks only for intentionally flexible command snippets
     where there is no stable structured representation yet; leave a short
     comment explaining why.

## Work Order

### 1. Baseline Inventory

Run:

```sh
rg -n "strings\\.Contains" --glob '*_test.go'
rg -n "requireContains|assert.*Contains|Contains\\(" --glob '*_test.go'
```

Record counts by file before starting. Focus first on vmsh tests, then repeat
for `cc/`.

### 2. Command Diagnostics

Start with recently touched diagnostics in `internal/shell/shell_unit_test.go`.

- Remove tests that only police wording.
- Keep tests that assert objective behavior such as `lastCode`, wrapped error
  identity, and context selection.
- For pipeline diagnostics, assert the stage index/status through structured
  helpers if possible; otherwise reduce the test to the behavioral signal that
  actually matters.

### 3. Jobs And Status

Review job/status tests around background jobs, `@jobs`, `@status`, and prompt
rendering.

- Do not require exact prose.
- For jobs, parse IDs, state, context, and command from a stable representation
  if one exists.
- If no stable representation exists, either add a small formatter/parser test
  helper or remove the prose-only test.
- Keep security checks that secrets are not leaked, but assert against the
  actual sensitive values rather than broad UI lines.

### 4. SSH Routing And Copy Endpoint Tests

Replace substring checks on rendered commands/errors with direct assertions:

- SSH host selected.
- Origin context selected.
- Jump/route command received by the fake server.
- Copy endpoint parsed into target kind, name, and path.
- File transfer side effects happened.

Remove tests that only match explanatory error text unless the exact message is
the behavior being intentionally specified.

### 5. Generated Shell Snippets

These are the highest-risk area for busy work.

- If the test only says a generated script contains `tar -cf -`, `mkfifo`, or a
  control marker, prefer deleting it.
- If the generated command is standing in for behavior, rewrite the test to run
  through the fake target and assert the observed result.
- If substring matching remains unavoidable, isolate it in a clearly named
  helper such as `requireFlexibleShellSnippet` with a comment explaining why the
  representation is intentionally not parsed.

### 6. Integration Tests

Be conservative.

- Do not rewrite slow integration tests just to remove `requireContains`.
- If an integration assertion checks broad output prose, remove it unless it is
  validating a real end-to-end behavior not covered elsewhere.
- Prefer asserting created files, exit codes, VM state, copied data, and command
  results.

### 7. cc Test Suite

After vmsh proper has a good pattern, repeat in `cc/`.

- Prioritize tests around recently touched BSD guest/runtime behavior.
- Avoid rewriting low-value historical tests unless they block current work.
- Convert backend error text checks into typed errors or structured state
  assertions where practical.

## Completion Criteria

- Completed: no `strings.Contains` remains in tests unless it is unavoidable and locally
  justified.
- Completed: no helper exists whose only job is to hide broad substring matching.
- Completed: prose-only UI tests are removed.
- Completed: backend-error tests assert structured behavior instead of wording
  or only assert that invalid operations fail when no structured error exists.
- Completed: the remaining tests are easier to change when user-facing copy
  improves.

Remaining `strings.Contains` uses are intentionally limited to:

- PTY close errors from OS/runtime boundaries in vmsh integration tests.
- KVM BSD boot tests that synchronize against unstructured serial-console logs.
- Runtime/guest tests that look for markers emitted through unstructured
  stdout/stderr streams.
- Negative scans of unstructured guest tool output, such as `fsck_ffs`.

## Verification

Run targeted tests after each cluster. Before opening a PR:

```sh
go test ./internal/shell
go test ./...
```

If the full `internal/shell` package enters slow integration coverage, run the
specific affected unit tests first and note any full-suite timeout separately.
