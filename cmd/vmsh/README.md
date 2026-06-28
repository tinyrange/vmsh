# vmsh

`vmsh` is an interactive shell frontend for `ccvm`. Ordinary lines run in the current context. Lines that begin with `@` are handled by `vmsh` itself.

`vmsh` must be run from an interactive terminal. Interactive sessions use the native `vmsh` line editor, persistent history stored in the `ccvm` cache directory, and autocomplete support for `@` builtins, cached image names, `vmsh` options, command names, and host paths.

Release builds use a `ccprod` cache/daemon identity by default. Development
builds use `ccdev`, so a checkout build can run alongside an installed release
without sharing daemon state. Pass `-cache-dir` to use an explicit cache root.
By default, a `vmsh` frontend owns its daemon session and the session is cleaned
up when that frontend exits. Start with `-system-session` or run `@detach` to
keep the session available after the current frontend closes.

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

@scratch --from alpine --memory 2g --cpus 4 sh -lc 'cat /etc/os-release'

@work --from ubuntu --memory-mb 4096
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

Named systems can be created from an image source:

```sh
@work --from ubuntu --cpus 8 --memory 12g
```

Options followed by a command apply to that command:

```sh
@ --cpus 2 pytest -q
```

The host root is mounted writable into guest commands at `/host`, and the guest workdir defaults to the mirrored host cwd, such as `/host/Users/alice/project`. Linux guests use the fastest available host-share backend, while built-in BSD guests use vmsh/cc's NFS host-share path on supported hosts. In host mode, `cd` changes the host directory. In VM mode, `cd /tmp` changes the guest workdir, while `cd /host/...` moves the host directory and returns guest commands to the mirrored host path.

`export NAME=value` is tracked by `vmsh` and applied to later host and guest commands.

Background commands can be started with a trailing `&` and inspected with `@jobs`:

```sh
sleep 10 &
@jobs
```

Aliases can include vmsh context prefixes and pipelines. Use `@alias expand`
to inspect the exact expanded line without running it:

```sh
@alias deploy=@ssh prod make deploy
@alias logs=@vm:app journalctl -f
@alias expand deploy && logs | @host cat
```

## Builtins

These attention words are reserved:

```sh
@help
@host [command...]
@jobs
@sessions
@detach
@ps
@status
@start
@stop [name|vm:name|ssh:name]
@forward <host-port:guest-port>
@copy SRC DST
@alias [name=value]
@alias expand line
```

`@host` with no command switches the current context to the host. `@host <command>` runs a one-shot host command.

## Options

`vmsh` options are parsed before the command:

```sh
--from <source>
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

For local development, `tools/build.go run` builds `ccvm` and `vmsh`
separately and runs `vmsh -ccvm build/vmsh/ccvm`.

`vmsh` keeps `cc` as a submodule. After cloning, initialize it with:

```sh
git submodule update --init --recursive
```

Then run:

```sh
./tools/build.go run
```

On Windows, run the same helper with:

```powershell
go run .\tools\build.go run
```

The runner builds the Linux guest init payloads and `ccvm` inside the `cc`
submodule, then builds this repository's `vmsh` binary.

You can also build `vmsh` directly and point it at a `ccvm` binary:

```sh
go build ./cmd/vmsh
./vmsh -ccvm /path/to/ccvm
```
