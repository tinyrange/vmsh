# SquadVM image

SquadVM is a curated Kali Linux desktop for UQ Cyber Squad. It boots systemd,
Xorg, and XFCE against the Glass virtio display and input devices. The image
includes the Glass clipboard and display-resize bridges, a non-root `squad`
user with passwordless sudo, and an explicit security-tool manifest.

Build the image from this directory:

```sh
docker build --tag squadvm:dev .
```

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
