package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tinyrange/vmsh/internal/desktopapp"
)

const defaultNeurodesktopImage = "ghcr.io/tinyrange/neurodesktop-glass:20260727"
const defaultCVMFSCacheLimit = int64(5 << 30)

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
