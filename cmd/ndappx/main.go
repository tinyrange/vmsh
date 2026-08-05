package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/desktopapp"
)

const defaultNeurodesktopImage = "ghcr.io/tinyrange/neurodesktop-glass:20260727"

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
		ProductName:                        "NeurodeskAppX",
		Subtitle:                           "Reproducible neuroimaging",
		Kind:                               "ndappx",
		Theme:                              desktopapp.ThemeNeurodesk,
		DefaultVMName:                      "ndappx",
		DefaultImage:                       defaultNeurodesktopImage,
		DefaultStorage:                     "~/neurodesktop-storage",
		GuestStorageMount:                  "/vmsh-neurodesktop-storage",
		DefaultUser:                        "jovyan",
		DefaultMemoryMB:                    8192,
		DefaultCPUs:                        platformDefaultCPUs(),
		DefaultEphemeralHome:               platformDefaultEphemeralHome(),
		AMD64Emulation:                     true,
		BrandPNG:                           neurodeskIconPNG,
		ConfigDirName:                      "NeurodeskAppX",
		DataDirName:                        "NeurodeskAppX-data",
		ImageNamespace:                     "ndappx",
		CacheImageDir:                      "ndappx",
		DesktopReadiness:                   neurodeskDesktopReadiness,
		SSHHost:                            "neurodesk",
		SSHUser:                            "jovyan",
		SSHHome:                            "/home/jovyan",
		ReleaseAssetPrefix:                 "NeurodeskAppX",
		ExperimentalCompressedOCI:          true,
		ExperimentalBackgroundImageUpdates: true,
	}, platformArguments(os.Args[1:]))
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	fmt.Fprintln(os.Stderr, "NeurodeskAppX:", err)
	os.Exit(1)
}
