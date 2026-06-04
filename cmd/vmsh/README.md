# vmsh

`vmsh` is an interactive shell frontend for `ccvm`. Ordinary lines run in the current context. Lines that begin with `@` are handled by `vmsh` itself.

`vmsh` must be run from an interactive terminal. Interactive sessions use the native `vmsh` line editor, persistent history stored in the `ccvm` cache directory, and autocomplete support for `@` builtins, cached image names, `vmsh` options, command names, and host paths.

Guest commands receive a TTY, terminal dimensions, and terminal color environment. `vmsh` keeps command execution non-interactive and adds a small color prelude for common commands such as `ls`.

Interactive host and guest commands run through persistent shell sessions when possible, so shell state such as aliases, functions, `cd`, and exported variables can survive across commands. Commands that need full foreground terminal control fall back to a one-shot shell path.

The core syntax is:

```sh
@<oci-tag> [vmsh-options] [--] [command...]
```

Examples:

```sh
@ubuntu:24.04
python --version

@host git status

@node:22 npm test

@alpine --vm scratch --memory 2g --cpus 4 sh -lc 'cat /etc/os-release'

@ --vm work --memory-mb 4096
make -j4
```

## Context Rules

Bare targets update the current context:

```sh
@alpine
```

Switching to an image checks the local image state and downloads it if needed.
The VM itself still starts lazily on the first guest command.

Commands after a target are one-shot:

```sh
@alpine uname -a
```

Bare options update the current context:

```sh
@ --vm work --cpus 8 --memory 12g
```

Options followed by a command apply to that command:

```sh
@ --cpus 2 pytest -q
```

The host root is mounted writable into guest commands at `/host`, and the guest workdir defaults to the mirrored host cwd, such as `/host/Users/alice/project`. In host mode, `cd` changes the host directory. In VM mode, `cd /tmp` changes the guest workdir, while `cd /host/...` moves the host directory and returns guest commands to the mirrored host path.

`export NAME=value` is tracked by `vmsh` and applied to later host and guest commands.

Background commands can be started with a trailing `&` and inspected with `@jobs`:

```sh
sleep 10 &
@jobs
```

## Builtins

These attention words are reserved:

```sh
@help
@host [command...]
@jobs
@ps
@status
@start [--vm id]
@stop [--vm id]
@forward <host-port:guest-port>
```

`@host` with no command switches the current context to the host. `@host <command>` runs a one-shot host command.

## Options

`vmsh` options are parsed before the command:

```sh
--vm <id>
--cwd <guest-path>
--user <user>
--sudo
--memory <n|nM|nG>
--memory-mb <n>
--cpus <n>
--network
--no-network
--nested
--no-nested
--arch <amd64|arm64>
```

Use `--` when a command begins with something that looks like a `vmsh` option:

```sh
@alpine -- --help
```

Guest commands run as UID `1000` by default. Use `@ --sudo <cmd>` or
`@sudo <cmd>` to run a command as root in the current VM.

If the daemon reports nested virtualization support, `vmsh` enables it by
default for VM contexts. Use `@ --no-nested` to disable it for the current
context or a one-shot command.

## Building

For local development, `tools/run_vmsh.sh` builds `ccvm` and `vmsh` separately
and runs `vmsh -ccvm build/vmsh/ccvm`.

`vmsh` keeps `cc` as a submodule. After cloning, initialize it with:

```sh
git submodule update --init --recursive
```

Then run:

```sh
./tools/run_vmsh.sh
```

The runner builds the Linux guest init payloads and `ccvm` inside the `cc`
submodule, then builds this repository's `vmsh` binary.

You can also build `vmsh` directly and point it at a `ccvm` binary:

```sh
go build ./cmd/vmsh
./vmsh -ccvm /path/to/ccvm
```
