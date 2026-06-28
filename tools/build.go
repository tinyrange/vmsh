///usr/bin/true; exec /usr/bin/env go run "$0" "$@"

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type paths struct {
	root  string
	ccDir string
	build string
	ccBin string
	ccvm  string
	vmsh  string
}

type guestInitPayload struct {
	name       string
	goos       string
	goarch     string
	pkg        string
	installRel string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "build.go: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cmd := "build"
	if len(args) > 0 {
		switch args[0] {
		case "build", "run", "help", "-h", "--help":
			cmd = args[0]
			args = args[1:]
		}
	}

	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		printUsage()
		return nil
	}

	p, err := makePaths()
	if err != nil {
		return err
	}

	switch cmd {
	case "build":
		if err := build(p); err != nil {
			return err
		}
		fmt.Println(p.vmsh)
	case "run":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if err := build(p); err != nil {
			return err
		}
		return runVMSH(p, args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	return nil
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `usage:
  ./tools/build.go [build]
  ./tools/build.go run [vmsh args...]
  go run .\tools\build.go [build|run] [vmsh args...]

The default command is build. Outputs are written under build/vmsh.
`)
}

func makePaths() (paths, error) {
	root, err := findRoot()
	if err != nil {
		return paths{}, err
	}

	targetGOOS, err := goEnv("GOOS")
	if err != nil {
		return paths{}, err
	}

	suffix := ""
	if targetGOOS == "windows" {
		suffix = ".exe"
	}

	buildDir := filepath.Join(root, "build", "vmsh")
	ccDir := filepath.Join(root, "cc")
	return paths{
		root:  root,
		ccDir: ccDir,
		build: buildDir,
		ccBin: filepath.Join(buildDir, "cc"+suffix),
		ccvm:  filepath.Join(buildDir, "ccvm"+suffix),
		vmsh:  filepath.Join(buildDir, "vmsh"+suffix),
	}, nil
}

func findRoot() (string, error) {
	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}
	if arg0 := os.Args[0]; arg0 != "" {
		if abs, err := filepath.Abs(arg0); err == nil {
			candidates = append(candidates, filepath.Dir(filepath.Dir(abs)))
		}
	}

	for _, start := range candidates {
		dir := filepath.Clean(start)
		for {
			if isRepoRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	return "", errors.New("could not find vmsh repo root; run from inside the repository")
}

func isRepoRoot(dir string) bool {
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	if !strings.Contains(string(mod), "module github.com/tinyrange/vmsh") {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, "cc", "go.mod"))
	return err == nil
}

func build(p paths) error {
	logf("repo root: %s", p.root)
	logf("build dir: %s", p.build)
	if err := os.MkdirAll(p.build, 0o755); err != nil {
		return err
	}

	installedPayloads, err := buildGuestInitPayloads(p)
	if err != nil {
		return err
	}
	defer cleanupGuestInitPayloads(installedPayloads)

	if err := step("build ccvm with embedded guest init", func() error {
		return goBuild(p.ccDir, []string{"CGO_ENABLED=0"}, p.ccvm, "-tags", "embed_guestinit", "./cmd/ccvm")
	}); err != nil {
		return err
	}
	if err := step("build cc", func() error {
		return goBuild(p.ccDir, nil, p.ccBin, "./cmd/cc")
	}); err != nil {
		return err
	}
	if err := step("build vmsh", func() error {
		return goBuild(p.root, nil, p.vmsh, "./cmd/vmsh")
	}); err != nil {
		return err
	}

	targetGOOS, err := goEnv("GOOS")
	if err != nil {
		return err
	}
	if targetGOOS == "darwin" && runtime.GOOS == "darwin" {
		if err := step("codesign ccvm", func() error {
			return command(p.root, nil, "codesign", "-f", "-s", "-", "--entitlements", filepath.Join(p.root, "tools", "entitlements.xml"), p.ccvm)
		}); err != nil {
			return err
		}
	}

	logf("built cc: %s", p.ccBin)
	logf("built ccvm: %s", p.ccvm)
	logf("built vmsh: %s", p.vmsh)

	return nil
}

func guestInitPayloads() []guestInitPayload {
	payloads := []guestInitPayload{
		{
			name:       "linux/arm64 guest init",
			goos:       "linux",
			goarch:     "arm64",
			pkg:        "./internal/cmd/init",
			installRel: filepath.Join("internal", "guestinit", "guest-init-linux-arm64"),
		},
		{
			name:       "linux/amd64 guest init",
			goos:       "linux",
			goarch:     "amd64",
			pkg:        "./internal/cmd/init",
			installRel: filepath.Join("internal", "guestinit", "guest-init-linux-amd64"),
		},
	}
	for _, bsd := range []string{"openbsd", "freebsd", "netbsd"} {
		for _, arch := range []string{"arm64", "amd64"} {
			payloads = append(payloads, guestInitPayload{
				name:       bsd + "/" + arch + " guest init",
				goos:       bsd,
				goarch:     arch,
				pkg:        "./internal/cmd/" + bsd + "-init",
				installRel: filepath.Join("internal", bsd, "guestinit", "guest-init-"+bsd+"-"+arch),
			})
		}
	}
	return payloads
}

func buildGuestInitPayloads(p paths) ([]string, error) {
	var installed []string
	for _, payload := range guestInitPayloads() {
		out := filepath.Join(p.build, strings.ReplaceAll(payload.name, "/", "-"))
		if err := step("build "+payload.name, func() error {
			return goBuild(p.ccDir, []string{"CGO_ENABLED=0", "GOOS=" + payload.goos, "GOARCH=" + payload.goarch}, out, payload.pkg)
		}); err != nil {
			cleanupGuestInitPayloads(installed)
			return nil, err
		}
		installPath := filepath.Join(p.ccDir, payload.installRel)
		if err := step("install "+payload.name, func() error {
			return copyFile(out, installPath, 0o644)
		}); err != nil {
			cleanupGuestInitPayloads(installed)
			return nil, err
		}
		installed = append(installed, installPath)
	}
	return installed, nil
}

func cleanupGuestInitPayloads(paths []string) {
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logf("warning: remove generated guest init %s: %v", path, err)
		}
	}
}

func step(name string, fn func() error) error {
	start := time.Now()
	logf("start: %s", name)
	if err := fn(); err != nil {
		logf("failed: %s (%s)", name, formatDuration(time.Since(start)))
		return err
	}
	logf("done: %s (%s)", name, formatDuration(time.Since(start)))
	return nil
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "build.go: "+format+"\n", args...)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func goBuild(workDir string, env []string, output string, args ...string) error {
	goArgs := append([]string{"build", "-o", output}, args...)
	return command(workDir, env, "go", goArgs...)
}

func command(workDir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func goEnv(key string) (string, error) {
	cmd := exec.Command("go", "env", key)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", key, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(dst, mode)
}

func runVMSH(p paths, args []string) error {
	vmshArgs := append([]string{"-ccvm", p.ccvm}, args...)
	logf("run: %s %s", p.vmsh, strings.Join(vmshArgs, " "))
	cmd := exec.Command(p.vmsh, vmshArgs...)
	cmd.Dir = p.root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}
