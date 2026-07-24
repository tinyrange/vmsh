package main

import "testing"

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
