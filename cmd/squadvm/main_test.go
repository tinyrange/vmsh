package main

import (
	"image"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/gowin/window"
	"j5.nz/cc/client"
	"j5.nz/cc/display"
)

func TestImagePipelineKeepsDownloadAndIndexProgressSeparate(t *testing.T) {
	progress := pullStartupProgress(client.ProgressEvent{
		Status:           "processing",
		Progress:         0.4,
		DownloadProgress: 0.6,
		IndexProgress:    0.2,
		BytesDownloaded:  600,
		BytesTotal:       1000,
		FilesDownloaded:  2,
		FilesTotal:       10,
	})
	if !progress.ImagePipeline {
		t.Fatal("image pipeline progress was not enabled")
	}
	if progress.DownloadProgress != 0.6 || progress.IndexProgress != 0.2 {
		t.Fatalf("pipeline progress = download %.2f, index %.2f", progress.DownloadProgress, progress.IndexProgress)
	}
}

func TestSquadVMUsesPersistentImageHomeByDefault(t *testing.T) {
	mounts, home, err := squadvmPersistentHome("research-desktop", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if home != "research-desktop" {
		t.Fatalf("persistent home identity = %q", home)
	}
	if len(mounts) != 1 {
		t.Fatalf("persistent mounts = %d, want 1", len(mounts))
	}
	if mounts[0].Name != "research-desktop" || mounts[0].Mount != "" {
		t.Fatalf("persistent mount = %+v", mounts[0])
	}
}

func TestSquadVMCanUseNamedOrEphemeralHome(t *testing.T) {
	mounts, home, err := squadvmPersistentHome("desktop-two", "shared-research", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Name != "shared-research" || home != "shared-research" {
		t.Fatalf("named persistent home = %+v, %q", mounts, home)
	}

	mounts, home, err = squadvmPersistentHome("desktop-two", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 || home != "" {
		t.Fatalf("ephemeral home = %+v, %q", mounts, home)
	}

	if _, _, err := squadvmPersistentHome("desktop-two", "shared-research", true); err == nil {
		t.Fatal("named and ephemeral home were accepted together")
	}
}

func TestSquadVMStorageShareIsWritableBySquadUser(t *testing.T) {
	source := filepath.Join(t.TempDir(), "squadvm-shared")
	share, err := squadvmStorageShare(source)
	if err != nil {
		t.Fatal(err)
	}
	if share.Source != source || share.Mount != "/shared" || !share.Writable {
		t.Fatalf("storage share = %+v", share)
	}
	if !share.MapOwner || share.OwnerUID != 1000 || share.OwnerGID != 1000 {
		t.Fatalf("storage ownership mapping = %+v", share)
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		t.Fatalf("created storage directory is unavailable: %v", err)
	}
}

func TestPlatformArgumentsDefaultToGraphicalDesktop(t *testing.T) {
	args := platformArguments(nil)
	if len(args) != 1 || args[0] != defaultSquadVMImage {
		t.Fatalf("default arguments = %q, want default SquadVM image", args)
	}

	explicit := []string{"--vnc", "example.invalid/image:tag"}
	args = platformArguments(explicit)
	if len(args) != len(explicit) {
		t.Fatalf("explicit arguments = %q, want %q", args, explicit)
	}
	for index := range explicit {
		if args[index] != explicit[index] {
			t.Fatalf("explicit arguments = %q, want %q", args, explicit)
		}
	}
}

func TestParseDisplaySizeRejectsUnusableFramebuffers(t *testing.T) {
	width, height, err := parseDisplaySize("1920x1080")
	if err != nil {
		t.Fatal(err)
	}
	if width != 1920 || height != 1080 {
		t.Fatalf("display = %dx%d", width, height)
	}
	for _, value := range []string{"1920", "0x1080", "9000x1080"} {
		if _, _, err := parseDisplaySize(value); err == nil {
			t.Fatalf("invalid display %q was accepted", value)
		}
	}
}

func TestGuestDisplayUsesLogicalResolution(t *testing.T) {
	if got := guestDisplaySize(2880, 1800, 2); got != image.Pt(1440, 900) {
		t.Fatalf("guest display size = %v, want logical resolution", got)
	}
}

func TestGeneratedVNCPasswordFitsProtocol(t *testing.T) {
	password, err := generateVNCPassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(password) != 8 {
		t.Fatalf("generated VNC password has %d bytes, want 8", len(password))
	}
}

func TestModifierTransitionsPreserveFastControlShortcut(t *testing.T) {
	down, ok := modifierTransitionDown(window.KeyLeftControl, window.ModCtrl, false)
	if !ok || !down {
		t.Fatalf("Ctrl-down transition = (%t, %t)", down, ok)
	}
	up, ok := modifierTransitionDown(window.KeyLeftControl, 0, down)
	if !ok || up {
		t.Fatalf("Ctrl-up transition = (%t, %t)", up, ok)
	}
}

func TestDesktopPresentationWaitsForReadyCompleteSettledFrame(t *testing.T) {
	now := time.Now()
	full := display.FramebufferUpdate{
		Width:  800,
		Height: 600,
		Rect:   image.Rect(0, 0, 800, 600),
		Pixels: make([]byte, 800*600*4),
	}
	var gate desktopPresentationGate

	gate.observe(full, now)
	if gate.ready(now.Add(time.Second)) {
		t.Fatal("desktop frame was presented before the guest session was ready")
	}

	gate.markGuestReady()
	partial := full
	partial.Rect = image.Rect(0, 0, 400, 600)
	partial.Pixels = partial.Pixels[:400*600*4]
	gate.observe(partial, now)
	if gate.ready(now.Add(time.Second)) {
		t.Fatal("partial desktop frame was presented")
	}

	gate.observe(full, now)
	if gate.ready(now.Add(desktopFrameSettleDelay - time.Millisecond)) {
		t.Fatal("desktop was presented before the complete frame settled")
	}
	if !gate.ready(now.Add(desktopFrameSettleDelay)) {
		t.Fatal("complete settled desktop frame was not presented")
	}
}

func TestSquadVMSSHConfigPreservesUserConfigurationAndUpdatesManagedHost(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const userConfig = "Host lab\n    HostName lab.example\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(home, "SquadVM", "squadvm_ed25519")
	if err := configureSquadVMSSHHost(home, identity, 22022); err != nil {
		t.Fatal(err)
	}
	if err := configureSquadVMSSHHost(home, identity, 22023); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sshDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	if !strings.Contains(config, userConfig) {
		t.Fatalf("user SSH configuration was not preserved:\n%s", config)
	}
	if strings.Count(config, squadVMSSHConfigBegin) != 1 || !strings.Contains(config, "Port 22023") || strings.Contains(config, "Port 22022") {
		t.Fatalf("managed SquadVM SSH configuration was not replaced:\n%s", config)
	}
	if err := removeSquadVMSSHHost(home); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(sshDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != userConfig {
		t.Fatalf("removing managed host changed user config: %q", data)
	}
}

func TestSquadVMSSHIdentityIsStableAndUsable(t *testing.T) {
	configDir := t.TempDir()
	path, firstPublic, err := ensureSquadVMSSHIdentity(configDir)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, secondPublic, err := ensureSquadVMSSHIdentity(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if path != secondPath || string(firstPublic) != string(secondPublic) {
		t.Fatal("SquadVM SSH identity changed when reused")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows protects the key with ACLs and does not report Unix permission bits.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("SSH private key mode = %o", info.Mode().Perm())
	}
}
