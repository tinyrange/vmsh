package version

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestFillFromBuildInfoUsesVCSMetadata(t *testing.T) {
	info := fillFromBuildInfo(Info{Version: "devel"}, &debug.BuildInfo{
		GoVersion: "go1.test",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "vcs.time", Value: "2026-07-05T00:00:00Z"},
		},
	})

	if info.Commit != "abc123" || !info.Dirty || info.BuildDate != "2026-07-05T00:00:00Z" {
		t.Fatalf("info = %+v", info)
	}
}

func TestExplicitMetadataOverridesBuildInfo(t *testing.T) {
	info := fillFromBuildInfo(Info{
		Version:   "v1.2.3",
		Commit:    "explicit",
		BuildDate: "2026-07-05T01:00:00Z",
	}, &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "vcs.time", Value: "2026-07-05T00:00:00Z"},
		},
	})

	if info.Version != "v1.2.3" || info.Commit != "explicit" || info.BuildDate != "2026-07-05T01:00:00Z" {
		t.Fatalf("info = %+v", info)
	}
}

func TestInfoStringReportsStableFields(t *testing.T) {
	out := Info{
		Version:   "v1.2.3",
		Commit:    "abc123",
		Dirty:     true,
		BuildDate: "2026-07-05T00:00:00Z",
		GoVersion: "go1.test",
		GOOS:      "linux",
		GOARCH:    "amd64",
	}.String()
	fields := outputFields(out)
	want := map[string]string{
		"version":  "v1.2.3",
		"commit":   "abc123",
		"dirty":    "true",
		"built":    "2026-07-05T00:00:00Z",
		"go":       "go1.test",
		"platform": "linux/amd64",
	}
	for key, value := range want {
		if fields[key] != value {
			t.Fatalf("field %s = %q, want %q; output:\n%s", key, fields[key], value, out)
		}
	}
}

func TestCurrentIncludesRuntimePlatform(t *testing.T) {
	info := Current()
	if info.GOOS != runtime.GOOS || info.GOARCH != runtime.GOARCH || info.GoVersion == "" {
		t.Fatalf("current info = %+v", info)
	}
}

func outputFields(out string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) >= 3 && parts[0] == "vmsh" && parts[1] == "version" {
			fields["version"] = parts[2]
			continue
		}
		if len(parts) >= 2 {
			fields[parts[0]] = parts[1]
		}
	}
	return fields
}
