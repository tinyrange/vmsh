//go:build !embed_ccvm

package shell

import "testing"

func TestDefaultDaemonIdentityForDevelopmentBuild(t *testing.T) {
	if got := defaultDaemonIdentity(); got != "ccdev" {
		t.Fatalf("default daemon identity = %q, want ccdev", got)
	}
}
