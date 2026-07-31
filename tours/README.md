# Tested vmsh tours

Tours are executable user stories. Each `.star` file drives a real interactive
vmsh session through a PTY, asserts user-visible behavior, and produces an
asciinema v2 cast with timed `vmsh.tour.section` metadata events.
The website replays the recorded terminal bytes and resize events with Xterm.js,
so alternate-screen applications such as tmux and nvim use normal terminal
semantics rather than a transcript-specific renderer.

Generate the context-switching tour after building vmsh and ccvm:

```sh
./tools/build.go build
go run ./cmd/vmsh-tour \
  -vmsh build/vmsh/vmsh \
  -ccvm build/vmsh/ccvm \
  -out site/public/tours/context-switching.cast \
  tours/context-switching.star
```

For deterministic CI runs, provide the local fixture instead of pulling an
image by name:

```sh
build/vmsh/cc \
  -ccvm build/vmsh/ccvm \
  -cache-dir build/tour-cache \
  pull vmsh-tour-alpine cc/fixtures/alpine.simg
go run ./cmd/vmsh-tour \
  -vmsh build/vmsh/vmsh \
  -ccvm build/vmsh/ccvm \
  -cache-dir build/tour-cache \
  -var image=vmsh-tour-alpine \
  -out-dir site/public/tours \
  tours/*.star
```

## Starlark API

- `ctx.section(title, markdown)` adds a timed teaching section.
- `ctx.type(text)`, `ctx.enter()`, and `ctx.key(chord)` send terminal input.
- `ctx.wait_prompt(timeout_seconds=30)` waits for the interactive vmsh editor.
- `ctx.expect_line(line, timeout_seconds=30)` checks an exact rendered line.
- `ctx.expect_output(text, timeout_seconds=30)` checks terminal bytes emitted
  after the last Enter key.
- `ctx.wait_title(title, timeout_seconds=30)` checks the terminal title.
- `ctx.resize(cols, rows)` resizes the real PTY.
- `ctx.pause(milliseconds=500)` adds presentation pacing, not synchronization.
- `ctx.value(name, fallback="")` reads a `-var name=value` supplied by the
  runner.
- `ctx.wait_exit(timeout_seconds=30)` verifies a clean terminal exit.

Prefer prompt and behavioral assertions over pauses. Avoid assertions on prose,
colors, incidental progress messages, or wall-clock timing.

The runner defaults to a readable presentation pace: 45 ms between typed runes,
350 ms between typing and Enter, and 650 ms after revealing a section. Override
these with `-type-delay`, `-enter-delay`, and `-section-delay` when a particular
tour needs different pacing. These pauses affect presentation only; assertions
must still wait for observable terminal behavior.
