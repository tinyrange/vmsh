package main

import "testing"

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
