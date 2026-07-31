package hostcompat

import "testing"

func TestDesktopMacOSRequirement(t *testing.T) {
	for _, version := range []string{"13.7.8", "14.7", "14", " 13.7.8 "} {
		if err := DesktopMacOSRequirement(version); err == nil {
			t.Errorf("macOS version %q was accepted", version)
		}
	}
	for _, version := range []string{"15", "15.0", "26.5.1", "", "unknown"} {
		if err := DesktopMacOSRequirement(version); err != nil {
			t.Errorf("macOS version %q was rejected: %v", version, err)
		}
	}
}
