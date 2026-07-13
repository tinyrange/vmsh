# Trusted context calls: delegated-action operations

The initial implementation exposes narrow, structured VM-to-host actions. It
does not enable a host shell, guest-selected executable, network or credential
access, a terminal, stdin, or detached jobs. A gateway is created only after an
explicit grant and its host port is admitted only to that exact running VM.

Profiles are owner-only JSON files under:

```text
~/.config/vmsh/trusted/profiles/<name>.json
```

The profile must name an audited executable by absolute path and SHA-256 digest,
constrain every argument position, name its cwd roots, and provide measured
handshake and action deadlines. For example:

```json
{
  "version": 1,
  "id": "format-check",
  "risk": "delegated",
  "target_id": "local-host",
  "handshake_timeout": "2s",
  "default_root_id": "workspace",
  "roots": {
    "workspace": { "path": "/absolute/path/to/checkout", "writable": false }
  },
  "actions": {
    "check": {
      "executable": "/absolute/path/to/audited-checker",
      "executable_digest": "<sha256>",
      "root_ids": ["workspace"],
      "argument_rules": [
        { "position": 0, "pattern": "check" }
      ],
      "max_request_bytes": 4096,
      "max_duration": "30s"
    }
  },
  "digest": ""
}
```

Set the file mode to `0600`, then bind its canonical digest:

```text
vmsh profile seal ~/.config/vmsh/trusted/profiles/format-check.json
```

Changing any profile field invalidates existing grants and requires sealing and
granting the new digest. From vmsh, grant and inspect it with:

```text
@trust work --grant format-check
@permissions work
```

The grant writes an owner-only `/run/vmsh/context.json` inside the VM. Install
the `vmsh-call` command in that VM (for a development checkout,
`go install ./cmd/vmsh-call`) and invoke an action from the shared `/host`
workspace:

```text
vmsh-call host check check
```

Revoke access with:

```text
@trust work --revoke format-check
```

Revocation rejects new calls immediately and preserves the denied gateway port
instead of allowing an unrelated local listener to inherit a port that the VM
can reach. Audit records contain identities, digests, action/root IDs, result,
and an audit reference; they do not contain environment values or stream data.
They are stored owner-only under `~/.config/vmsh/trusted/audit`.

`workspace-code` and `target-user` profiles remain disabled until the target
platform can enforce their declared filesystem, process, network, and
credential containment. The daemon rejects those risk classes rather than
silently running them with weaker isolation.
