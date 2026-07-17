package vmconfig

import (
	"strings"

	"j5.nz/cc/client"
)

const IsolatedVMSuffix = "-isolated"

// IsolatedVMID returns the backend identity used by ordinary vmsh --isolated
// contexts. Isolation is a frontend policy, not a distinct backend VM type.
func IsolatedVMID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	return name + IsolatedVMSuffix
}

// IsolatedNetworkConfig is the normal vmsh --isolated network policy: guests
// can reach the internet but cannot reach host services unless the user grants
// a specific facility through an ordinary host-side vmsh command.
func IsolatedNetworkConfig() *client.NetworkConfig {
	return &client.NetworkConfig{Enabled: true, AllowInternet: true, BlockHostAccess: true}
}
