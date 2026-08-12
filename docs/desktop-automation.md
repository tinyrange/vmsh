# Desktop automation API

SquadVM and NeurodeskAppX can expose their native guest display session through
an optional local HTTP API. The API reads frames directly from the virtual
desktop and sends input through its virtio keyboard and pointer devices. It does
not use host screen capture, Accessibility, or other macOS automation APIs.

The API is disabled unless all three automation options are supplied. It only
accepts a literal loopback listen address, and every request requires the bearer
token read from the configured file.

Create a token and capture root:

```sh
mkdir -p "$HOME/vmsh-automation/captures"
openssl rand -hex 32 > "$HOME/vmsh-automation/token"
chmod 600 "$HOME/vmsh-automation/token"
```

Start either desktop app with its normal image argument plus:

```sh
go run ./cmd/squadvm --automation-listen 127.0.0.1:17870 \
  --automation-token-file "$HOME/vmsh-automation/token" \
  --automation-capture-dir "$HOME/vmsh-automation/captures" \
  --automation-autostart \
  ghcr.io/tinyrange/squadvm:edge
```

The startup log reports the actual address, which is useful when port `0` was
requested. Automation currently accompanies the native graphics window and
cannot be combined with `--vnc`. `--automation-autostart` is optional; it skips
the host-side settings screen and starts the VM with the saved settings so a
fully unattended session does not require host UI automation.

Set a shell variable for the examples below:

```sh
AUTOMATION_TOKEN=$(tr -d '\r\n' < "$HOME/vmsh-automation/token")
AUTOMATION_URL=http://127.0.0.1:17870
```

Check whether the guest desktop session is ready:

```sh
curl -sS -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  "$AUTOMATION_URL/v1/status"
```

Capture the next 120 framebuffer updates observed by the API into a new
directory beneath the configured capture root:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"frames":120,"subdir":"firefox-av1-001","source":"framebuffer"}' \
  "$AUTOMATION_URL/v1/captures"
```

Capture jobs are asynchronous. `GET /v1/status` reports progress and the
absolute output directory. A subdirectory must be one new directory name;
existing paths and traversal are rejected. To stop the active job:

`source` can be `framebuffer` (the default) or `presentation`. Framebuffer
captures read the VM scanout through its CPU path. Presentation captures read
the desktop app's composed OpenGL backbuffer immediately before it is presented,
which includes the zero-copy accelerated texture and is useful for isolating
native presentation corruption.

To stop the active job:

```sh
curl -sS -X DELETE -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  "$AUTOMATION_URL/v1/captures/current"
```

Keyboard input uses Linux input-event keycodes. For example, keycode 30 is the
physical `A` key:

```sh
curl -sS -X POST -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  -H 'Content-Type: application/json' -d '{"code":30,"down":true}' \
  "$AUTOMATION_URL/v1/input/key"
curl -sS -X POST -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  -H 'Content-Type: application/json' -d '{"code":30,"down":false}' \
  "$AUTOMATION_URL/v1/input/key"
```

Pointer coordinates are absolute guest pixels. The button mask is left `1`,
middle `2`, right `4`, wheel up `8`, and wheel down `16`:

```sh
# Move to (640, 400), press left, then release it.
curl -sS -X POST -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"x":640,"y":400,"buttons":1}' "$AUTOMATION_URL/v1/input/pointer"
curl -sS -X POST -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"x":640,"y":400,"buttons":0}' "$AUTOMATION_URL/v1/input/pointer"
```

High-resolution wheel input uses v120 units, where 120 is one detent:

```sh
curl -sS -X POST -H "Authorization: Bearer $AUTOMATION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"delta_x_120":0,"delta_y_120":-120}' \
  "$AUTOMATION_URL/v1/input/scroll"
```

Treat the token like a password: a process that can read it can capture the
guest display and control its active desktop session.
