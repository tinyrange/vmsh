package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/desktopapp"
)

const defaultNeurodesktopImage = "ghcr.io/tinyrange/neurodesktop-glass:latest-estargz"
const defaultCVMFSCacheLimit = int64(5 << 30)

const neurodeskGPUDesktopSetup = `
if ! command -v virgl_test_server >/dev/null 2>&1; then
    echo "the Neurodesktop image does not contain virgl-server; refresh the image before enabling GPU acceleration" >&2
    exit 1
fi

# systemd-binfmt may replace registrations made by the early guest init. The
# ARM64 Neurodesktop image carries amd64 application containers, so restore the
# emulator after systemd has settled if its registration was removed.
systemctl start systemd-binfmt.service 2>/dev/null || true
if [ -x /run/ccx3-qemu-x86_64 ] && [ ! -e /proc/sys/fs/binfmt_misc/qemu-x86_64 ]; then
    printf '%s' ':qemu-x86_64:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/run/ccx3-qemu-x86_64:F' \
        | sed 's/\\\\/\\/g' > /proc/sys/fs/binfmt_misc/register
fi

cat > /run/systemd/system/neurodesktop-virgl.service <<'EOF'
[Unit]
Description=VirGL bridge for Neurodesktop application containers
After=ccx3-stage2.service
ConditionPathExists=/dev/dri/renderD128

[Service]
Type=simple
User=jovyan
Group=users
SupplementaryGroups=render
ExecStartPre=/usr/bin/rm -f /tmp/.neurodesktop-virgl
ExecStart=/usr/bin/virgl_test_server --use-egl-surfaceless --rendernode /dev/dri/renderD128 --socket-path /tmp/.neurodesktop-virgl
Restart=on-failure
RestartSec=1s

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
chgrp render /dev/dri/renderD128
chmod g+rw /dev/dri/renderD128
systemctl restart neurodesktop-virgl.service

attempt=0
while [ "$attempt" -lt 100 ]; do
    if [ -S /tmp/.neurodesktop-virgl ]; then
        break
    fi
    if ! systemctl is-active --quiet neurodesktop-virgl.service; then
        systemctl --no-pager --full status neurodesktop-virgl.service >&2 || true
        exit 1
    fi
    attempt=$((attempt + 1))
    sleep 0.05
done
if [ ! -S /tmp/.neurodesktop-virgl ]; then
    echo "timed out waiting for the Neurodesktop VirGL bridge" >&2
    exit 1
fi

systemctl set-environment \
    LIBGL_DRI3_DISABLE=true \
    SINGULARITYENV_LIBGL_DRI3_DISABLE=true \
    SINGULARITYENV_LIBGL_ALWAYS_SOFTWARE=true \
    SINGULARITYENV_GALLIUM_DRIVER=virpipe \
    SINGULARITYENV_VTEST_SOCKET_NAME=/tmp/.neurodesktop-virgl \
    APPTAINERENV_LIBGL_DRI3_DISABLE=true \
    APPTAINERENV_LIBGL_ALWAYS_SOFTWARE=true \
    APPTAINERENV_GALLIUM_DRIVER=virpipe \
    APPTAINERENV_VTEST_SOCKET_NAME=/tmp/.neurodesktop-virgl
systemctl restart --no-block neurodesktop-glass.service
`

var defaultCVMFSMirrors = []string{
	"http://cvmfs-geoproximity.neurodesk.org",
	"http://cvmfs.neurodesk.org",
	"http://cvmfs-brisbane.neurodesk.org",
	"http://cvmfs-melbourne.neurodesk.org",
	"http://cvmfs-sydney.neurodesk.org",
	"http://cvmfs-perth.neurodesk.org",
	"http://cvmfs-jetstream.neurodesk.org",
	"http://cvmfs-frankfurt.neurodesk.org",
	"http://cvmfs01.nikhef.nl:8000",
	"http://cvmfs-s1bnl.opensciencegrid.org:8000",
	"http://cvmfs-s1goc.opensciencegrid.org:8000",
	"http://cvmfs-stratum-one.ihep.ac.cn",
	"http://sampacs01.if.usp.br:8000",
	"http://s1brisbane-cvmfs.openhtc.io",
	"http://s1melbourne-cvmfs.openhtc.io",
	"http://s1nikhef-cvmfs.openhtc.io",
	"http://s1osggoc-cvmfs.openhtc.io:8080",
	"http://s1bnl-cvmfs.openhtc.io",
	"http://s1sampa-cvmfs.openhtc.io:8080",
}

const neurodeskDesktopReadiness = `
attempt=0
while [ "$attempt" -lt 900 ]; do
    if [ -S /tmp/.X11-unix/X0 ]; then
        for comm in /proc/[0-9]*/comm; do
            [ -r "$comm" ] || continue
            IFS= read -r process < "$comm" || continue
            if [ "$process" = lxsession ]; then
                for tty in /dev/tty[0-9]*; do
                    stty -F "$tty" -isig 2>/dev/null || true
                done
                exit 0
            fi
        done
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
exit 1
`

func main() {
	err := desktopapp.Run(desktopapp.Config{
		ProductName:          "NeurodeskAppX",
		Subtitle:             "Reproducible neuroimaging",
		Kind:                 "ndappx",
		Theme:                desktopapp.ThemeNeurodesk,
		DefaultVMName:        "ndappx",
		DefaultImage:         defaultNeurodesktopImage,
		DefaultStorage:       "~/neurodesktop-storage",
		GuestStorageMount:    "/vmsh-neurodesktop-storage",
		DefaultUser:          "jovyan",
		DefaultMemoryMB:      8192,
		DefaultCPUs:          platformDefaultCPUs(),
		DefaultEphemeralHome: platformDefaultEphemeralHome(),
		PersistentHomeOwner:  &desktopapp.GuestOwner{UID: 1000, GID: 100},
		AMD64Emulation:       true,
		BrandPNG:             neurodeskIconPNG,
		ConfigDirName:        "NeurodeskAppX",
		DataDirName:          "NeurodeskAppX-data",
		ImageNamespace:       "ndappx",
		CacheImageDir:        "ndappx",
		DesktopReadiness:     neurodeskDesktopReadiness,
		DesktopWebApp: &desktopapp.DesktopWebAppConfig{
			GuestPort: 8888,
			URLPath:   "/lab",
		},
		SSHHost:                            "neurodesk",
		SSHUser:                            "jovyan",
		SSHHome:                            "/home/jovyan",
		ReleaseAssetPrefix:                 "NeurodeskAppX",
		ExperimentalGPUAcceleration:        true,
		ExperimentalGPUDesktopSetup:        neurodeskGPUDesktopSetup,
		ExperimentalCompressedOCI:          true,
		ExperimentalBackgroundImageUpdates: true,
		CVMFSHostMount: &desktopapp.CVMFSHostMountConfig{
			Mount: "/cvmfs/neurodesk.ardc.edu.au", Mirror: defaultCVMFSMirrors[0],
			Mirrors: defaultCVMFSMirrors, Repo: "neurodesk.ardc.edu.au", Path: "/",
			CacheLimitBytes: defaultCVMFSCacheLimit,
		},
	}, platformArguments(os.Args[1:]))
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintln(os.Stderr, "NeurodeskAppX:", err)
	os.Exit(1)
}
