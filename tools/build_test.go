package main

import (
	"path/filepath"
	"reflect"
	"testing"
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
