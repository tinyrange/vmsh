package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/desktopapp"
)

const defaultSquadVMImage = "ghcr.io/tinyrange/squadvm:edge"

const squadVMDesktopReadiness = `
attempt=0
while [ "$attempt" -lt 900 ]; do
    if [ -S /tmp/.X11-unix/X0 ] &&
       [ -f /run/user/1000/squadvm-desktop-ready ]; then
        if [ "$(uname -m)" = "aarch64" ] &&
           { ! grep -qs ' /proc/sys/fs/binfmt_misc binfmt_misc ' /proc/mounts ||
             [ ! -x /usr/bin/qemu-x86_64 ] ||
             [ ! -x /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 ] ||
             [ ! -r /proc/sys/fs/binfmt_misc/qemu-x86_64 ]; }; then
            exit 1
        fi
        for tty in /dev/tty[0-9]*; do
            stty -F "$tty" -isig 2>/dev/null || true
        done
        desktop_pid=$(pgrep -xo xfdesktop 2>/dev/null || true)
        if [ -n "$desktop_pid" ]; then
            session_bus=$(tr '\0' '\n' < "/proc/$desktop_pid/environ" |
                sed -n 's/^DBUS_SESSION_BUS_ADDRESS=//p')
            if [ -n "$session_bus" ]; then
                setpriv --reuid=1000 --regid=1000 --clear-groups env \
                    DISPLAY=:0 \
                    HOME=/home/squad \
                    USER=squad \
                    XDG_RUNTIME_DIR=/run/user/1000 \
                    DBUS_SESSION_BUS_ADDRESS="$session_bus" \
                    xfdesktop --reload >/dev/null 2>&1 || true
            fi
        fi
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
exit 1
`

func main() {
	err := desktopapp.Run(desktopapp.Config{
		ProductName:          "SquadVM",
		Subtitle:             "UQ Cyber Squad",
		Kind:                 "squadvm",
		Theme:                desktopapp.ThemeSquadVM,
		DefaultVMName:        "squadvm",
		DefaultImage:         defaultSquadVMImage,
		DefaultStorage:       "~/squadvm-shared",
		GuestStorageMount:    "/shared",
		DefaultUser:          "root",
		DefaultMemoryMB:      4096,
		DefaultCPUs:          platformDefaultCPUs(),
		DefaultEphemeralHome: platformDefaultEphemeralHome(),
		BrandPNG:             squadvmBrandPNG,
		ConfigDirName:        "SquadVM",
		DataDirName:          "SquadVM-data",
		ImageNamespace:       "squadvm",
		CacheImageDir:        "squadvm",
		DesktopReadiness:     squadVMDesktopReadiness,
		SSHHost:              "squadvm",
		SSHUser:              "squad",
		SSHHome:              "/home/squad",
		ReleaseAssetPrefix:   "SquadVM",
	}, platformArguments(os.Args[1:]))
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintln(os.Stderr, "SquadVM:", err)
	os.Exit(1)
}
