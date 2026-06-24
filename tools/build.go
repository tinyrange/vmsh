///usr/bin/true; DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd); ROOT=$(dirname -- "$DIR"); cd "$ROOT" && VMSH_BUILD_SCRIPT_DIR="$DIR" exec /usr/bin/env go run ./tools "$@"

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type paths struct {
	root         string
	ccDir        string
	build        string
	ccBin        string
	ccvm         string
	vmsh         string
	initAMD64    string
	initARM64    string
	targetGOARCH string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "build.go: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	buildDir, args, err := parseBuildDirArg(args)
	if err != nil {
		return err
	}
	cmd := "build"
	if len(args) > 0 {
		switch args[0] {
		case "build", "run", "demo", "help", "-h", "--help":
			cmd = args[0]
			args = args[1:]
		}
	}

	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		printUsage()
		return nil
	}

	p, err := makePaths(buildDir)
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
	case "demo":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if demoWantsHelp(args) {
			printDemoUsage(os.Stderr)
			return nil
		}
		if err := build(p); err != nil {
			return err
		}
		return runDemo(p, args)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	return nil
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `usage:
  ./tools/build.go [--build-dir DIR] [build]
  ./tools/build.go [--build-dir DIR] run [vmsh args...]
  ./tools/build.go [--build-dir DIR] demo [demo args...]
  go run ./tools [--build-dir DIR] [build|run|demo] [...]

The default command is build. Outputs are written under build/vmsh unless
--build-dir or VMSH_BUILD_DIR is set.
The run command records an asciinema session to build/vmsh/session.cast unless
vmsh args already include -record/--record.
The demo command drives a real vmsh session through a PTY and writes a redacted
marketing/demo cast to build/vmsh/demo.cast.
`)
}

func makePaths(buildDirArg string) (paths, error) {
	root, err := findRoot()
	if err != nil {
		return paths{}, err
	}

	targetGOOS, err := goEnv("GOOS")
	if err != nil {
		return paths{}, err
	}
	targetGOARCH, err := goEnv("GOARCH")
	if err != nil {
		return paths{}, err
	}

	suffix := ""
	if targetGOOS == "windows" {
		suffix = ".exe"
	}

	buildDir := resolveBuildDir(root, buildDirArg)
	ccDir := filepath.Join(root, "cc")
	return paths{
		root:         root,
		ccDir:        ccDir,
		build:        buildDir,
		ccBin:        filepath.Join(buildDir, "cc"+suffix),
		ccvm:         filepath.Join(buildDir, "ccvm"+suffix),
		vmsh:         filepath.Join(buildDir, "vmsh"+suffix),
		initAMD64:    filepath.Join(buildDir, "init-linux-amd64"),
		initARM64:    filepath.Join(buildDir, "init-linux-arm64"),
		targetGOARCH: targetGOARCH,
	}, nil
}

func parseBuildDirArg(args []string) (string, []string, error) {
	var buildDir string
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}
		switch {
		case arg == "--build-dir" || arg == "-build-dir":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a directory", arg)
			}
			i++
			buildDir = args[i]
		case strings.HasPrefix(arg, "--build-dir="):
			buildDir = strings.TrimPrefix(arg, "--build-dir=")
		case strings.HasPrefix(arg, "-build-dir="):
			buildDir = strings.TrimPrefix(arg, "-build-dir=")
		default:
			out = append(out, arg)
		}
	}
	return buildDir, out, nil
}

func resolveBuildDir(root, buildDirArg string) string {
	buildDir := strings.TrimSpace(buildDirArg)
	if buildDir == "" {
		buildDir = strings.TrimSpace(os.Getenv("VMSH_BUILD_DIR"))
	}
	if buildDir == "" {
		return filepath.Join(root, "build", "vmsh")
	}
	buildDir = os.ExpandEnv(buildDir)
	if !filepath.IsAbs(buildDir) {
		buildDir = filepath.Join(root, buildDir)
	}
	return filepath.Clean(buildDir)
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
	if scriptDir := strings.TrimSpace(os.Getenv("VMSH_BUILD_SCRIPT_DIR")); scriptDir != "" {
		candidates = append(candidates, filepath.Dir(scriptDir))
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

	if err := step("build linux/arm64 guest init", func() error {
		return goBuild(p.ccDir, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64"}, p.initARM64, "./internal/cmd/init")
	}); err != nil {
		return err
	}
	if err := step("install linux/arm64 guest init", func() error {
		return copyFile(p.initARM64, filepath.Join(p.ccDir, "internal", "guestinit", "guest-init-linux-arm64"), 0o644)
	}); err != nil {
		return err
	}

	if err := step("build linux/amd64 guest init", func() error {
		return goBuild(p.ccDir, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"}, p.initAMD64, "./internal/cmd/init")
	}); err != nil {
		return err
	}
	if err := step("install linux/amd64 guest init", func() error {
		return copyFile(p.initAMD64, filepath.Join(p.ccDir, "internal", "guestinit", "guest-init-linux-amd64"), 0o644)
	}); err != nil {
		return err
	}

	for _, bsd := range []string{"openbsd", "freebsd", "netbsd"} {
		bsd := bsd
		out := filepath.Join(p.build, "guest-init-"+bsd+"-"+p.targetGOARCH)
		if err := step("build "+bsd+"/"+p.targetGOARCH+" guest init", func() error {
			return goBuild(p.ccDir, []string{"CGO_ENABLED=0", "GOOS=" + bsd, "GOARCH=" + p.targetGOARCH}, out, "./internal/cmd/"+bsd+"-init")
		}); err != nil {
			return err
		}
		if err := step("install "+bsd+"/"+p.targetGOARCH+" guest init", func() error {
			return copyFile(out, filepath.Join(p.ccDir, "internal", bsd, "guestinit", "guest-init-"+bsd+"-"+p.targetGOARCH), 0o644)
		}); err != nil {
			return err
		}
	}

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
	if !hasRecordArg(args) {
		vmshArgs = append([]string{"-ccvm", p.ccvm, "-record", filepath.Join(p.build, "session.cast")}, args...)
	}
	logf("run: %s %s", p.vmsh, strings.Join(vmshArgs, " "))
	cmd := exec.Command(p.vmsh, vmshArgs...)
	cmd.Dir = p.root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func hasRecordArg(args []string) bool {
	for i, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-record" || arg == "--record" || arg == "-recording" || arg == "--recording" {
			return true
		}
		if strings.HasPrefix(arg, "-record=") || strings.HasPrefix(arg, "--record=") {
			return true
		}
		if strings.HasPrefix(arg, "-recording=") || strings.HasPrefix(arg, "--recording=") {
			return true
		}
		if (arg == "-h" || arg == "--help") && i == 0 {
			return true
		}
	}
	return false
}
