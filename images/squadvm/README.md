# SquadVM image

SquadVM is a curated Kali Linux desktop for UQ Cyber Squad. It boots systemd,
Xorg, and XFCE against the Glass virtio display and input devices. The image
includes the Glass clipboard and display-resize bridges, a non-root `squad`
user with passwordless sudo, and an explicit security-tool manifest.
The SSH daemon runs inside the isolated guest with password and root login
disabled. SquadVM's optional host integration installs a dedicated key,
forwards the service on host loopback, and manages the `Host squadvm` block in
`~/.ssh/config`.

The image builds the Rust rewrite of Binwalk from the pinned v3.1.0 release
rather than installing Kali's Binwalk v2 package.

Build the image from this directory:

```sh
docker build --tag squadvm:dev .
```

Run the native SquadVM frontend from the repository root:

```sh
go run ./cmd/squadvm
```

With no arguments it pulls the host architecture from
`ghcr.io/tinyrange/squadvm:edge`, opens the Glass desktop in a native window,
persists the image home directory, and maps `~/squadvm-shared` on the host to
`/shared` in the guest. Pass another OCI reference as the final argument to
test a locally published image.

Import and start it with `cc`:

```sh
docker save --output squadvm.docker.tar squadvm:dev
cc pull squadvm 'docker-archive:squadvm.docker.tar#squadvm:dev'
cc vm start --vnc --network --share ~/squadvm-shared:/shared \
  --display 1280x800 --init systemd --default-user root \
  --memory-mb 2048 --cpus 2 --timeout 5m squadvm squadvm
```

The VNC listener is bound to the host loopback interface. Use the
`display.vnc_address` returned by `cc vm start` from the host or through an SSH
tunnel.

The production publisher should retain the base-image digest in the Dockerfile,
tag SquadVM releases immutably, and create amd64 and arm64 images from the same
recipe.
