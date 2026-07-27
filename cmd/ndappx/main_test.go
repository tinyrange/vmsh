package main

import (
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tinyrange/gowin/window"
	"j5.nz/cc/display"
)

func TestNDAppXUsesPersistentImageHomeByDefault(t *testing.T) {
	mounts, home, err := ndappxPersistentHome("research-desktop", "", false)
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

func TestNDAppXCanUseNamedOrEphemeralHome(t *testing.T) {
	mounts, home, err := ndappxPersistentHome("desktop-two", "shared-research", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Name != "shared-research" || home != "shared-research" {
		t.Fatalf("named persistent home = %+v, %q", mounts, home)
	}

	mounts, home, err = ndappxPersistentHome("desktop-two", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 0 || home != "" {
		t.Fatalf("ephemeral home = %+v, %q", mounts, home)
	}

	if _, _, err := ndappxPersistentHome("desktop-two", "shared-research", true); err == nil {
		t.Fatal("named and ephemeral home were accepted together")
	}
}

func TestNDAppXStorageShareIsWritableByJovyan(t *testing.T) {
	source := filepath.Join(t.TempDir(), "neurodesktop-storage")
	share, err := ndappxStorageShare(source)
	if err != nil {
		t.Fatal(err)
	}
	if share.Source != source || share.Mount != "/vmsh-neurodesktop-storage" || !share.Writable {
		t.Fatalf("storage share = %+v", share)
	}
	if !share.MapOwner || share.OwnerUID != 1000 || share.OwnerGID != 100 {
		t.Fatalf("storage ownership mapping = %+v", share)
	}
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		t.Fatalf("created storage directory is unavailable: %v", err)
	}
}

func TestPlatformArgumentsDefaultToGraphicalDesktop(t *testing.T) {
	args := platformArguments(nil)
	if len(args) != 1 || args[0] != defaultNeurodesktopImage {
		t.Fatalf("default arguments = %q, want default Neurodesktop image", args)
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
