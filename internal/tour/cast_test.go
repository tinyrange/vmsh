package tour

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateCastRejectsTourWithoutSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.cast")
	data := "{\"version\":2,\"width\":80,\"height\":24,\"vmsh_tour\":{\"schema\":1,\"id\":\"missing-sections\",\"title\":\"Missing sections\"}}\n" +
		"[0.1,\"o\",\"hello\"]\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCast(path); err == nil {
		t.Fatal("ValidateCast accepted a tour without guided sections")
	}
}
