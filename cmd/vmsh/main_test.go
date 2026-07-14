package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestVersionFlagPrintsBuildIdentity(t *testing.T) {
	cmd := exec.Command("go", "run", ".", "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("vmsh --version: %v\n%s", err, out)
	}
	fields := versionOutputFields(string(out))
	if fields["version"] == "" || fields["dirty"] == "" || fields["platform"] == "" || fields["go"] == "" || fields["ccvm"] == "" {
		t.Fatalf("version fields = %#v output:\n%s", fields, out)
	}
}

func versionOutputFields(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		if len(parts) >= 3 && parts[0] == "vmsh" && parts[1] == "version" {
			fields["version"] = parts[2]
			continue
		}
		if len(parts) >= 2 {
			fields[parts[0]] = strings.Join(parts[1:], " ")
		}
	}
	return fields
}
