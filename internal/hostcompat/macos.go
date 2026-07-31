package hostcompat

import (
	"fmt"
	"strconv"
	"strings"
)

const MinimumDesktopMacOSMajor = 15

// DesktopMacOSRequirement returns an actionable compatibility error for a
// known macOS version that predates the Hypervisor.framework GIC APIs used by
// the desktop applications. An unknown version is left to the backend's
// capability checks rather than preventing startup.
func DesktopMacOSRequirement(version string) error {
	version = strings.TrimSpace(version)
	majorText, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorText)
	if err != nil || major <= 0 {
		return nil
	}
	if major >= MinimumDesktopMacOSMajor {
		return nil
	}
	return fmt.Errorf(
		"macOS %d or newer is required; this Mac is running macOS %s. Update macOS and try again",
		MinimumDesktopMacOSMajor,
		version,
	)
}
