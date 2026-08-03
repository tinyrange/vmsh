package desktopapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	configured, err := normalizeConfig(Config{
		ProductName:        "SquadVM",
		Subtitle:           "UQ Cyber Squad",
		Kind:               "squadvm",
		DefaultVMName:      "squadvm",
		DefaultImage:       "ghcr.io/tinyrange/squadvm:edge",
		DefaultStorage:     "~/squadvm-shared",
		GuestStorageMount:  "/shared",
		DefaultUser:        "root",
		DefaultMemoryMB:    4096,
		DefaultCPUs:        4,
		ConfigDirName:      "SquadVM",
		DataDirName:        "SquadVM-data",
		ImageNamespace:     "squadvm",
		CacheImageDir:      "squadvm",
		SSHHost:            "squadvm",
		SSHUser:            "squad",
		SSHHome:            "/home/squad",
		ReleaseAssetPrefix: "SquadVM",
	})
	if err != nil {
		panic(err)
	}
	appConfig = configured
	os.Exit(m.Run())
}

func TestProductConfigurationControlsSharedFolderAndImageIdentity(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })

	configured, err := normalizeConfig(Config{
		ProductName:       "NeurodeskAppX",
		Kind:              "ndappx",
		DefaultVMName:     "ndappx",
		GuestStorageMount: "/vmsh-neurodesktop-storage",
		ImageNamespace:    "ndappx",
	})
	if err != nil {
		t.Fatal(err)
	}
	appConfig = configured

	share, err := createStorageShare(filepath.Join(t.TempDir(), "research-data"))
	if err != nil {
		t.Fatal(err)
	}
	if share.Mount != "/vmsh-neurodesktop-storage" || !share.Writable || !share.MapOwner {
		t.Fatalf("configured share = %+v", share)
	}
	if image := pulledImageName("ghcr.io/example/neurodesktop:latest", "arm64"); !strings.HasPrefix(image, "ndappx/") {
		t.Fatalf("configured image identity = %q", image)
	}
}

func TestProductConfigurationKeepsManagedSSHBlocksIndependent(t *testing.T) {
	previous := appConfig
	t.Cleanup(func() { appConfig = previous })

	appConfig = Config{DefaultVMName: "squadvm"}
	squadMarker := managedSSHConfigBegin()
	appConfig = Config{DefaultVMName: "ndappx"}
	neurodeskMarker := managedSSHConfigBegin()
	if squadMarker == neurodeskMarker {
		t.Fatalf("managed SSH markers collide: %q", squadMarker)
	}
}
