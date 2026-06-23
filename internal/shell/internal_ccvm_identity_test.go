//go:build embed_ccvm

package shell

import "testing"

func TestDefaultDaemonIdentityForEmbeddedBuild(t *testing.T) {
	if got := defaultDaemonIdentity(); got != "ccprod" {
		t.Fatalf("default daemon identity = %q, want ccprod", got)
	}
}
