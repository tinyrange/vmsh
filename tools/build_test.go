package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseBuildDirArg(t *testing.T) {
	buildDir, args, err := parseBuildDirArg([]string{"--build-dir", "out", "run", "--", "--build-dir", "vmsh-arg"})
	if err != nil {
		t.Fatalf("parse build dir: %v", err)
	}
	if buildDir != "out" {
		t.Fatalf("build dir = %q, want out", buildDir)
	}
	wantArgs := []string{"run", "--", "--build-dir", "vmsh-arg"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %q, want %q", args, wantArgs)
	}
}

func TestResolveBuildDir(t *testing.T) {
	root := filepath.Join("tmp", "repo")
	t.Setenv("VMSH_BUILD_DIR", "env-build")
	if got := resolveBuildDir(root, "flag-build"); got != filepath.Join(root, "flag-build") {
		t.Fatalf("flag build dir = %q", got)
	}
	if got := resolveBuildDir(root, ""); got != filepath.Join(root, "env-build") {
		t.Fatalf("env build dir = %q", got)
	}
	t.Setenv("VMSH_BUILD_DIR", "")
	if got := resolveBuildDir(root, ""); got != filepath.Join(root, "build", "vmsh") {
		t.Fatalf("default build dir = %q", got)
	}
}

func TestHasRecordArg(t *testing.T) {
	if !hasRecordArg([]string{"-record", "session.cast"}) {
		t.Fatalf("-record was not detected")
	}
	if !hasRecordArg([]string{"--record=session.cast"}) {
		t.Fatalf("--record= was not detected")
	}
	if hasRecordArg([]string{"--", "-record", "script-arg"}) {
		t.Fatalf("-record after -- should not be treated as a build wrapper vmsh flag")
	}
	if hasRecordArg([]string{"@alpine"}) {
		t.Fatalf("unexpected record arg detected")
	}
}

func TestParseDemoArgs(t *testing.T) {
	p := paths{root: filepath.Join("tmp", "repo"), build: filepath.Join("tmp", "repo", "build")}
	opts, err := parseDemoArgs(p, []string{
		"--out", "demo/out.cast",
		"--raw=demo/raw.cast",
		"--gif", "demo/out.gif",
		"--no-gif",
		"--keep-raw",
		"--live",
		"--vm-image", "ubuntu",
		"--memory=2g",
		"--timeout", "30s",
	})
	if err != nil {
		t.Fatalf("parse demo args: %v", err)
	}
	if opts.out != filepath.Join(p.root, "demo", "out.cast") {
		t.Fatalf("out = %q", opts.out)
	}
	if opts.raw != filepath.Join(p.root, "demo", "raw.cast") {
		t.Fatalf("raw = %q", opts.raw)
	}
	if opts.gif != filepath.Join(p.root, "demo", "out.gif") {
		t.Fatalf("gif = %q", opts.gif)
	}
	if !opts.noGIF || !opts.keepRaw || !opts.live {
		t.Fatalf("boolean options not set: %+v", opts)
	}
	if opts.vmImage != "ubuntu" || opts.vmMemory != "2g" || opts.timeout != 30*time.Second {
		t.Fatalf("options = %+v", opts)
	}
}

func TestRedactDemoCast(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.cast")
	out := filepath.Join(dir, "out.cast")
	p := paths{root: filepath.Join(dir, "repo")}
	input := `{"version":2,"width":80,"height":24}` + "\n" +
		`[0.1,"o","` + p.root + ` /tmp/d123/h 127.0.0.1:49152"]` + "\n"
	if err := os.WriteFile(raw, []byte(input), 0o644); err != nil {
		t.Fatalf("write raw cast: %v", err)
	}
	if err := redactDemoCast(raw, out, p, "127.0.0.1:49152"); err != nil {
		t.Fatalf("redact cast: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read redacted cast: %v", err)
	}
	text := string(data)
	for _, sensitive := range []string{p.root, "127.0.0.1:49152"} {
		if strings.Contains(text, sensitive) {
			t.Fatalf("redacted cast still contains %q: %s", sensitive, text)
		}
	}
	if !strings.Contains(text, "/work/vmsh") || !strings.Contains(text, "/tmp/d123/h") || !strings.Contains(text, "127.0.0.1:2222") {
		t.Fatalf("redacted cast missing replacements: %s", text)
	}
	if strings.Contains(text, "[0.1,") {
		t.Fatalf("redacted cast was not timeline-normalized: %s", text)
	}
	if !strings.Contains(text, "[0,") {
		t.Fatalf("redacted cast missing zero-based first event: %s", text)
	}
}

func TestNormalizeDemoCastTimeline(t *testing.T) {
	input := `{"version":2,"width":80,"height":24}` + "\n" +
		`[0.25,"o","fi"]` + "\n" +
		`[0.26,"o","rst"]` + "\n" +
		`[1.0,"o","second"]` + "\n"
	got := normalizeDemoCastTimeline(input)
	if !strings.Contains(got, `[0,"o","first"]`) {
		t.Fatalf("nearby output events were not shifted and merged: %s", got)
	}
	if !strings.Contains(got, `[0.75,"o","second"]`) {
		t.Fatalf("later event was not shifted by first timestamp: %s", got)
	}
}

func TestParseSSHExecPayload(t *testing.T) {
	payload := make([]byte, 4+len("hostname"))
	binary.BigEndian.PutUint32(payload[:4], uint32(len("hostname")))
	copy(payload[4:], "hostname")
	got, err := parseSSHExecPayload(payload)
	if err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if got != "hostname" {
		t.Fatalf("payload = %q", got)
	}
	if _, err := parseSSHExecPayload([]byte{0, 0, 0, 9, 'h'}); err == nil {
		t.Fatalf("truncated payload succeeded")
	}
}
