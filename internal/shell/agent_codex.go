package shell

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"j5.nz/cc/client"
)

const (
	codexGuestHomeMount       = "/vmsh/codex-home"
	codexGuestStandaloneMount = "/vmsh/codex-standalone"
	codexGuestCertMount       = "/vmsh/host-certs"
	codexStandaloneDir        = "packages/standalone"
)

var (
	codexVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta)(\.[0-9]+)?)?$`)
	codexGitHubAPI      = "https://api.github.com/repos/openai/codex"
	codexHTTPClient     = &http.Client{Timeout: 2 * time.Minute}
)

var (
	codexAgentSeedFiles     = []string{"auth.json", "config.toml", "AGENTS.md", "installation_id", "models_cache.json", "version.json"}
	codexAgentSeedDirs      = []string{"rules", "skills"}
	codexAgentSeedOnceFiles = []string{"history.jsonl", "session_index.jsonl"}
	codexAgentSeedOnceDirs  = []string{"sessions"}
)

type codexAgentOptions struct {
	Release   string
	Update    bool
	NoInstall bool
	Proxy     bool
	Args      []string
}

type codexHostRelease struct {
	Version       string
	Target        string
	Name          string
	ReleaseDir    string
	CodexRelPath  string
	CodexGuestBin string
}

type codexGitHubRelease struct {
	TagName string             `json:"tag_name"`
	Assets  []codexGitHubAsset `json:"assets"`
}

type codexGitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

func (s *shellState) runAgent(at atLine, stdout, stderr io.Writer) error {
	opts, err := parseCodexAgentCommand(at.Command)
	if err != nil {
		return err
	}
	progress := newTerminalHoldStatus(stderr, "Codex: preparing agent")
	defer progress.Close()
	if at.Options.AgentProxy {
		opts.Proxy = true
	}
	ctx := s.context.withOptions(at.Options)
	ctx.Mode = modeVM
	if ctx.Image == "" {
		return fmt.Errorf("no guest image selected; run @<oci-tag> first")
	}
	if at.Options.Network == nil {
		ctx.Network = true
	}
	if ctx.Isolated {
		opts.Proxy = true
	}
	hostCodexHome, err := hostCodexHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hostCodexHome, 0o700); err != nil {
		return err
	}
	progress.Update("Codex: ensuring guest image")
	if err := s.ensureImageAvailable(ctx, stderr); err != nil {
		return err
	}
	progress.Update("Codex: starting guest VM")
	if err := s.ensureVMRunning(ctx, stderr); err != nil {
		return err
	}
	progress.Update("Codex: detecting guest platform")
	target, err := s.detectGuestCodexTarget(ctx)
	if err != nil {
		return err
	}
	progress.Update("Codex: ensuring CLI release for " + target)
	release, err := ensureHostCodexRelease(hostCodexHome, target, opts, stderr)
	if err != nil {
		return err
	}
	if opts.Proxy {
		progress.Update("Codex: starting host auth proxy")
		proxy, err := startCodexAgentProxy(hostCodexHome)
		if err != nil {
			return err
		}
		defer proxy.Close()
		progress.Update("Codex: allowing proxy access from guest")
		if err := s.allowRunningCodexAgentProxyPort(ctx, proxy.Port()); err != nil {
			return err
		}
		progress.Update("Codex: preparing isolated guest launch")
		req, err := s.prepareCodexAgentProxyRunRequest(ctx, hostCodexHome, release, opts.Args, proxy.Port(), proxy.Token(), stdout, stderr)
		if err != nil {
			return err
		}
		progress.Update("Codex: launching guest agent")
		progress.Close()
		return s.streamGuestRun(backendVMID(ctx), req, stdout, stderr)
	}
	progress.Update("Codex: preparing guest home")
	agentHome, err := prepareCodexAgentHome(hostCodexHome, ctx, target)
	if err != nil {
		return err
	}
	progress.Update("Codex: preparing guest launch")
	req, err := s.prepareCodexAgentRunRequest(ctx, hostCodexHome, agentHome, release, opts.Args, stdout, stderr)
	if err != nil {
		return err
	}
	progress.Update("Codex: launching guest agent")
	progress.Close()
	return s.streamGuestRun(backendVMID(ctx), req, stdout, stderr)
}

func (s *shellState) allowRunningCodexAgentProxyPort(ctx commandContext, port int) error {
	if !ctx.Isolated {
		return nil
	}
	state, err := s.api.InstanceStatusOf(backendVMID(ctx))
	if err != nil || state.Status != "running" {
		return nil
	}
	if err := s.api.AllowServiceProxyPortTo(backendVMID(ctx), port); err != nil {
		return fmt.Errorf("allow Codex host proxy in isolated VM: %w", err)
	}
	return nil
}

func parseCodexAgentCommand(command string) (codexAgentOptions, error) {
	fields, err := splitShellFields(command)
	if err != nil {
		return codexAgentOptions{}, err
	}
	if len(fields) == 0 {
		return codexAgentOptions{}, fmt.Errorf("usage: @agent [--proxy] codex [--release version] [--update] [--no-install] [-- args...]")
	}
	opts := codexAgentOptions{Release: "latest"}
	i := 0
	for i < len(fields) && strings.HasPrefix(fields[i], "-") {
		switch fields[i] {
		case "--proxy":
			opts.Proxy = true
			i++
		default:
			return codexAgentOptions{}, fmt.Errorf("unsupported @agent option %q", fields[i])
		}
	}
	if i >= len(fields) {
		return codexAgentOptions{}, fmt.Errorf("usage: @agent [--proxy] codex [--release version] [--update] [--no-install] [-- args...]")
	}
	if fields[i] != "codex" {
		return codexAgentOptions{}, fmt.Errorf("unsupported agent %q", fields[i])
	}
	for i = i + 1; i < len(fields); i++ {
		field := fields[i]
		switch {
		case field == "--":
			opts.Args = append(opts.Args, fields[i+1:]...)
			return opts, nil
		case field == "--proxy":
			opts.Proxy = true
		case field == "--update":
			opts.Update = true
		case field == "--no-install":
			opts.NoInstall = true
		case field == "--release":
			if i+1 >= len(fields) {
				return codexAgentOptions{}, fmt.Errorf("--release requires a value")
			}
			i++
			opts.Release = fields[i]
		case strings.HasPrefix(field, "--release="):
			opts.Release = strings.TrimPrefix(field, "--release=")
			if opts.Release == "" {
				return codexAgentOptions{}, fmt.Errorf("--release requires a value")
			}
		default:
			opts.Args = append(opts.Args, fields[i:]...)
			return opts, nil
		}
	}
	return opts, nil
}

func (s *shellState) detectGuestCodexTarget(ctx commandContext) (string, error) {
	arch := normalizeVMSHArchitecture(ctx.Arch)
	if arch == "" {
		arch = runtime.GOARCH
	}
	return codexGuestTarget("linux", arch)
}

func codexGuestTarget(osName, machine string) (string, error) {
	osName = strings.ToLower(strings.TrimSpace(osName))
	machine = strings.ToLower(strings.TrimSpace(machine))
	if osName != "linux" {
		return "", fmt.Errorf("@agent codex supports Linux guests, got %s", osName)
	}
	switch machine {
	case "aarch64", "arm64":
		return "aarch64-unknown-linux-musl", nil
	case "x86_64", "amd64":
		return "x86_64-unknown-linux-musl", nil
	default:
		return "", fmt.Errorf("@agent codex does not support guest architecture %s", machine)
	}
}

func hostCodexHomeDir() (string, error) {
	if value := strings.TrimSpace(os.Getenv("CODEX_HOME")); value != "" {
		return filepath.Abs(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func ensureHostCodexRelease(hostCodexHome, target string, opts codexAgentOptions, stderr io.Writer) (codexHostRelease, error) {
	release := normalizeCodexRelease(opts.Release)
	if !codexVersionValid(release) {
		return codexHostRelease{}, fmt.Errorf("invalid Codex release version %q", opts.Release)
	}
	standaloneRoot := filepath.Join(hostCodexHome, filepath.FromSlash(codexStandaloneDir))
	releasesDir := filepath.Join(standaloneRoot, "releases")
	if release == "latest" && !opts.Update {
		if installed, ok := newestInstalledCodexRelease(releasesDir, target); ok {
			_ = updateVMShCodexLink(standaloneRoot, target, installed.Name)
			installed.CodexGuestBin = codexGuestBinaryPath(installed)
			return installed, nil
		}
	}
	if release != "latest" {
		name := release + "-" + target
		installed := codexHostRelease{
			Version:      release,
			Target:       target,
			Name:         name,
			ReleaseDir:   filepath.Join(releasesDir, name),
			CodexRelPath: "bin/codex",
		}
		if codexReleaseDirComplete(installed.ReleaseDir, release, target) {
			installed.CodexRelPath = codexReleaseBinaryRelPath(installed.ReleaseDir)
			_ = updateVMShCodexLink(standaloneRoot, target, installed.Name)
			installed.CodexGuestBin = codexGuestBinaryPath(installed)
			return installed, nil
		}
	}
	if opts.NoInstall {
		return codexHostRelease{}, fmt.Errorf("Codex is not installed for guest target %s", target)
	}
	if err := os.MkdirAll(standaloneRoot, 0o700); err != nil {
		return codexHostRelease{}, err
	}
	var installed codexHostRelease
	err := withCodexInstallLock(standaloneRoot, func() error {
		selectedRelease := release
		var metadata codexGitHubRelease
		var err error
		if selectedRelease == "latest" {
			metadata, selectedRelease, err = fetchLatestCodexRelease()
		} else {
			metadata, err = fetchCodexReleaseByVersion(selectedRelease)
		}
		if err != nil {
			return err
		}
		name := selectedRelease + "-" + target
		releaseDir := filepath.Join(releasesDir, name)
		if codexReleaseDirComplete(releaseDir, selectedRelease, target) {
			installed = codexHostRelease{Version: selectedRelease, Target: target, Name: name, ReleaseDir: releaseDir, CodexRelPath: codexReleaseBinaryRelPath(releaseDir)}
			return nil
		}
		fmt.Fprintf(stderr, "Installing Codex CLI %s for %s\n", selectedRelease, target)
		if err := installCodexPackageRelease(metadata, releasesDir, selectedRelease, target); err != nil {
			return err
		}
		if !codexReleaseDirComplete(releaseDir, selectedRelease, target) {
			return fmt.Errorf("installed Codex release %s is incomplete", name)
		}
		installed = codexHostRelease{Version: selectedRelease, Target: target, Name: name, ReleaseDir: releaseDir, CodexRelPath: codexReleaseBinaryRelPath(releaseDir)}
		return nil
	})
	if err != nil {
		return codexHostRelease{}, err
	}
	_ = updateVMShCodexLink(standaloneRoot, target, installed.Name)
	installed.CodexGuestBin = codexGuestBinaryPath(installed)
	return installed, nil
}

func normalizeCodexRelease(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "", value == "latest":
		return "latest"
	case strings.HasPrefix(value, "rust-v"):
		return strings.TrimPrefix(value, "rust-v")
	case strings.HasPrefix(value, "v"):
		return strings.TrimPrefix(value, "v")
	default:
		return value
	}
}

func codexVersionValid(value string) bool {
	return value == "latest" || codexVersionPattern.MatchString(value)
}

func newestInstalledCodexRelease(releasesDir, target string) (codexHostRelease, bool) {
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return codexHostRelease{}, false
	}
	var releases []codexHostRelease
	suffix := "-" + target
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		version := strings.TrimSuffix(name, suffix)
		if !codexVersionValid(version) {
			continue
		}
		releaseDir := filepath.Join(releasesDir, name)
		if !codexReleaseDirComplete(releaseDir, version, target) {
			continue
		}
		releases = append(releases, codexHostRelease{
			Version:      version,
			Target:       target,
			Name:         name,
			ReleaseDir:   releaseDir,
			CodexRelPath: codexReleaseBinaryRelPath(releaseDir),
		})
	}
	if len(releases) == 0 {
		return codexHostRelease{}, false
	}
	sort.Slice(releases, func(i, j int) bool {
		return compareCodexVersions(releases[i].Version, releases[j].Version) > 0
	})
	return releases[0], true
}

func codexReleaseDirComplete(releaseDir, expectedVersion, expectedTarget string) bool {
	if filepath.Base(releaseDir) != expectedVersion+"-"+expectedTarget {
		return false
	}
	if !isExecutable(filepath.Join(releaseDir, "bin", "codex")) {
		return false
	}
	if !isExecutable(filepath.Join(releaseDir, "codex")) {
		return false
	}
	if !isExecutable(filepath.Join(releaseDir, "codex-path", "rg")) {
		return false
	}
	if strings.Contains(expectedTarget, "linux") && !isExecutable(filepath.Join(releaseDir, "codex-resources", "bwrap")) {
		return false
	}
	if _, err := os.Stat(filepath.Join(releaseDir, "codex-package.json")); err != nil {
		return false
	}
	return true
}

func codexReleaseBinaryRelPath(releaseDir string) string {
	if isExecutable(filepath.Join(releaseDir, "bin", "codex")) {
		return "bin/codex"
	}
	return "codex"
}

func isExecutable(file string) bool {
	info, err := os.Stat(file)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}

func codexGuestBinaryPath(release codexHostRelease) string {
	return path.Join(codexGuestStandaloneMount, "releases", release.Name, filepath.ToSlash(release.CodexRelPath))
}

func prepareCodexAgentHome(hostCodexHome string, ctx commandContext, target string) (string, error) {
	home := codexAgentHomeDir(hostCodexHome, ctx, target)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", err
	}
	for _, name := range codexAgentSeedFiles {
		if err := copyCodexHomePathIfExists(hostCodexHome, home, name, true); err != nil {
			return "", err
		}
	}
	for _, name := range codexAgentSeedDirs {
		if err := copyCodexHomePathIfExists(hostCodexHome, home, name, false); err != nil {
			return "", err
		}
	}
	for _, name := range codexAgentSeedOnceFiles {
		if err := copyCodexHomePathIfExists(hostCodexHome, home, name, false); err != nil {
			return "", err
		}
	}
	for _, name := range codexAgentSeedOnceDirs {
		if err := copyCodexHomePathIfExists(hostCodexHome, home, name, false); err != nil {
			return "", err
		}
	}
	for _, dir := range []string{".tmp", "tmp", "cache", "log"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return "", err
		}
	}
	if err := ensureCodexAgentStandaloneLink(home); err != nil {
		return "", err
	}
	return home, nil
}

func codexAgentHomeDir(hostCodexHome string, ctx commandContext, target string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		backendVMID(ctx),
		localImageName(ctx.Image, ctx.Arch),
		guestRunUser(ctx),
		target,
	}, "\x00")))
	name := sanitizeCodexAgentHomeName(backendVMID(ctx)) + "-" + hex.EncodeToString(sum[:8])
	return filepath.Join(hostCodexHome, filepath.FromSlash(codexStandaloneDir), "vmsh", "agent-homes", name)
}

func sanitizeCodexAgentHomeName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "default"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

func copyCodexHomePathIfExists(srcRoot, dstRoot, name string, overwrite bool) error {
	src := filepath.Join(srcRoot, filepath.FromSlash(name))
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dst := filepath.Join(dstRoot, filepath.FromSlash(name))
	if !overwrite {
		if _, err := os.Lstat(dst); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if info.IsDir() {
		return copyHostDir(src, dst, info.Mode())
	}
	if info.Mode().IsRegular() {
		return copyHostFile(src, dst, info.Mode())
	}
	return nil
}

func ensureCodexAgentStandaloneLink(agentHome string) error {
	packagesDir := filepath.Join(agentHome, "packages")
	if err := os.MkdirAll(packagesDir, 0o700); err != nil {
		return err
	}
	link := filepath.Join(packagesDir, "standalone")
	if _, err := os.Lstat(link); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(codexGuestStandaloneMount, link)
}

func trustCodexAgentProject(agentHome, guestWorkDir string) error {
	guestWorkDir = strings.TrimSpace(guestWorkDir)
	if guestWorkDir == "" {
		return nil
	}
	key, err := tomlBasicString(guestWorkDir)
	if err != nil {
		return err
	}
	configPath := filepath.Join(agentHome, "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	header := "[projects." + key + "]"
	if strings.Contains(string(data), header) {
		return nil
	}
	file, err := os.OpenFile(configPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		if _, err := file.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(file, "\n%s\ntrust_level = \"trusted\"\n", header)
	return err
}

func tomlBasicString(value string) (string, error) {
	for _, r := range value {
		if r < 0x20 && r != '\t' {
			return "", fmt.Errorf("path contains a control character")
		}
	}
	return strconv.Quote(value), nil
}

func (s *shellState) prepareCodexAgentRunRequest(ctx commandContext, hostCodexHome, agentHome string, release codexHostRelease, args []string, stdout, stderr io.Writer) (client.RunRequest, error) {
	tty, cols, rows := terminalRequestSize(stdout)
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	req, err := s.prepareGuestRunRequest(ctx, ":", true, cols, rows, stderr)
	if err != nil {
		return client.RunRequest{}, err
	}
	if err := trustCodexAgentProject(agentHome, req.WorkDir); err != nil {
		return client.RunRequest{}, err
	}
	commandLine := codexAgentCommandLine(release.CodexGuestBin, args)
	if codexAgentRunsAsRoot(ctx) {
		commandLine = codexRootAgentCommandLine(release, args)
	}
	req.Command = guestCommand(commandLine, true)
	req.TTY = true
	req.Cols = cols
	req.Rows = rows
	req.Shares = append(req.Shares, client.ShareMount{
		Source:   agentHome,
		Mount:    codexGuestHomeMount,
		Writable: true,
		MapOwner: true,
		OwnerUID: defaultGuestUID,
		OwnerGID: defaultGuestGID,
		Cache:    "strict",
	}, client.ShareMount{
		Source: filepath.Join(hostCodexHome, filepath.FromSlash(codexStandaloneDir)),
		Mount:  codexGuestStandaloneMount,
		Cache:  "strict",
	})
	req.Env = mergedEnv(req.Env, []string{"CODEX_HOME=" + codexGuestHomeMount})
	if certFile, ok := hostCABundlePath(hostCodexHome); ok {
		guestCertFile := path.Join(codexGuestCertMount, filepath.Base(certFile))
		req.Shares = append(req.Shares, client.ShareMount{
			Source: filepath.Dir(certFile),
			Mount:  codexGuestCertMount,
			Cache:  "strict",
		})
		req.Env = mergedEnv(req.Env, codexCertificateEnv(guestCertFile))
	}
	if !tty {
		req.Env = mergedEnv(req.Env, terminalEnv(cols, rows))
	}
	return req, nil
}

func (s *shellState) prepareCodexAgentProxyRunRequest(ctx commandContext, hostCodexHome string, release codexHostRelease, args []string, proxyPort int, proxyToken string, stdout, stderr io.Writer) (client.RunRequest, error) {
	tty, cols, rows := terminalRequestSize(stdout)
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	rootAgent := codexAgentRunsAsRoot(ctx)
	launchCtx := ctx
	if rootAgent && ctx.Isolated {
		launchCtx.User = "0:0"
	}
	req, err := s.prepareGuestRunRequest(launchCtx, ":", true, cols, rows, stderr)
	if err != nil {
		return client.RunRequest{}, err
	}
	if req.Network == nil || !req.Network.Enabled {
		return client.RunRequest{}, fmt.Errorf("@agent --proxy codex requires guest networking")
	}
	req.Network.AllowedServiceProxyPorts = appendAllowedServiceProxyPort(req.Network.AllowedServiceProxyPorts, proxyPort)
	proxyHome := codexGuestProxyHomeDir(ctx)
	ensureGitWorkDir := ctx.Isolated
	agentWorkDir := req.WorkDir
	if rootAgent && ctx.Isolated {
		agentWorkDir = firstNonEmpty(ctx.CWD, guestHomeDir(ctx))
	}
	commandLine, err := codexProxyAgentCommandLine(proxyHome, release.CodexGuestBin, args, proxyPort, agentWorkDir, ensureGitWorkDir)
	if err != nil {
		return client.RunRequest{}, err
	}
	if rootAgent {
		commandLine, err = codexProxyRootAgentCommandLine(proxyHome, release, args, proxyPort, agentWorkDir, ensureGitWorkDir)
		if err != nil {
			return client.RunRequest{}, err
		}
	}
	req.Command = guestCommand(commandLine, true)
	req.TTY = true
	req.Cols = cols
	req.Rows = rows
	req.Shares = append(req.Shares, client.ShareMount{
		Source: filepath.Join(hostCodexHome, filepath.FromSlash(codexStandaloneDir), "releases", release.Name),
		Mount:  path.Join(codexGuestStandaloneMount, "releases", release.Name),
		Cache:  "strict",
	})
	req.Env = mergedEnv(req.Env, []string{
		"CODEX_HOME=" + proxyHome,
		codexAgentProxyTokenEnv + "=" + proxyToken,
	})
	if !tty {
		req.Env = mergedEnv(req.Env, terminalEnv(cols, rows))
	}
	return req, nil
}

func appendAllowedServiceProxyPort(ports []int, port int) []int {
	if port <= 0 || port > 65535 {
		return ports
	}
	for _, existing := range ports {
		if existing == port {
			return ports
		}
	}
	return append(ports, port)
}

func codexAgentRunsAsRoot(ctx commandContext) bool {
	user := strings.TrimSpace(guestRunUser(ctx))
	return user == "root" || user == "0" || strings.HasPrefix(user, "0:")
}

func codexGuestProxyHomeDir(ctx commandContext) string {
	return path.Join(guestHomeDir(ctx), ".vmsh", "codex")
}

func codexRootAgentCommandLine(release codexHostRelease, args []string) string {
	sourceRoot := path.Join(codexGuestStandaloneMount, "releases", release.Name)
	stageRoot := path.Join("/run/vmsh-codex", release.Name)
	stageBin := path.Join(stageRoot, filepath.ToSlash(release.CodexRelPath))
	fields := []string{
		codexShellStatusCommand("Codex: preparing sudo staging area"),
		"rm -rf -- " + shellQuote(stageRoot),
		"mkdir -p -- " + shellQuote(path.Dir(stageRoot)),
		codexShellRunWithSpinnerCommand("Codex: staging CLI release for sudo", "cp -a -- "+shellQuote(sourceRoot)+" "+shellQuote(stageRoot)),
		codexShellStatusCommand("Codex: starting"),
		codexShellClearStatusCommand(),
		codexAgentCommandLine(stageBin, args),
	}
	return strings.Join(fields, "\n")
}

func codexProxyRootAgentCommandLine(proxyHome string, release codexHostRelease, args []string, proxyPort int, guestWorkDir string, ensureGitWorkDir bool) (string, error) {
	sourceRoot := path.Join(codexGuestStandaloneMount, "releases", release.Name)
	stageRoot := path.Join("/run/vmsh-codex", release.Name)
	stageBin := path.Join(stageRoot, filepath.ToSlash(release.CodexRelPath))
	agentCommand, err := codexProxyAgentCommandLine(proxyHome, stageBin, args, proxyPort, guestWorkDir, ensureGitWorkDir)
	if err != nil {
		return "", err
	}
	fields := []string{
		codexShellStatusCommand("Codex: preparing sudo staging area"),
		"rm -rf -- " + shellQuote(stageRoot),
		"mkdir -p -- " + shellQuote(path.Dir(stageRoot)),
		codexShellRunWithSpinnerCommand("Codex: staging CLI release for sudo", "cp -a -- "+shellQuote(sourceRoot)+" "+shellQuote(stageRoot)),
		agentCommand,
	}
	return strings.Join(fields, "\n"), nil
}

func codexAgentCommandLine(binary string, args []string) string {
	fields := []string{
		"export CODEX_HOME=" + shellQuote(codexGuestHomeMount),
		"export PATH=" + shellQuote(strings.Join(codexAgentPathDirs(binary), ":")) + `:"$PATH"`,
	}
	fields = append(fields, "exec "+shellQuote(binary))
	for _, arg := range args {
		fields[len(fields)-1] += " " + shellQuote(arg)
	}
	return strings.Join(fields, "; ")
}

func codexProxyAgentCommandLine(proxyHome, binary string, args []string, proxyPort int, guestWorkDir string, ensureGitWorkDir bool) (string, error) {
	config, err := codexProxyAgentConfig(proxyPort, guestWorkDir)
	if err != nil {
		return "", err
	}
	execLine := "exec " + shellQuote(binary)
	for _, arg := range args {
		execLine += " " + shellQuote(arg)
	}
	fields := []string{
		codexShellStatusCommand("Codex: preparing proxy home"),
		"rm -rf -- " + shellQuote(proxyHome),
		"mkdir -p -- " + shellQuote(proxyHome),
		"umask 077",
		codexShellStatusCommand("Codex: writing proxy config"),
		"cat > " + shellQuote(path.Join(proxyHome, "config.toml")) + " <<'VMSH_CODEX_CONFIG'\n" + config + "VMSH_CODEX_CONFIG",
	}
	if ensureGitWorkDir {
		fields = append(fields, codexShellStatusCommand("Codex: preparing trusted workspace"))
		fields = append(fields, codexEnsureGitWorkDirCommand(guestWorkDir))
	}
	fields = append(fields,
		codexShellStatusCommand("Codex: starting"),
		codexShellClearStatusCommand(),
		"export CODEX_HOME="+shellQuote(proxyHome),
		"export PATH="+shellQuote(strings.Join(codexAgentPathDirs(binary), ":"))+`:"$PATH"`,
		execLine,
	)
	return strings.Join(fields, "\n"), nil
}

func codexShellStatusCommand(message string) string {
	return "printf '\\r\\033[2K%s' " + shellQuote(message) + " >&2"
}

func codexShellClearStatusCommand() string {
	return "printf '\\r\\033[2K' >&2"
}

func codexShellRunWithSpinnerCommand(message, command string) string {
	message = shellQuote(message)
	return strings.Join([]string{
		"(" + command + ") &",
		"__vmsh_codex_pid=$!",
		"__vmsh_codex_i=0",
		"while kill -0 \"$__vmsh_codex_pid\" 2>/dev/null; do",
		"  case $__vmsh_codex_i in 0) __vmsh_codex_frame=- ;; 1) __vmsh_codex_frame=+ ;; 2) __vmsh_codex_frame='|' ;; *) __vmsh_codex_frame=/ ;; esac",
		"  printf '\\r\\033[2K%s %s' \"$__vmsh_codex_frame\" " + message + " >&2",
		"  __vmsh_codex_i=$(( (__vmsh_codex_i + 1) % 4 ))",
		"  sleep 0.2",
		"done",
		"wait \"$__vmsh_codex_pid\"",
		"__vmsh_codex_status=$?",
		"unset __vmsh_codex_pid __vmsh_codex_i __vmsh_codex_frame",
		codexShellClearStatusCommand(),
		"if [ \"$__vmsh_codex_status\" -ne 0 ]; then exit \"$__vmsh_codex_status\"; fi",
		"unset __vmsh_codex_status",
	}, "\n")
}

func codexProxyAgentConfig(proxyPort int, guestWorkDir string) (string, error) {
	baseURL, err := tomlBasicString(fmt.Sprintf("http://10.42.0.100:%d/v1", proxyPort))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("model_provider = \"vmsh-host-proxy\"\n\n")
	b.WriteString("[model_providers.vmsh-host-proxy]\n")
	b.WriteString("name = \"vmsh host Codex proxy\"\n")
	b.WriteString("base_url = " + baseURL + "\n")
	b.WriteString("wire_api = \"responses\"\n")
	b.WriteString("requires_openai_auth = false\n")
	b.WriteString("supports_websockets = false\n")
	b.WriteString("request_max_retries = 0\n")
	b.WriteString("stream_max_retries = 5\n")
	b.WriteString("stream_idle_timeout_ms = 300000\n\n")
	b.WriteString("[model_providers.vmsh-host-proxy.env_http_headers]\n")
	b.WriteString("\"" + codexAgentProxyTokenHeader + "\" = \"" + codexAgentProxyTokenEnv + "\"\n")
	if strings.TrimSpace(guestWorkDir) != "" {
		key, err := tomlBasicString(guestWorkDir)
		if err != nil {
			return "", err
		}
		b.WriteString("\n[projects." + key + "]\n")
		b.WriteString("trust_level = \"trusted\"\n")
	}
	return b.String(), nil
}

func codexEnsureGitWorkDirCommand(guestWorkDir string) string {
	guestWorkDir = strings.TrimSpace(guestWorkDir)
	if guestWorkDir == "" {
		return ":"
	}
	gitDir := path.Join(guestWorkDir, ".git")
	fields := []string{
		"mkdir -p -- " + shellQuote(guestWorkDir),
		"if [ ! -e " + shellQuote(gitDir) + " ]; then",
		"  mkdir -p -- " + shellQuote(path.Join(gitDir, "refs", "heads")) + " " + shellQuote(path.Join(gitDir, "refs", "tags")) + " " + shellQuote(path.Join(gitDir, "objects")),
		"  printf '%s\\n' 'ref: refs/heads/main' > " + shellQuote(path.Join(gitDir, "HEAD")),
		"  cat > " + shellQuote(path.Join(gitDir, "config")) + " <<'VMSH_CODEX_GIT_CONFIG'\n[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\nVMSH_CODEX_GIT_CONFIG",
		"fi",
	}
	return strings.Join(fields, "\n")
}

func codexAgentPathDirs(binary string) []string {
	binDir := path.Dir(binary)
	releaseRoot := binDir
	if path.Base(binDir) == "bin" {
		releaseRoot = path.Dir(binDir)
	}
	return []string{
		binDir,
		path.Join(releaseRoot, "codex-resources"),
		path.Join(releaseRoot, "codex-path"),
	}
}

func hostCABundlePath(hostCodexHome string) (string, bool) {
	for _, env := range []string{"SSL_CERT_FILE", "NIX_SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE"} {
		if path, ok := readableHostFile(os.Getenv(env)); ok {
			return path, true
		}
	}
	for _, candidate := range []string{
		"/etc/ssl/cert.pem",
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/ssl/ca-bundle.pem",
		"/opt/homebrew/etc/ca-certificates/cert.pem",
		"/usr/local/etc/openssl@3/cert.pem",
		"/usr/local/etc/openssl/cert.pem",
	} {
		if path, ok := readableHostFile(candidate); ok {
			return path, true
		}
	}
	if runtime.GOOS == "darwin" {
		if path, err := writeDarwinSystemCABundle(hostCodexHome); err == nil {
			return path, true
		}
	}
	return "", false
}

func readableHostFile(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	path, err := filepath.Abs(value)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

func writeDarwinSystemCABundle(hostCodexHome string) (string, error) {
	out, err := exec.Command("security", "find-certificate", "-a", "-p", "/System/Library/Keychains/SystemRootCertificates.keychain").Output()
	if err != nil {
		return "", err
	}
	if !strings.Contains(string(out), "-----BEGIN CERTIFICATE-----") {
		return "", fmt.Errorf("security output did not contain PEM certificates")
	}
	dir := filepath.Join(hostCodexHome, "packages", "standalone", "vmsh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "host-ca-certificates.pem")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func codexCertificateEnv(guestCertFile string) []string {
	return []string{
		"SSL_CERT_FILE=" + guestCertFile,
		"NIX_SSL_CERT_FILE=" + guestCertFile,
		"CURL_CA_BUNDLE=" + guestCertFile,
		"REQUESTS_CA_BUNDLE=" + guestCertFile,
		"GIT_SSL_CAINFO=" + guestCertFile,
		"NODE_EXTRA_CA_CERTS=" + guestCertFile,
	}
}

func fetchLatestCodexRelease() (codexGitHubRelease, string, error) {
	release, err := fetchCodexGitHubRelease(codexGitHubAPI + "/releases/latest")
	if err != nil {
		return codexGitHubRelease{}, "", err
	}
	version := normalizeCodexRelease(release.TagName)
	if !codexVersionValid(version) || version == "latest" {
		return codexGitHubRelease{}, "", fmt.Errorf("latest Codex release has unexpected tag %q", release.TagName)
	}
	return release, version, nil
}

func fetchCodexReleaseByVersion(version string) (codexGitHubRelease, error) {
	return fetchCodexGitHubRelease(codexGitHubAPI + "/releases/tags/rust-v" + version)
}

func fetchCodexGitHubRelease(url string) (codexGitHubRelease, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return codexGitHubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "vmsh")
	resp, err := codexHTTPClient.Do(req)
	if err != nil {
		return codexGitHubRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return codexGitHubRelease{}, fmt.Errorf("fetch Codex release metadata: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var release codexGitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return codexGitHubRelease{}, err
	}
	return release, nil
}

func installCodexPackageRelease(release codexGitHubRelease, releasesDir, version, target string) error {
	packageAssetName := "codex-package-" + target + ".tar.gz"
	packageAsset, ok := findCodexReleaseAsset(release, packageAssetName)
	if !ok {
		return fmt.Errorf("release %s does not include %s", version, packageAssetName)
	}
	checksumAsset, ok := findCodexReleaseAsset(release, "codex-package_SHA256SUMS")
	if !ok {
		return fmt.Errorf("release %s does not include codex-package_SHA256SUMS", version)
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(releasesDir), ".vmsh-codex-download.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	checksumPath := filepath.Join(tmpDir, checksumAsset.Name)
	if err := downloadCodexAsset(checksumAsset, checksumPath); err != nil {
		return err
	}
	checksumDigest, err := codexAssetSHA256(checksumAsset)
	if err != nil {
		return err
	}
	if err := verifyFileSHA256(checksumPath, checksumDigest); err != nil {
		return err
	}
	packageDigest, err := codexPackageArchiveDigest(checksumPath, packageAsset.Name)
	if err != nil {
		return err
	}
	archivePath := filepath.Join(tmpDir, packageAsset.Name)
	if err := downloadCodexAsset(packageAsset, archivePath); err != nil {
		return err
	}
	if err := verifyFileSHA256(archivePath, packageDigest); err != nil {
		return err
	}
	releaseDir := filepath.Join(releasesDir, version+"-"+target)
	return extractCodexPackageArchive(releaseDir, archivePath)
}

func findCodexReleaseAsset(release codexGitHubRelease, name string) (codexGitHubAsset, bool) {
	for _, asset := range release.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return codexGitHubAsset{}, false
}

func codexAssetSHA256(asset codexGitHubAsset) (string, error) {
	digest := strings.TrimSpace(asset.Digest)
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("release asset %s is missing a SHA-256 digest", asset.Name)
	}
	hexDigest := strings.TrimPrefix(digest, prefix)
	if _, err := hex.DecodeString(hexDigest); err != nil || len(hexDigest) != 64 {
		return "", fmt.Errorf("release asset %s has invalid SHA-256 digest", asset.Name)
	}
	return strings.ToLower(hexDigest), nil
}

func downloadCodexAsset(asset codexGitHubAsset, dst string) error {
	if asset.BrowserDownloadURL == "" {
		return fmt.Errorf("release asset %s is missing a download URL", asset.Name)
	}
	req, err := http.NewRequest(http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "vmsh")
	resp, err := codexHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s: %s", asset.Name, resp.Status)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func verifyFileSHA256(file, expected string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("SHA-256 mismatch for %s", filepath.Base(file))
	}
	return nil
}

func codexPackageArchiveDigest(checksumPath, assetName string) (string, error) {
	f, err := os.Open(checksumPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == assetName {
			digest := strings.ToLower(fields[0])
			if _, err := hex.DecodeString(digest); err == nil && len(digest) == 64 {
				return digest, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksum manifest does not include %s", assetName)
}

func extractCodexPackageArchive(releaseDir, archivePath string) error {
	stage := filepath.Join(filepath.Dir(releaseDir), ".staging."+filepath.Base(releaseDir)+"."+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stage)
		}
	}()
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	gzr, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gzr.Close()
	if err := extractTarSafe(gzr, stage); err != nil {
		return err
	}
	for _, file := range []string{
		filepath.Join(stage, "bin", "codex"),
		filepath.Join(stage, "codex-path", "rg"),
		filepath.Join(stage, "codex-resources", "bwrap"),
	} {
		if _, err := os.Stat(file); err == nil {
			_ = os.Chmod(file, 0o755)
		}
	}
	link := filepath.Join(stage, "codex")
	_ = os.Remove(link)
	if err := os.Symlink(filepath.Join("bin", "codex"), link); err != nil {
		info, statErr := os.Stat(filepath.Join(stage, "bin", "codex"))
		if statErr != nil {
			return statErr
		}
		if err := copyHostFile(filepath.Join(stage, "bin", "codex"), link, info.Mode()); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(releaseDir); err != nil {
		return err
	}
	if err := os.Rename(stage, releaseDir); err != nil {
		return err
	}
	cleanupStage = false
	return nil
}

func extractTarSafe(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "/"))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe tar path %q", header.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		cleanDst := filepath.Clean(dst)
		cleanTarget := filepath.Clean(target)
		if cleanTarget != cleanDst && !strings.HasPrefix(cleanTarget, cleanDst+string(filepath.Separator)) {
			return fmt.Errorf("unsafe tar path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode).Perm())
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}

func withCodexInstallLock(standaloneRoot string, fn func() error) error {
	lockDir := filepath.Join(standaloneRoot, "vmsh-install.lock.d")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err := os.Mkdir(lockDir, 0o700)
		if err == nil {
			_ = os.WriteFile(filepath.Join(lockDir, "started_at"), []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600)
			defer os.RemoveAll(lockDir)
			return fn()
		}
		if !os.IsExist(err) {
			return err
		}
		if codexLockStale(lockDir, 10*time.Minute) {
			_ = os.RemoveAll(lockDir)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Codex install lock")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func codexLockStale(lockDir string, staleAfter time.Duration) bool {
	info, err := os.Stat(lockDir)
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) > staleAfter
}

func updateVMShCodexLink(standaloneRoot, target, releaseName string) error {
	linkDir := filepath.Join(standaloneRoot, "vmsh", target)
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(linkDir, "current")
	tmp := filepath.Join(linkDir, ".current."+strconv.FormatInt(time.Now().UnixNano(), 10))
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("..", "..", "releases", releaseName), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func compareCodexVersions(a, b string) int {
	pa, oka := parseCodexVersion(a)
	pb, okb := parseCodexVersion(b)
	if !oka || !okb {
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if pa.nums[i] != pb.nums[i] {
			if pa.nums[i] > pb.nums[i] {
				return 1
			}
			return -1
		}
	}
	if pa.preKind != pb.preKind {
		return comparePrereleaseKind(pa.preKind, pb.preKind)
	}
	if pa.preNum != pb.preNum {
		if pa.preNum > pb.preNum {
			return 1
		}
		return -1
	}
	return 0
}

type parsedCodexVersion struct {
	nums    [3]int
	preKind string
	preNum  int
}

func parseCodexVersion(value string) (parsedCodexVersion, bool) {
	if !codexVersionPattern.MatchString(value) {
		return parsedCodexVersion{}, false
	}
	var out parsedCodexVersion
	base, pre, _ := strings.Cut(value, "-")
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return parsedCodexVersion{}, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return parsedCodexVersion{}, false
		}
		out.nums[i] = n
	}
	if pre != "" {
		preParts := strings.Split(pre, ".")
		out.preKind = preParts[0]
		if len(preParts) > 1 {
			out.preNum, _ = strconv.Atoi(preParts[1])
		}
	}
	return out, true
}

func comparePrereleaseKind(a, b string) int {
	rank := func(kind string) int {
		switch kind {
		case "":
			return 3
		case "beta":
			return 2
		case "alpha":
			return 1
		default:
			return 0
		}
	}
	ra, rb := rank(a), rank(b)
	if ra == rb {
		return 0
	}
	if ra > rb {
		return 1
	}
	return -1
}

type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) Write(data []byte) (int, error) {
	b.data = append(b.data, data...)
	return len(data), nil
}

func (b *bytesBuffer) String() string {
	return string(b.data)
}
