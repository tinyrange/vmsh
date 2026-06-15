package shell

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/tinyrange/vmsh/internal/backend"
	"github.com/tinyrange/vmsh/internal/editor"
	"github.com/tinyrange/vmsh/internal/terminal"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
	"j5.nz/cc/client"
)

const guestHostMount = "/host"
const isolatedVMSuffix = "-isolated"
const defaultGuestUser = "1000:1000"
const defaultVMSHBootTimeoutSeconds = 60
const defaultGuestShellReadyTimeout = 30 * time.Second
const maxEmbeddedHostInitPreludeBytes = 64 * 1024
const ubuntuCloudRootFSBaseURL = "https://cloud-images.ubuntu.com/releases/noble/release"
const (
	colorReset   = "\x1b[0m"
	colorGreen   = "\x1b[32m"
	colorCyan    = "\x1b[36m"
	colorBlue    = "\x1b[34m"
	colorMagenta = "\x1b[35m"
	colorYellow  = "\x1b[33m"
)

type shellMode string

const (
	modeHost shellMode = "host"
	modeVM   shellMode = "vm"
	modeSSH  shellMode = "ssh"
)

type shellState struct {
	api              backend.API
	context          commandContext
	hostCWD          string
	rootCache        string
	vmshPath         string
	ccvmPath         string
	imageCache       map[string]bool
	vmRunning        map[string]bool
	hostInit         hostShellInit
	hostShell        *persistentHostShell
	guestShell       *persistentGuestShell
	sshShells        map[string]*persistentSSHShell
	sshMu            sync.Mutex
	sshClients       map[string]*persistentSSHClient
	lastCode         int
	promptOut        io.Writer
	history          string
	env              map[string]string
	aliases          map[string]string
	confirmPull      func(string, io.Writer) (bool, error)
	confirmVMRestart func(string, io.Writer) (bool, error)
	confirmSSHHost   func(resolvedSSHConfig, string, net.Addr, ssh.PublicKey) (bool, error)
	confirmExit      func([]exitResource, io.Writer) (bool, error)
	sshPassword      func(resolvedSSHConfig) (string, error)
	sshKeyboardAuth  func(resolvedSSHConfig, string, string, []string, []bool) ([]string, error)
	sshBanner        func(resolvedSSHConfig, string) error
	jobs             []shellJob
	nextJobID        int
	jobsMu           sync.Mutex
	contextCWD       map[string]string
	contextStack     []commandContext
	statusSeq        atomic.Uint64
	completion       *vmshCompleter
	tmuxExec         func([]string) error
	interruptSignals <-chan os.Signal
}

type imagePullContextAPI interface {
	PullImageStreamContext(context.Context, string, client.PullImageRequest, func(client.ProgressEvent) error) error
}

type instanceStartContextAPI interface {
	StartInstanceStreamWithIDContext(context.Context, string, client.StartInstanceRequest, func(client.BootEvent) error) (client.InstanceState, error)
}

type execStreamContextAPI interface {
	ExecStreamInContext(context.Context, string, client.ExecRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
}

type shellJob struct {
	ID      int
	Context commandContext
	Command string
	Done    bool
	Code    int
	Err     string
}

type hostShellInit struct {
	once     sync.Once
	prelude  string
	fallback bool
}

type persistentHostShell struct {
	mu          sync.Mutex
	outputMu    sync.Mutex
	cmd         *exec.Cmd
	tty         *os.File
	stdin       io.WriteCloser
	stdout      *bufio.Reader
	control     *bufio.Reader
	controlFile *os.File
	output      io.Writer
	seq         uint64
	lastCWD     string
	pending     string
	done        chan error
}

type persistentGuestShell struct {
	mu      sync.Mutex
	key     string
	inputs  chan client.ExecInput
	events  chan client.ExecEvent
	done    chan error
	lastCWD string
}

type vmshCompleter struct {
	shell *shellState
}

type completionKind = editor.CompletionKind

const (
	completionNone    = editor.CompletionNone
	completionAt      = editor.CompletionAt
	completionOption  = editor.CompletionOption
	completionCommand = editor.CompletionCommand
	completionPath    = editor.CompletionPath
)

func newVMSHCompleter(shell *shellState) *vmshCompleter {
	return &vmshCompleter{shell: shell}
}

func (c *vmshCompleter) Do(line []rune, pos int) ([][]rune, int) {
	candidates, replacementLen, _ := c.CompleteWithKind(line, pos)
	return stringCompletions(candidates), replacementLen
}

func (c *vmshCompleter) Complete(line []rune, pos int) ([]string, int) {
	candidates, replacementLen, _ := c.CompleteWithKind(line, pos)
	return candidates, replacementLen
}

func (c *vmshCompleter) CompleteWithKind(line []rune, pos int) ([]string, int, completionKind) {
	prefix := currentCompletionSegment(string(line[:pos]))
	typedTokenStart := lastCompletionTokenStart(prefix)
	typedToken := prefix[typedTokenStart:]
	effectivePrefix := c.effectiveCompletionPrefix(prefix)
	completionCtx := c.completionContext(effectivePrefix)
	prefix = effectivePrefix
	tokenStart := lastCompletionTokenStart(prefix)
	token := prefix[tokenStart:]
	isFirstToken := strings.TrimSpace(prefix[:tokenStart]) == ""
	var candidates []string
	if strings.HasPrefix(prefix, "@") && isFirstToken {
		for _, word := range c.atTargetWords() {
			if strings.HasPrefix(word, token) {
				candidates = append(candidates, word[len(token):])
			}
		}
		return candidates, len([]rune(token)), completionAt
	}
	if strings.HasPrefix(prefix, "@") && strings.HasPrefix(token, "--") {
		candidates = suffixCompletions(vmshOptionWords(prefix[:tokenStart]), token)
		return candidates, len([]rune(token)), completionOption
	}
	if c.shouldCompleteSSHHost(prefix, tokenStart) {
		candidates = suffixCompletions(sshHostAliases(), token)
		return candidates, len([]rune(typedToken)), completionAt
	}
	if c.shouldCompleteRMIImage(prefix, tokenStart) {
		candidates = suffixCompletions(c.cachedImageNames(), token)
		return candidates, len([]rune(typedToken)), completionAt
	}
	if c.shouldCompleteStopTarget(prefix, tokenStart) {
		candidates = suffixCompletions(c.stopTargetNames(), token)
		return candidates, len([]rune(typedToken)), completionAt
	}
	if c.shouldCompleteCommand(prefix, tokenStart, isFirstToken, token) {
		candidates = c.commandCandidates(token, completionCtx)
		return candidates, len([]rune(typedToken)), completionCommand
	}
	if !isFirstToken || token == "" || strings.Contains(token, "/") || token == "." || token == ".." || strings.HasPrefix(token, "~") {
		candidates = c.pathCandidates(token, completionCtx)
		return candidates, pathCompletionReplaceLen(typedToken), completionPath
	}
	return nil, 0, completionNone
}

func currentCompletionSegment(line string) string {
	start := 0
	escaped := false
	var quote rune
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ';' || r == '|' || r == '&':
			start = i + len(string(r))
			if (r == '|' || r == '&') && start < len(line) && line[start] == byte(r) {
				start++
			}
		}
	}
	return strings.TrimLeft(line[start:], " \t\n")
}

func (c *vmshCompleter) effectiveCompletionPrefix(prefix string) string {
	if c.shell == nil || isAliasCommandLine(strings.TrimSpace(prefix)) {
		return prefix
	}
	expanded, err := c.shell.expandAliasCompletionPrefix(prefix)
	if err != nil {
		return prefix
	}
	return expanded
}

func (c *vmshCompleter) completionContext(prefix string) commandContext {
	var ctx commandContext
	if c.shell != nil {
		ctx = c.shell.context
	}
	if !strings.HasPrefix(strings.TrimSpace(prefix), "@") {
		return ctx
	}
	at, err := parseAtLine(prefix)
	if err != nil {
		return ctx
	}
	switch at.Target {
	case "":
		ctx = ctx.withOptions(at.Options)
		if at.Options.Sudo {
			ctx.Mode = modeVM
			ctx.User = "root"
		}
	case "sudo":
		ctx = ctx.withOptions(at.Options)
		if ctx.Mode != modeHost {
			ctx = vmCommandContext(ctx, commandOptions{}, ctx.Image)
			ctx.User = "root"
		}
	case "host":
		ctx = hostCommandContext(ctx, at.Options)
	case "ssh":
		host, _, err := parseSSHAtCommand(at.Command)
		if err == nil {
			ctx = sshCommandContext(ctx, at.Options, host)
		}
	case "help", "ps", "jobs", "alias", "status", "where", "start", "stop", "restart", "forward", "tmux", "agent":
	default:
		if sshCtx, ok := c.shellSSHSessionContext(at.Target); ok {
			ctx = sshCtx
		} else {
			ctx = vmCommandContext(ctx, at.Options, at.Target)
		}
	}
	return ctx
}

func (c *vmshCompleter) shellSSHSessionContext(name string) (commandContext, bool) {
	if c.shell == nil {
		return commandContext{}, false
	}
	return c.shell.sshSessionContext(name)
}

func pathCompletionReplaceLen(token string) int {
	if token == "" {
		return 0
	}
	return len([]rune(filepath.Base(token)))
}

func (c *vmshCompleter) atTargetWords() []string {
	words := []string{"@agent", "@alias", "@copy", "@help", "@host", "@jobs", "@ps", "@restart", "@status", "@start", "@stop", "@forward", "@rmi", "@ssh", "@sudo", "@tmux"}
	if c.shell != nil {
		for _, name := range c.shell.sshSessionNames() {
			words = append(words, "@"+name)
		}
	}
	for _, image := range c.cachedImageNames() {
		words = append(words, "@"+image)
	}
	sort.Strings(words)
	return uniqueStrings(words)
}

func (c *vmshCompleter) cachedImageNames() []string {
	if c.shell == nil || c.shell.rootCache == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(c.shell.rootCache, "images"))
	if err != nil {
		return nil
	}
	var images []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "blobs" || name == "layers" {
			continue
		}
		images = append(images, name)
	}
	sort.Strings(images)
	return images
}

func vmshOptionWords(prefix string) []string {
	words := []string{
		"--vm",
		"--cwd",
		"--user",
		"--sudo",
		"--memory",
		"--memory-mb",
		"--cpus",
		"--arch",
		"--network",
		"--no-network",
		"--nested",
		"--no-nested",
		"--isolated",
		"--shared",
	}
	completed := completedShellWords(prefix)
	if len(completed) > 0 && completed[0] == "@agent" {
		words = append(words, "--proxy")
	}
	return words
}

func suffixCompletions(words []string, token string) []string {
	var out []string
	for _, word := range words {
		if strings.HasPrefix(word, token) {
			out = append(out, word[len(token):])
		}
	}
	sort.Strings(out)
	return uniqueStrings(out)
}

func (c *vmshCompleter) shouldCompleteCommand(prefix string, tokenStart int, isFirstToken bool, token string) bool {
	if strings.Contains(token, "/") || strings.HasPrefix(token, "~") || token == "." || token == ".." {
		return false
	}
	if token == "" {
		return false
	}
	if isFirstToken {
		return !strings.HasPrefix(prefix, "@")
	}
	if !strings.HasPrefix(prefix, "@") {
		return false
	}
	words := completedShellWords(prefix[:tokenStart])
	seenTarget := false
	for i, word := range words {
		if i == 0 {
			if !strings.HasPrefix(word, "@") {
				return false
			}
			seenTarget = strings.TrimPrefix(word, "@") != ""
			continue
		}
		if strings.HasPrefix(word, "--") {
			continue
		}
		if !seenTarget && word != "" {
			seenTarget = true
			continue
		}
		return false
	}
	return true
}

func (c *vmshCompleter) shouldCompleteRMIImage(prefix string, tokenStart int) bool {
	if !strings.HasPrefix(prefix, "@") {
		return false
	}
	words := completedShellWords(prefix[:tokenStart])
	if len(words) != 1 {
		return false
	}
	return words[0] == "@rmi"
}

func (c *vmshCompleter) shouldCompleteSSHHost(prefix string, tokenStart int) bool {
	if !strings.HasPrefix(prefix, "@") {
		return false
	}
	words := completedShellWords(prefix[:tokenStart])
	return len(words) == 1 && words[0] == "@ssh"
}

func (c *vmshCompleter) shouldCompleteStopTarget(prefix string, tokenStart int) bool {
	if !strings.HasPrefix(prefix, "@") {
		return false
	}
	words := completedShellWords(prefix[:tokenStart])
	return len(words) == 1 && words[0] == "@stop"
}

func (c *vmshCompleter) stopTargetNames() []string {
	if c.shell == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, name := range c.shell.sshSessionNames() {
		add(name)
	}
	if c.shell.api != nil {
		if states, err := c.shell.api.InstanceStatuses(); err == nil {
			for _, state := range states {
				add(state.ID)
			}
		}
	}
	sort.Strings(names)
	return names
}

func completedShellWords(line string) []string {
	tokens, err := lexShellTokens(line)
	if err != nil {
		return strings.Fields(line)
	}
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		words = append(words, token.Value)
	}
	return words
}

func (c *vmshCompleter) commandCandidates(token string, ctx commandContext) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name == "" || seen[name] || !strings.HasPrefix(name, token) {
			return
		}
		seen[name] = true
		out = append(out, shellEscapeCompletion(name[len(token):]))
	}
	for _, name := range []string{"cd", "exit", "export", "pwd", "echo", "env", "ls", "cat", "grep", "find", "git", "make", "go", "python", "python3", "sh"} {
		add(name)
	}
	if ctx.Mode == modeVM {
		for _, name := range c.guestCommandNames(ctx, token) {
			add(name)
		}
		sortCompletionItems(out)
		return out
	}
	if ctx.Mode == modeSSH {
		for _, name := range c.sshCommandNames(ctx, token) {
			add(name)
		}
		sortCompletionItems(out)
		return out
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.Mode()&0o111 == 0 {
				continue
			}
			add(entry.Name())
		}
	}
	sortCompletionItems(out)
	return out
}

func (c *vmshCompleter) guestCommandNames(ctx commandContext, token string) []string {
	if c.shell == nil || c.shell.api == nil || ctx.VMID == "" || ctx.Image == "" {
		return nil
	}
	id := backendVMID(ctx)
	status, err := c.shell.api.InstanceStatusOf(id)
	if err != nil || status.Status != "running" {
		return nil
	}
	req := client.RunRequest{
		Image:   localImageName(ctx.Image, ctx.Arch),
		Command: []string{"sh", "-lc", guestCommandCompletionScript(token)},
		WorkDir: c.shell.currentGuestCWD(ctx),
		User:    guestRunUser(ctx),
	}
	var stdout strings.Builder
	runCtx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	err = c.shell.api.RunStreamInContext(runCtx, id, req, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			stdout.WriteString(execEventText(event))
		case "error":
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return fmt.Errorf("guest command completion failed")
		}
		return nil
	})
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return uniqueStrings(names)
}

func (c *vmshCompleter) sshCommandNames(ctx commandContext, token string) []string {
	if c.shell == nil || !c.shell.hasSSHClient(ctx) {
		return nil
	}
	var stdout strings.Builder
	if err := c.shell.runSSHCommand(ctx, guestCommandCompletionScript(token), nil, &stdout, io.Discard, false, false); err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return uniqueStrings(names)
}

func guestCommandCompletionScript(prefix string) string {
	return strings.Join([]string{
		"prefix=" + shellQuote(prefix),
		`old_ifs=$IFS`,
		`IFS=:`,
		`for dir in $PATH; do`,
		`  [ -d "$dir" ] || continue`,
		`  for path in "$dir"/"$prefix"*; do`,
		`    [ -f "$path" ] && [ -x "$path" ] || continue`,
		`    name=${path##*/}`,
		`    case "$name" in "$prefix"*) printf '%s\n' "$name" ;; esac`,
		`  done`,
		`done`,
		`IFS=$old_ifs`,
	}, "\n")
}

func lastCompletionTokenStart(line string) int {
	last := 0
	escaped := false
	var quote rune
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' {
			last = i + len(string(r))
		}
	}
	return last
}

func (c *vmshCompleter) pathCandidates(token string, ctx commandContext) []string {
	if ctx.Mode == modeVM {
		if out, ok := c.guestPathCandidates(token, ctx); ok {
			return out
		}
	}
	if ctx.Mode == modeSSH {
		if out, ok := c.sshPathCandidates(token, ctx); ok {
			return out
		}
		return nil
	}
	return c.hostPathCandidates(token, ctx)
}

func (c *vmshCompleter) sshPathCandidates(token string, ctx commandContext) ([]string, bool) {
	if c.shell == nil || !c.shell.hasSSHClient(ctx) {
		return nil, false
	}
	dirPart, base := path.Split(filepath.ToSlash(token))
	current := c.shell.currentSSHCWD(ctx)
	var remoteDir string
	switch {
	case dirPart == "":
		remoteDir = current
	case dirPart == "~" || strings.HasPrefix(dirPart, "~/"):
		remoteDir = path.Join("~", strings.TrimPrefix(strings.TrimSuffix(dirPart, "/"), "~"))
	case strings.HasPrefix(dirPart, "/"):
		remoteDir = path.Clean(dirPart)
	default:
		remoteDir = path.Clean(path.Join(current, dirPart))
	}
	var stdout strings.Builder
	if err := c.shell.runSSHCommand(ctx, guestCompletionScript(remoteDir, base), nil, &stdout, io.Discard, false, false); err != nil {
		return nil, true
	}
	var out []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, shellEscapeCompletion(line))
	}
	sortCompletionItems(out)
	return out, true
}

func (c *vmshCompleter) hostPathCandidates(token string, ctx commandContext) []string {
	dirPart, base := filepath.Split(token)
	hostDir := dirPart
	if hostDir == "" {
		if c.shell != nil {
			hostDir = c.shell.hostCWD
		}
		if c.shell != nil && ctx.Mode == modeVM {
			current := ctx.CWD
			if current == "" {
				_, current, _ = guestHostPaths(c.shell.hostCWD)
			}
			if hostPath, ok := c.guestHostCompletionDir(current); ok {
				hostDir = hostPath
			}
		}
	} else {
		hostDir = c.hostCompletionDir(hostDir, ctx)
	}
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if !strings.HasPrefix(name, base) {
			continue
		}
		suffix := name[len(base):]
		if entry.IsDir() {
			suffix += "/"
		}
		out = append(out, shellEscapeCompletion(suffix))
	}
	sortCompletionItems(out)
	return out
}

func (c *vmshCompleter) guestPathCandidates(token string, ctx commandContext) ([]string, bool) {
	if c.shell == nil || c.shell.api == nil || ctx.VMID == "" || ctx.Image == "" {
		return nil, false
	}
	dirPart, base := path.Split(filepath.ToSlash(token))
	current := ctx.CWD
	if current == "" {
		_, current, _ = guestHostPaths(c.shell.hostCWD)
	}
	var guestDir string
	switch {
	case dirPart == "":
		guestDir = current
	case dirPart == "~" || strings.HasPrefix(dirPart, "~/"):
		guestDir = path.Join(guestHomeDir(ctx), strings.TrimPrefix(strings.TrimSuffix(dirPart, "/"), "~"))
	case strings.HasPrefix(dirPart, "/"):
		guestDir = path.Clean(dirPart)
	default:
		guestDir = path.Clean(path.Join(current, dirPart))
	}
	if guestDir == guestHostMount || strings.HasPrefix(guestDir, guestHostMount+"/") {
		return nil, false
	}
	out, err := c.guestPathCandidatesInDir(ctx, guestDir, base)
	if err != nil {
		return nil, true
	}
	return out, true
}

func (c *vmshCompleter) guestPathCandidatesInDir(ctx commandContext, guestDir, base string) ([]string, error) {
	id := backendVMID(ctx)
	status, err := c.shell.api.InstanceStatusOf(id)
	if err != nil || status.Status != "running" {
		return nil, err
	}
	script := guestCompletionScript(guestDir, base)
	req := client.RunRequest{
		Image:   localImageName(ctx.Image, ctx.Arch),
		Command: []string{"sh", "-lc", script},
		WorkDir: guestDir,
		User:    guestRunUser(ctx),
	}
	var stdout strings.Builder
	runCtx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	err = c.shell.api.RunStreamInContext(runCtx, id, req, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			stdout.WriteString(execEventText(event))
		case "error":
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return fmt.Errorf("guest completion failed")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, shellEscapeCompletion(line))
	}
	sortCompletionItems(out)
	return out, nil
}

func guestCompletionScript(guestDir, base string) string {
	return strings.Join([]string{
		"dir=" + shellQuote(guestDir),
		"base=" + shellQuote(base),
		`case "$base" in .*) include_hidden=1 ;; *) include_hidden=0 ;; esac`,
		`for p in "$dir"/"$base"*; do`,
		`  [ -e "$p" ] || [ -L "$p" ] || continue`,
		`  name=${p##*/}`,
		`  [ "$include_hidden" = 1 ] || case "$name" in .*) continue ;; esac`,
		`  suffix=${name#"$base"}`,
		`  if [ -d "$p" ]; then printf '%s/\n' "$suffix"; else printf '%s\n' "$suffix"; fi`,
		`done`,
	}, "\n")
}

func (c *vmshCompleter) hostCompletionDir(dirPart string, ctx commandContext) string {
	hostDir := os.ExpandEnv(dirPart)
	if c.shell != nil && ctx.Mode == modeVM {
		if hostPath, ok := c.guestHostCompletionDir(hostDir); ok {
			return hostPath
		}
	}
	if strings.HasPrefix(hostDir, "~/") || hostDir == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			if hostDir == "~" {
				hostDir = home
			} else {
				hostDir = filepath.Join(home, hostDir[2:])
			}
		}
	}
	if !filepath.IsAbs(hostDir) {
		cwd := "."
		if c.shell != nil && c.shell.hostCWD != "" {
			cwd = c.shell.hostCWD
		}
		hostDir = filepath.Join(cwd, hostDir)
	}
	return hostDir
}

func (c *vmshCompleter) guestHostCompletionDir(dirPart string) (string, bool) {
	if dirPart == guestHostMount || strings.HasPrefix(dirPart, guestHostMount+"/") {
		if c.shell == nil {
			return "", false
		}
		return guestHostPathToHost(c.shell.hostCWD, dirPart)
	}
	if !strings.HasPrefix(dirPart, "/") {
		current := c.shell.context.CWD
		if current == "" {
			_, current, _ = guestHostPaths(c.shell.hostCWD)
		}
		guestDir := path.Clean(path.Join(current, filepath.ToSlash(dirPart)))
		if guestDir == guestHostMount || strings.HasPrefix(guestDir, guestHostMount+"/") {
			return guestHostPathToHost(c.shell.hostCWD, guestDir)
		}
	}
	return "", false
}

func shellEscapeCompletion(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, " ", `\ `)
	return value
}

func stringCompletions(items []string) [][]rune {
	if len(items) == 0 {
		return nil
	}
	out := make([][]rune, 0, len(items))
	for _, item := range items {
		out = append(out, []rune(item))
	}
	return out
}

func sortCompletionItems(items []string) {
	sort.Slice(items, func(i, j int) bool {
		iDir := strings.HasSuffix(items[i], "/")
		jDir := strings.HasSuffix(items[j], "/")
		if iDir != jDir {
			return iDir
		}
		if len(items[i]) != len(items[j]) {
			return len(items[i]) < len(items[j])
		}
		return items[i] < items[j]
	})
}

func uniqueStrings(items []string) []string {
	if len(items) < 2 {
		return items
	}
	out := items[:0]
	var prev string
	for i, item := range items {
		if i > 0 && item == prev {
			continue
		}
		out = append(out, item)
		prev = item
	}
	return out
}

type commandContext struct {
	Mode       shellMode `json:"mode"`
	Image      string    `json:"image,omitempty"`
	SSHHost    string    `json:"ssh_host,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	VMID       string    `json:"vm,omitempty"`
	CWD        string    `json:"cwd,omitempty"`
	User       string    `json:"user,omitempty"`
	InitSystem string    `json:"init,omitempty"`
	Kernel     string    `json:"kernel,omitempty"`
	MemoryMB   uint64    `json:"memory_mb,omitempty"`
	CPUs       int       `json:"cpus,omitempty"`
	Network    bool      `json:"network,omitempty"`
	NestedVirt bool      `json:"nested_virtualization,omitempty"`
	Isolated   bool      `json:"isolated,omitempty"`
}

type atLine struct {
	Target  string
	Options commandOptions
	Command string
}

type commandOptions struct {
	VMID         string
	CWD          string
	User         string
	Arch         string
	Sudo         bool
	AgentProxy   bool
	InitSystem   *string
	Kernel       *string
	MemoryMB     uint64
	CPUs         int
	Network      *bool
	NestedVirt   *bool
	Isolated     *bool
	OptionFields []string
}

type shellToken struct {
	Value string
	Start int
	End   int
}

func Run(args []string) error {
	fs := flag.NewFlagSet("vmsh", flag.ExitOnError)
	ccvmPath := fs.String("ccvm", "", "Path to ccvm binary")
	cacheDir := fs.String("cache-dir", "", "Cache directory")
	image := fs.String("image", "", "Initial image for VM commands")
	vmID := fs.String("vm", "default", "Initial VM id")
	startVM := fs.Bool("start", false, "Start the selected blank VM before entering the shell")
	script := fs.String("script", "", "Internal test hook: read vmsh commands from this file")
	recordPath := fs.String("record", "", "Record terminal output to an asciinema v2 .cast file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: vmsh [flags]")
	}

	rootCache, err := resolveCacheDir(*cacheDir)
	if err != nil {
		return err
	}
	statePath := filepath.Join(rootCache, "ccvm.json")
	ccvmLaunch, err := backend.ResolveCCVMPath(*ccvmPath, bundledCCVMAvailable())
	if err != nil {
		return err
	}
	vmshPath, err := os.Executable()
	if err != nil {
		return err
	}
	childCCVMPath := ""
	if strings.TrimSpace(*ccvmPath) != "" {
		childCCVMPath = ccvmLaunch.Path
		if abs, err := filepath.Abs(childCCVMPath); err == nil {
			childCCVMPath = abs
		}
	}
	stdout := io.Writer(os.Stdout)
	stderr := io.Writer(os.Stderr)
	var recorder *asciinemaRecorder
	if strings.TrimSpace(*recordPath) != "" {
		_, cols, rows := terminalRequestSize(os.Stdout)
		if cols <= 0 {
			cols = 80
		}
		if rows <= 0 {
			rows = 24
		}
		rec, err := newAsciinemaRecorder(strings.TrimSpace(*recordPath), cols, rows)
		if err != nil {
			return err
		}
		defer rec.Close()
		recorder = rec
		stdout = newRecordingTerminalWriter(os.Stdout, recorder)
		stderr = newRecordingTerminalWriter(os.Stderr, recorder)
	}
	api, err := backend.ConnectCCVMWithOptions(ccvmLaunch, rootCache, statePath, backend.ConnectOptions{
		OnReuse: func(state backend.DaemonState) {
			fmt.Fprintf(stderr, "vmsh: reusing ccvm daemon at %s\n", state.Addr)
		},
	})
	if err != nil {
		return err
	}
	caps, _ := api.Capabilities()
	stopLease, err := backend.StartDaemonLease(api)
	if err != nil {
		return err
	}
	defer stopLease()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	sh := &shellState{
		api:        api,
		context:    defaultContext(strings.TrimSpace(*vmID), strings.TrimSpace(*image), caps.SupportsNestedVirt),
		hostCWD:    cwd,
		rootCache:  rootCache,
		vmshPath:   vmshPath,
		ccvmPath:   childCCVMPath,
		imageCache: map[string]bool{},
		vmRunning:  map[string]bool{},
		contextCWD: map[string]string{},
		promptOut:  stdout,
		history:    filepath.Join(rootCache, "vmsh_history"),
		env:        map[string]string{},
		aliases:    map[string]string{},
		confirmPull: func(source string, stderr io.Writer) (bool, error) {
			return promptPullConfirmation(os.Stdin, stderr, source)
		},
		confirmVMRestart: func(id string, stderr io.Writer) (bool, error) {
			return promptVMRestartConfirmation(os.Stdin, stderr, id)
		},
		confirmSSHHost: func(cfg resolvedSSHConfig, hostname string, remote net.Addr, key ssh.PublicKey) (bool, error) {
			return promptSSHHostKeyConfirmation(os.Stdin, stderr, cfg, hostname, remote, key)
		},
		confirmExit: func(resources []exitResource, stderr io.Writer) (bool, error) {
			return promptExitConfirmation(os.Stdin, stderr, resources)
		},
		sshPassword: func(cfg resolvedSSHConfig) (string, error) {
			return promptSSHPassword(os.Stdin, stderr, cfg)
		},
		sshKeyboardAuth: func(cfg resolvedSSHConfig, name, instruction string, questions []string, echos []bool) ([]string, error) {
			return promptSSHKeyboardInteractive(os.Stdin, stderr, cfg, name, instruction, questions, echos)
		},
		sshBanner: func(cfg resolvedSSHConfig, message string) error {
			_, err := fmt.Fprint(stderr, message)
			return err
		},
	}
	sh.completion = newVMSHCompleter(sh)
	defer sh.closeSessions()
	if err := sh.loadVMSHRC(defaultVMSHRCPath()); err != nil {
		return err
	}
	if *startVM {
		if err := sh.startVM(sh.context.VMID, sh.context, stderr); err != nil {
			return err
		}
	}
	if *script != "" {
		f, err := os.Open(*script)
		if err != nil {
			return err
		}
		defer f.Close()
		return sh.runScript(f, stdout, stderr)
	}
	return sh.loop(os.Stdin, stdout, stderr)
}

func defaultContext(vmID, image string, nestedVirt bool) commandContext {
	return commandContext{
		Mode:       modeHost,
		VMID:       firstNonEmpty(vmID, "default"),
		Image:      image,
		InitSystem: defaultInitSystemForImage(image),
		Kernel:     defaultKernelForImage(image),
		Network:    true,
		NestedVirt: nestedVirt,
	}
}

func (s *shellState) loop(in io.Reader, stdout, stderr io.Writer) error {
	if !readerIsTerminal(in) || !writerIsTerminal(stdout) {
		return fmt.Errorf("vmsh requires an interactive terminal")
	}
	if outFile, ok := terminalWriterFile(stdout); ok {
		restoreOutput := terminal.PrepareOutput(outFile)
		defer restoreOutput()
	}
	inCloser, ok := in.(io.ReadCloser)
	if !ok {
		return fmt.Errorf("vmsh stdin does not support interactive editing")
	}
	inFile, ok := inCloser.(*os.File)
	if !ok {
		return fmt.Errorf("vmsh stdin does not support terminal editing")
	}
	s.warmHostShell(stdout, stderr)
	return s.evalLineEditor(inFile, stdout, stderr)
}

func (s *shellState) warmHostShell(stdout, stderr io.Writer) {
	tty, cols, rows := terminalRequestSize(stdout)
	if !tty {
		return
	}
	env := hostCommandEnv(s.env, terminalEnv(cols, rows))
	_, _ = s.hostPersistentShell(env, cols, rows, stderr)
}

func (s *shellState) evalLineEditor(in *os.File, stdout, stderr io.Writer) error {
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	previousInterrupts := s.interruptSignals
	s.interruptSignals = sigCh
	defer func() {
		s.interruptSignals = previousInterrupts
	}()

	lineEditor := editor.NewLineEditor(in, stdout, s.history, s.completion)
	for {
		drainInterruptSignals(s.interruptSignals)
		s.drawPromptStatus(stdout)
		line, err := lineEditor.ReadLine(s.prompt())
		s.statusSeq.Add(1)
		switch {
		case errors.Is(err, editor.ErrLineInterrupted):
			continue
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		var evalErr error
		if strings.Contains(line, "\n") {
			evalErr = s.evalPastedLines(line, stdout, stderr)
		} else {
			evalErr = s.eval(line, stdout, stderr)
		}
		if err := evalErr; err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if code := sessionLastCode(err); code >= 0 {
				s.lastCode = code
				continue
			}
			s.lastCode = 1
			fmt.Fprintln(stderr, "vmsh:", err)
		}
	}
}

func (s *shellState) runScript(in io.Reader, stdout, stderr io.Writer) error {
	return s.evalScriptLines(in, stdout, stderr)
}

func defaultVMSHRCPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".vmshrc")
}

func (s *shellState) loadVMSHRC(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	return s.evalVMSHRCLines(path, f)
}

func (s *shellState) evalVMSHRCLines(source string, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			at, err := parseAtLine(line)
			if err != nil {
				return fmt.Errorf("%s:%d: %w", source, lineNo, err)
			}
			if at.Target != "alias" || len(at.Options.OptionFields) != 0 {
				return fmt.Errorf("%s:%d: .vmshrc only supports aliases", source, lineNo)
			}
			if err := s.evalAlias(at.Command, io.Discard); err != nil {
				return fmt.Errorf("%s:%d: %w", source, lineNo, err)
			}
			continue
		}
		if _, _, ok := parseAliasAssignment(line); !ok {
			return fmt.Errorf("%s:%d: .vmshrc only supports aliases", source, lineNo)
		}
		if err := s.evalAlias(line, io.Discard); err != nil {
			return fmt.Errorf("%s:%d: %w", source, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", source, err)
	}
	return nil
}

func (s *shellState) evalScriptLines(in io.Reader, stdout, stderr io.Writer) error {
	return s.evalScriptLinesWithEcho(in, stdout, stderr, nil)
}

func (s *shellState) evalPastedLines(text string, stdout, stderr io.Writer) error {
	return s.evalScriptLinesWithEcho(strings.NewReader(text), stdout, stderr, func(block string) error {
		s.drawPromptStatus(stdout)
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			return nil
		}
		if _, err := fmt.Fprint(stdout, s.prompt()); err != nil {
			return err
		}
		if _, err := fmt.Fprint(stdout, lines[0]); err != nil {
			return err
		}
		if _, err := fmt.Fprint(stdout, "\r\n"); err != nil {
			return err
		}
		for _, line := range lines[1:] {
			if _, err := fmt.Fprint(stdout, line+"\r\n"); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *shellState) evalScriptLinesWithEcho(in io.Reader, stdout, stderr io.Writer, echo func(string) error) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	var pending []string
	var heredocs []hereDocDelimiter
	pendingHasHeredoc := false
	for {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if len(pending) == 0 && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		pending = append(pending, line)
		if len(heredocs) == 0 {
			heredocs = shellHereDocDelimiters(line)
			if len(heredocs) != 0 {
				pendingHasHeredoc = true
				continue
			}
		} else if heredocs[0].matches(line) {
			heredocs = heredocs[1:]
			if len(heredocs) != 0 {
				continue
			}
		} else {
			continue
		}
		block := strings.Join(pending, "\n")
		if !pendingHasHeredoc && shellLineNeedsContinuation(block) {
			continue
		}
		if echo != nil {
			if err := echo(block); err != nil {
				return err
			}
		}
		if err := s.eval(block, stdout, stderr); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			s.lastCode = 1
			return err
		}
		pending = nil
		pendingHasHeredoc = false
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if pendingHasHeredoc {
		return fmt.Errorf("unterminated here-document %s", heredocs[0].delim)
	}
	if len(pending) != 0 {
		return fmt.Errorf("incomplete shell command")
	}
	return nil
}

func shellLineNeedsContinuation(line string) bool {
	inSingle := false
	inDouble := false
	escaped := false
	for _, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		}
	}
	return escaped || inSingle || inDouble
}

type hereDocDelimiter struct {
	delim     string
	stripTabs bool
}

func (d hereDocDelimiter) matches(line string) bool {
	if d.stripTabs {
		line = strings.TrimLeft(line, "\t")
	}
	return line == d.delim
}

func shellHereDocDelimiters(line string) []hereDocDelimiter {
	var out []hereDocDelimiter
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case escaped:
			escaped = false
			continue
		case c == '\\' && !inSingle:
			escaped = true
			continue
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			continue
		case c == '"' && !inSingle:
			inDouble = !inDouble
			continue
		case inSingle || inDouble:
			continue
		case c == '<' && i+1 < len(line) && line[i+1] == '<':
			if i+2 < len(line) && line[i+2] == '<' {
				continue
			}
			stripTabs := false
			j := i + 2
			if j < len(line) && line[j] == '-' {
				stripTabs = true
				j++
			}
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			delim, next := parseHereDocDelimiterToken(line, j)
			if delim != "" {
				out = append(out, hereDocDelimiter{delim: delim, stripTabs: stripTabs})
			}
			if next > i {
				i = next - 1
			}
		}
	}
	return out
}

func parseHereDocDelimiterToken(line string, start int) (string, int) {
	var b strings.Builder
	quote := byte(0)
	escaped := false
	for i := start; i < len(line); i++ {
		c := line[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if quote == 0 {
			switch c {
			case ' ', '\t', ';', '|', '&', '(', ')', '<', '>':
				return b.String(), i
			case '\\':
				escaped = true
			case '\'', '"':
				quote = c
			default:
				b.WriteByte(c)
			}
			continue
		}
		if c == quote {
			quote = 0
			continue
		}
		if quote == '"' && c == '\\' {
			escaped = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), len(line)
}

func shouldSaveHistory(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && !strings.HasPrefix(trimmed, "#")
}

func (s *shellState) eval(line string, stdout, stderr io.Writer) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	if !isAliasCommandLine(line) {
		expanded, err := s.expandAliasLine(line)
		if err != nil {
			return err
		}
		line = expanded
	}
	if strings.HasPrefix(line, "@") {
		return s.evalAt(line, stdout, stderr)
	}
	if segments, ok, err := splitPipelineLine(line); ok || err != nil {
		if err != nil {
			return err
		}
		if shouldRunVMSHPipeline(segments) {
			return s.runPipeline(s.context, segments, stdout, stderr)
		}
	}
	if exit, force, err := parseExitCommand(line); exit || err != nil {
		if err != nil {
			return err
		}
		if s.exitSubshell() {
			s.lastCode = 0
			return nil
		}
		if !force {
			ok, err := s.confirmExitIfNeeded(stderr)
			if err != nil {
				return err
			}
			if !ok {
				s.lastCode = 1
				return nil
			}
		}
		return io.EOF
	}
	if ok, err := s.evalExport(line); ok || err != nil {
		return err
	}
	if cd, ok, err := parseCD(line); ok || err != nil {
		if err != nil {
			return err
		}
		if s.context.Mode == modeVM && s.guestShell != nil {
			return s.runInContext(s.context, line, stdout, stderr)
		}
		return s.chdirContext(cd)
	}
	if command, ok, err := stripBackground(line); ok || err != nil {
		if err != nil {
			return err
		}
		return s.startBackgroundJob(s.context, command, stdout, stderr)
	}
	return s.runInContext(s.context, line, stdout, stderr)
}

func isAliasCommandLine(line string) bool {
	return line == "@alias" || strings.HasPrefix(line, "@alias ") || strings.HasPrefix(line, "@alias\t")
}

func (s *shellState) expandAliasLine(line string) (string, error) {
	const maxAliasExpansionDepth = 16
	line = strings.TrimSpace(line)
	for depth := 0; depth < maxAliasExpansionDepth; depth++ {
		expanded, changed, err := s.expandAliasLineOnce(line)
		if err != nil || !changed {
			return expanded, err
		}
		line = strings.TrimSpace(expanded)
		if isAliasCommandLine(line) {
			return line, nil
		}
	}
	return "", fmt.Errorf("alias expansion exceeded %d levels", maxAliasExpansionDepth)
}

func (s *shellState) expandAliasLineOnce(line string) (string, bool, error) {
	if len(s.aliases) == 0 {
		return line, false, nil
	}
	segments, err := shellCommandSegments(line)
	if err != nil {
		return line, false, err
	}
	for _, segment := range segments {
		commandStart := segment.start + leadingShellSpace(line[segment.start:segment.end])
		if commandStart >= segment.end {
			continue
		}
		tokens, err := lexShellTokens(line[commandStart:segment.end])
		if err != nil {
			return line, false, err
		}
		if len(tokens) == 0 {
			continue
		}
		first := tokens[0]
		replacement, ok := s.aliases[first.Value]
		if !ok {
			continue
		}
		firstEnd := commandStart + first.End
		rest := strings.TrimLeft(line[firstEnd:segment.end], " \t\n")
		expanded := replacement
		if rest != "" {
			expanded = strings.TrimRight(replacement, " \t") + " " + rest
		}
		return line[:commandStart] + expanded + line[segment.end:], true, nil
	}
	return line, false, nil
}

func (s *shellState) expandAliasCompletionPrefix(prefix string) (string, error) {
	const maxAliasExpansionDepth = 16
	line := prefix
	for depth := 0; depth < maxAliasExpansionDepth; depth++ {
		expanded, changed, err := s.expandAliasCompletionPrefixOnce(line)
		if err != nil || !changed {
			return expanded, err
		}
		line = expanded
		if isAliasCommandLine(strings.TrimSpace(line)) {
			return line, nil
		}
	}
	return "", fmt.Errorf("alias expansion exceeded %d levels", maxAliasExpansionDepth)
}

func (s *shellState) expandAliasCompletionPrefixOnce(line string) (string, bool, error) {
	if len(s.aliases) == 0 {
		return line, false, nil
	}
	tokens, err := lexShellTokens(line)
	if err != nil {
		return line, false, err
	}
	if len(tokens) == 0 {
		return line, false, nil
	}
	first := tokens[0]
	replacement, ok := s.aliases[first.Value]
	if !ok {
		return line, false, nil
	}
	rest := line[first.End:]
	if rest == "" {
		return replacement, true, nil
	}
	return strings.TrimRight(replacement, " \t") + rest, true, nil
}

func (s *shellState) runInContext(ctx commandContext, line string, stdout, stderr io.Writer) error {
	target, err := s.targetFor(ctx)
	if err != nil {
		return err
	}
	return target.Run(line, stdout, stderr)
}

func (s *shellState) runMaybeBackground(ctx commandContext, line string, stdout, stderr io.Writer) error {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "@") {
		return s.evalScopedAt(ctx, line, stdout, stderr)
	}
	if command, ok, err := stripBackground(line); ok || err != nil {
		if err != nil {
			return err
		}
		return s.startBackgroundJob(ctx, command, stdout, stderr)
	}
	if parts, ok, err := splitCommandListLine(line); ok && commandListHasVMShCommand(parts) {
		if err != nil {
			return err
		}
		return s.runCommandList(ctx, parts, stdout, stderr)
	}
	if segments, ok, err := splitPipelineLine(line); ok || err != nil {
		if err != nil {
			return err
		}
		if shouldRunVMSHPipeline(segments) {
			return s.runPipeline(ctx, segments, stdout, stderr)
		}
	}
	return s.runInContext(ctx, line, stdout, stderr)
}

func (s *shellState) evalScopedAt(ctx commandContext, line string, stdout, stderr io.Writer) error {
	previous := s.context
	s.context = ctx
	defer func() {
		if (s.context.Mode == modeVM || s.context.Mode == modeSSH) && s.context.CWD != "" {
			s.rememberContextCWD(s.context)
		}
		s.context = previous
	}()
	return s.evalAt(line, stdout, stderr)
}

type shellCommandSegment struct {
	start int
	end   int
}

func shellCommandSegments(line string) ([]shellCommandSegment, error) {
	segments := []shellCommandSegment{{start: 0, end: len(line)}}
	inSingle := false
	inDouble := false
	escaped := false
	start := 0
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && !inSingle:
			escaped = true
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && !inDouble && isShellCommandSeparator(line, i):
			if start == 0 {
				segments = segments[:0]
			}
			segments = append(segments, shellCommandSegment{start: start, end: i})
			if (line[i] == '&' || line[i] == '|') && i+1 < len(line) && line[i+1] == line[i] {
				i++
			}
			start = i + 1
		}
	}
	if escaped {
		return nil, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	if start != 0 {
		segments = append(segments, shellCommandSegment{start: start, end: len(line)})
	}
	return segments, nil
}

func isShellCommandSeparator(line string, i int) bool {
	switch line[i] {
	case ';':
		return true
	case '&':
		return i+1 < len(line) && line[i+1] == '&'
	case '|':
		return true
	default:
		return false
	}
}

func leadingShellSpace(value string) int {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case ' ', '\t', '\n':
			continue
		default:
			return i
		}
	}
	return len(value)
}

type commandListPart struct {
	op      string
	command string
}

func splitCommandListLine(line string) ([]commandListPart, bool, error) {
	var parts []commandListPart
	start := 0
	op := ""
	found := false
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && !inSingle:
			escaped = true
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && !inDouble && ch == ';':
			part := strings.TrimSpace(line[start:i])
			parts = append(parts, commandListPart{op: op, command: part})
			if part == "" {
				return parts, true, fmt.Errorf("empty command before ;")
			}
			op = ";"
			start = i + 1
			found = true
		case !inSingle && !inDouble && (ch == '&' || ch == '|') && i+1 < len(line) && line[i+1] == ch:
			part := strings.TrimSpace(line[start:i])
			parts = append(parts, commandListPart{op: op, command: part})
			if part == "" {
				return parts, true, fmt.Errorf("empty command before %s", line[i:i+2])
			}
			op = line[i : i+2]
			i++
			start = i + 1
			found = true
		}
	}
	if !found {
		return nil, false, nil
	}
	if escaped {
		return parts, true, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		return parts, true, fmt.Errorf("unterminated quote")
	}
	part := strings.TrimSpace(line[start:])
	parts = append(parts, commandListPart{op: op, command: part})
	if part == "" {
		return parts, true, fmt.Errorf("empty command after %s", op)
	}
	return parts, true, nil
}

func commandListHasVMShCommand(parts []commandListPart) bool {
	for i := 1; i < len(parts); i++ {
		if strings.HasPrefix(strings.TrimSpace(parts[i].command), "@") {
			return true
		}
	}
	return false
}

func (s *shellState) runCommandList(base commandContext, parts []commandListPart, stdout, stderr io.Writer) error {
	for i, part := range parts {
		if i > 0 {
			switch part.op {
			case "&&":
				if s.lastCode != 0 {
					continue
				}
			case "||":
				if s.lastCode == 0 {
					continue
				}
			}
		}
		err := s.runCommandListPart(base, part.command, stdout, stderr)
		if err != nil && sessionLastCode(err) < 0 {
			return err
		}
	}
	return nil
}

func (s *shellState) runCommandListPart(base commandContext, command string, stdout, stderr io.Writer) error {
	command = strings.TrimSpace(command)
	if strings.HasPrefix(command, "@") {
		return s.evalAt(command, stdout, stderr)
	}
	return s.runMaybeBackground(base, command, stdout, stderr)
}

type pipelineStage struct {
	command preparedTargetCommand
}

func splitPipelineLine(line string) ([]string, bool, error) {
	var segments []string
	start := 0
	inSingle := false
	inDouble := false
	escaped := false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case r == '|' && !inSingle && !inDouble:
			prevPipe := i > 0 && line[i-1] == '|'
			nextPipe := i+1 < len(line) && line[i+1] == '|'
			if prevPipe || nextPipe {
				continue
			}
			segment := strings.TrimSpace(line[start:i])
			if segment == "" {
				return nil, true, fmt.Errorf("pipeline segment is empty")
			}
			segments = append(segments, segment)
			start = i + 1
		}
	}
	if escaped {
		if len(segments) == 0 {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		if len(segments) == 0 {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("unterminated quote")
	}
	if len(segments) == 0 {
		return nil, false, nil
	}
	last := strings.TrimSpace(line[start:])
	if last == "" {
		return nil, true, fmt.Errorf("pipeline segment is empty")
	}
	segments = append(segments, last)
	return segments, true, nil
}

func shouldRunVMSHPipeline(segments []string) bool {
	for _, segment := range segments {
		if strings.HasPrefix(strings.TrimSpace(segment), "@") {
			return true
		}
	}
	return false
}

func (s *shellState) runPipeline(base commandContext, segments []string, stdout, stderr io.Writer) error {
	if len(segments) < 2 {
		return fmt.Errorf("pipeline requires at least two commands")
	}
	stages := make([]pipelineStage, 0, len(segments))
	for _, segment := range segments {
		stage, err := s.preparePipelineStage(base, segment, stderr)
		if err != nil {
			return err
		}
		stages = append(stages, stage)
	}

	readers := make([]*io.PipeReader, len(stages)-1)
	writers := make([]*io.PipeWriter, len(stages)-1)
	for i := range readers {
		readers[i], writers[i] = io.Pipe()
	}

	errs := make([]error, len(stages))
	var wg sync.WaitGroup
	for i := range stages {
		i := i
		var stdin io.Reader
		if i > 0 {
			stdin = readers[i-1]
		}
		stageStdout := stdout
		if i < len(stages)-1 {
			stageStdout = writers[i]
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.runPipelineStage(stages[i], stdin, stageStdout, stderr)
			errs[i] = err
			if reader, ok := stdin.(*io.PipeReader); ok {
				_ = reader.Close()
			}
			if writer, ok := stageStdout.(*io.PipeWriter); ok {
				if err != nil && sessionLastCode(err) < 0 {
					_ = writer.CloseWithError(err)
				} else {
					_ = writer.Close()
				}
			}
		}()
	}
	wg.Wait()
	for _, reader := range readers {
		_ = reader.Close()
	}

	lastErr := errs[len(errs)-1]
	s.lastCode = sessionLastCode(lastErr)
	if lastErr != nil {
		if s.lastCode >= 0 {
			return nil
		}
		return lastErr
	}
	for _, err := range errs[:len(errs)-1] {
		if sessionLastCode(err) == 130 {
			s.lastCode = 130
			return nil
		}
		if err != nil && sessionLastCode(err) < 0 {
			return err
		}
	}
	return nil
}

func (s *shellState) preparePipelineStage(base commandContext, segment string, stderr io.Writer) (pipelineStage, error) {
	ctx := base
	line := segment
	if strings.HasPrefix(segment, "@") {
		at, err := parseAtLine(segment)
		if err != nil {
			return pipelineStage{}, err
		}
		ctx = base.withOptions(at.Options)
		line = at.Command
		switch at.Target {
		case "":
			if at.Options.Sudo {
				ctx.Mode = modeVM
				ctx.User = "root"
			}
		case "host":
			if at.Options.Sudo {
				return pipelineStage{}, fmt.Errorf("usage: @host [cmd]")
			}
			ctx = hostCommandContext(base, at.Options)
		case "ssh":
			host, command, err := parseSSHAtCommand(at.Command)
			if err != nil {
				return pipelineStage{}, err
			}
			ctx = sshCommandContext(ctx, at.Options, host)
			line = command
		case "sudo":
			if line == "" {
				return pipelineStage{}, fmt.Errorf("usage: @sudo <cmd>")
			}
			ctx, line = sudoCommandContext(ctx, line)
		default:
			if isControlAtTarget(at.Target) {
				return pipelineStage{}, fmt.Errorf("@%s cannot be used in a pipeline", at.Target)
			}
			if strings.HasPrefix(at.Target, "vm:") {
				ctx, err = s.vmTargetCommandContext(strings.TrimPrefix(at.Target, "vm:"), at.Options)
				if err != nil {
					return pipelineStage{}, err
				}
			} else if sshCtx, ok := s.sshSessionContext(at.Target); ok {
				if len(at.Options.OptionFields) != 0 {
					return pipelineStage{}, fmt.Errorf("usage: @%s [cmd]", at.Target)
				}
				ctx = sshCtx
			} else {
				ctx = vmCommandContext(base, at.Options, at.Target)
			}
		}
		if line == "" {
			return pipelineStage{}, fmt.Errorf("pipeline segment %q has no command", segment)
		}
	}
	target, err := s.targetFor(ctx)
	if err != nil {
		return pipelineStage{}, err
	}
	command, err := target.PrepareRunWithInput(line, stderr)
	if err != nil {
		return pipelineStage{}, err
	}
	return pipelineStage{command: command}, nil
}

func isControlAtTarget(target string) bool {
	switch target {
	case "help", "?", "ps", "jobs", "alias", "status", "where", "start", "stop", "restart", "save", "rmi", "tmux", "forward", "copy", "cp", "agent", "ssh":
		return true
	default:
		return false
	}
}

func (s *shellState) runPipelineStage(stage pipelineStage, stdin io.Reader, stdout, stderr io.Writer) error {
	return stage.command.RunWithInput(stdin, stdout, stderr)
}

func (s *shellState) evalExport(line string) (bool, error) {
	fields, err := splitShellFields(line)
	if err != nil {
		return false, nil
	}
	if len(fields) == 0 || fields[0] != "export" {
		return false, nil
	}
	if s.env == nil {
		s.env = map[string]string{}
	}
	for _, field := range fields[1:] {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			value = os.Getenv(field)
		}
		if !isShellName(key) {
			return true, fmt.Errorf("export: invalid name %q", key)
		}
		s.env[key] = value
	}
	s.closeSessions()
	return true, nil
}

func (s *shellState) evalAlias(command string, stdout io.Writer) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return s.printAliases(stdout)
	}
	fields, err := splitShellFields(command)
	if err != nil {
		return err
	}
	if len(fields) == 2 && (fields[0] == "-d" || fields[0] == "--delete") {
		delete(s.aliases, fields[1])
		return nil
	}
	name, value, ok := parseAliasAssignment(command)
	if !ok {
		return fmt.Errorf("usage: @alias [name=value] | @alias -d name")
	}
	if !isAliasName(name) {
		return fmt.Errorf("alias: invalid name %q", name)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		delete(s.aliases, name)
		return nil
	}
	if s.aliases == nil {
		s.aliases = map[string]string{}
	}
	s.aliases[name] = value
	return nil
}

func (s *shellState) printAliases(w io.Writer) error {
	if len(s.aliases) == 0 {
		_, err := fmt.Fprintln(w, "No aliases")
		return err
	}
	names := make([]string, 0, len(s.aliases))
	for name := range s.aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := fmt.Fprintf(w, "%s=%s\n", name, s.aliases[name]); err != nil {
			return err
		}
	}
	return nil
}

func parseAliasAssignment(command string) (string, string, bool) {
	tokens, err := lexShellTokens(command)
	if err != nil || len(tokens) == 0 {
		return "", "", false
	}
	first := tokens[0]
	eq := strings.Index(first.Value, "=")
	if eq <= 0 {
		return "", "", false
	}
	name := first.Value[:eq]
	if strings.TrimSpace(command[first.End:]) == "" {
		return name, strings.TrimSpace(first.Value[eq+1:]), true
	}
	rawFirst := command[first.Start:first.End]
	rawEq := strings.Index(rawFirst, "=")
	if rawEq < 0 {
		return "", "", false
	}
	valueStart := first.Start + rawEq + 1
	return name, strings.TrimSpace(command[valueStart:]), true
}

func isAliasName(name string) bool {
	if name == "" || strings.HasPrefix(name, "@") {
		return false
	}
	return isShellName(name)
}

func isShellName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func stripBackground(line string) (string, bool, error) {
	tokens, err := lexShellTokens(line)
	if err != nil {
		return "", false, err
	}
	if len(tokens) == 0 || tokens[len(tokens)-1].Value != "&" {
		return "", false, nil
	}
	command := strings.TrimSpace(line[:tokens[len(tokens)-1].Start])
	if command == "" {
		return "", true, fmt.Errorf("background command is empty")
	}
	return command, true, nil
}

func (s *shellState) startBackgroundJob(ctx commandContext, line string, stdout, stderr io.Writer) error {
	bgShell := &shellState{
		api:              s.api,
		context:          ctx,
		hostCWD:          s.hostCWD,
		promptOut:        s.promptOut,
		env:              cloneEnv(s.env),
		aliases:          cloneEnv(s.aliases),
		confirmPull:      s.confirmPull,
		confirmVMRestart: s.confirmVMRestart,
		confirmSSHHost:   s.confirmSSHHost,
		sshPassword:      s.sshPassword,
		sshKeyboardAuth:  s.sshKeyboardAuth,
		sshBanner:        s.sshBanner,
		contextCWD:       cloneEnv(s.contextCWD),
	}
	s.jobsMu.Lock()
	s.nextJobID++
	id := s.nextJobID
	s.jobs = append(s.jobs, shellJob{ID: id, Context: ctx, Command: line})
	idx := len(s.jobs) - 1
	s.jobsMu.Unlock()
	fmt.Fprintf(stdout, "[%d] running %s\n", id, line)
	go func() {
		err := bgShell.runInContext(ctx, line, io.Discard, io.Discard)
		code := bgShell.lastCode
		s.jobsMu.Lock()
		s.jobs[idx].Done = true
		s.jobs[idx].Code = code
		if err != nil {
			s.jobs[idx].Err = err.Error()
		}
		s.jobsMu.Unlock()
		_ = stderr
	}()
	return nil
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func (s *shellState) evalAt(line string, stdout, stderr io.Writer) error {
	at, err := parseAtLine(line)
	if err != nil {
		return err
	}
	if at.Target == "" && len(at.Options.OptionFields) == 0 && at.Command == "" {
		return s.help(stdout)
	}
	if at.Target == "" {
		ctx := s.context.withOptions(at.Options)
		if at.Options.Sudo {
			ctx.Mode = modeVM
			ctx.User = "root"
		}
		if at.Command == "" {
			if at.Options.Sudo {
				return fmt.Errorf("usage: @ --sudo <cmd>")
			}
			if err := s.prepareActivatedVMContext(&ctx, stdout, stderr); err != nil {
				return err
			}
			s.activateContext(ctx)
			return nil
		}
		return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
	}

	switch at.Target {
	case "help", "?":
		if at.Command != "" || len(at.Options.OptionFields) != 0 {
			return fmt.Errorf("usage: @help")
		}
		return s.help(stdout)
	case "host":
		ctx := hostCommandContext(s.context, at.Options)
		if at.Options.Sudo {
			return fmt.Errorf("usage: @host [cmd]")
		}
		if at.Command == "" {
			s.activateContext(ctx)
			return nil
		}
		return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
	case "ssh":
		host, command, err := parseSSHAtCommand(at.Command)
		if err != nil {
			return err
		}
		ctx := sshCommandContext(s.context, at.Options, host)
		if command == "" {
			session, err := s.sshPersistentShell(ctx, stdout, stderr)
			if err != nil {
				return err
			}
			if cwd := session.cwd(); cwd != "" {
				ctx.CWD = cwd
			}
			s.activateContext(ctx)
			return nil
		}
		return s.runMaybeBackground(ctx, command, stdout, stderr)
	case "ps":
		if at.Command != "" || len(at.Options.OptionFields) != 0 {
			return fmt.Errorf("usage: @ps")
		}
		return s.printVMs(stdout)
	case "jobs":
		if at.Command != "" || len(at.Options.OptionFields) != 0 {
			return fmt.Errorf("usage: @jobs")
		}
		return s.printJobs(stdout)
	case "alias":
		if len(at.Options.OptionFields) != 0 {
			return fmt.Errorf("usage: @alias [name=value] | @alias -d name")
		}
		return s.evalAlias(at.Command, stdout)
	case "status", "where":
		if at.Command != "" || len(at.Options.OptionFields) != 0 {
			return fmt.Errorf("usage: @%s", at.Target)
		}
		return s.printStatus(stdout)
	case "sudo":
		ctx := s.context.withOptions(at.Options)
		if at.Command == "" {
			return s.enterSudoSubshell(ctx, stdout, stderr)
		}
		ctx, command := sudoCommandContext(ctx, at.Command)
		return s.runMaybeBackground(ctx, command, stdout, stderr)
	case "start":
		if at.Command != "" {
			return fmt.Errorf("usage: @start [--vm id]")
		}
		ctx := s.context.withOptions(at.Options)
		return s.ensureVMRunning(ctx, stderr)
	case "stop":
		return s.stopSession(at, stdout)
	case "restart":
		if at.Command != "" {
			return fmt.Errorf("usage: @restart [--vm id]")
		}
		ctx := s.context.withOptions(at.Options)
		id := backendVMID(ctx)
		return s.restartVM(id, ctx, stderr)
	case "save":
		return s.saveVM(at, stdout)
	case "rmi":
		return s.removeImage(at, stdout)
	case "tmux":
		return s.startTmux(at)
	case "forward":
		if at.Command == "" {
			return fmt.Errorf("usage: @forward <host-port:guest-port>")
		}
		fields, err := splitShellFields(at.Command)
		if err != nil {
			return err
		}
		if len(fields) != 1 {
			return fmt.Errorf("usage: @forward <host-port:guest-port>")
		}
		forward, err := parsePortForwardSpec(fields[0])
		if err != nil {
			return err
		}
		ctx := s.context.withOptions(at.Options)
		id := backendVMID(ctx)
		return s.api.AddPortForwardTo(id, forward)
	case "copy", "cp":
		if len(at.Options.OptionFields) != 0 {
			return fmt.Errorf("usage: @copy SRC DST")
		}
		return s.copyPath(at.Command, stdout, stderr)
	case "agent":
		return s.runAgent(at, stdout, stderr)
	default:
		if strings.HasPrefix(at.Target, "vm:") {
			ctx, err := s.vmTargetCommandContext(strings.TrimPrefix(at.Target, "vm:"), at.Options)
			if err != nil {
				return err
			}
			if at.Command == "" {
				if at.Options.Sudo {
					return fmt.Errorf("usage: @%s --sudo <cmd>", at.Target)
				}
				if err := s.prepareActivatedVMContext(&ctx, stdout, stderr); err != nil {
					return err
				}
				s.activateContext(ctx)
				return nil
			}
			return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
		}
		if ctx, ok := s.sshSessionContext(at.Target); ok {
			if len(at.Options.OptionFields) != 0 {
				return fmt.Errorf("usage: @%s [cmd]", at.Target)
			}
			if at.Command == "" {
				s.activateContext(ctx)
				return nil
			}
			return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
		}
		ctx := vmCommandContext(s.context, at.Options, at.Target)
		if at.Command == "" {
			if at.Options.Sudo {
				return fmt.Errorf("usage: @%s --sudo <cmd>", at.Target)
			}
			if err := s.prepareActivatedVMContext(&ctx, stdout, stderr); err != nil {
				return err
			}
			s.activateContext(ctx)
			return nil
		}
		return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
	}
}

func (s *shellState) vmTargetCommandContext(id string, opts commandOptions) (commandContext, error) {
	ctx, err := s.vmCopyEndpointContext(strings.TrimSpace(id))
	if err != nil {
		return commandContext{}, err
	}
	ctx = ctx.withOptions(opts)
	return ctx, nil
}

func (s *shellState) prepareActivatedVMContext(ctx *commandContext, stdout, stderr io.Writer) error {
	if ctx == nil || ctx.Mode != modeVM {
		return nil
	}
	if ctx.Image == "" {
		return nil
	}
	if err := s.ensureImageAvailable(*ctx, stderr); err != nil {
		return err
	}
	return s.ensureVMRunning(*ctx, stderr)
}

func (s *shellState) runHost(line string, stdout, stderr io.Writer) error {
	tty, cols, rows := terminalRequestSize(stdout)
	env := []string(nil)
	if tty {
		env = hostCommandEnv(s.env, terminalEnv(cols, rows))
	} else if len(s.env) > 0 {
		env = mergedEnv(os.Environ(), shellEnv(s.env))
	}
	if persistentHostCommandAllowed(line) {
		session, err := s.hostPersistentShell(env, cols, rows, stderr)
		if err == nil {
			var interrupted *atomic.Bool
			err = session.run(line, stdout, stderr, func() (func(), error) {
				interrupts := newCommandInterruptEscalator(line, stderr, nil, func() {
					if s.hostShell == session {
						s.hostShell = nil
					}
					go session.close()
				})
				stop, state, err := s.startHostPTYForwarding(tty, session, stdout, stderr, interrupts.Interrupt)
				interrupted = state
				return stop, err
			})
			if interrupted != nil && interrupted.Load() && err != nil && sessionLastCode(err) < 0 {
				err = persistentShellExit{code: 130}
				if s.hostShell == session {
					s.hostShell = nil
				}
			}
			s.lastCode = sessionLastCode(err)
			if err == nil || s.lastCode >= 0 {
				if session.cwd() != "" {
					s.hostCWD = session.cwd()
					_ = os.Chdir(s.hostCWD)
				}
				return nil
			}
		}
	}
	args := hostShellCommand(line, tty, s.hostCommandPrelude(tty))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = s.hostCWD
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		s.lastCode = exitCode(err)
		if s.lastCode < 0 {
			return err
		}
		return nil
	}
	interrupts := newCommandInterruptEscalator(line, stderr, func() {
		_ = cmd.Process.Signal(os.Interrupt)
	}, func() {
		_ = cmd.Process.Kill()
	})
	stopInterrupts, interrupted := s.startInterruptWatcher(interrupts.Interrupt)
	err := cmd.Wait()
	stopInterrupts()
	if interrupted.Load() {
		s.lastCode = 130
		return nil
	}
	s.lastCode = exitCode(err)
	if err != nil && s.lastCode < 0 {
		return err
	}
	return nil
}

func (s *shellState) runHostWithInput(line string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := hostShellCommand(line, false, s.hostCommandPrelude(false))
	cmdCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
	cmd.Dir = s.hostCWD
	if stdin == nil {
		cmd.Stdin = nil
	} else {
		cmd.Stdin = stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(s.env) > 0 {
		cmd.Env = mergedEnv(os.Environ(), shellEnv(s.env))
	}
	err := cmd.Run()
	if interrupted.Load() {
		return persistentShellExit{code: 130}
	}
	code := exitCode(err)
	if err != nil && code < 0 {
		return err
	}
	if code != 0 {
		return persistentShellExit{code: code}
	}
	return nil
}

func sshRemoteCommandScript(ctx commandContext, line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		line = ":"
	}
	cwd := strings.TrimSpace(ctx.CWD)
	if cwd == "" || cwd == "~" {
		return line
	}
	return remoteCDCommand(cwd) + " && " + line
}

type copyEndpoint struct {
	target    shellTarget
	path      string
	directory bool
}

func (e copyEndpoint) context() commandContext {
	if e.target == nil {
		return commandContext{}
	}
	return e.target.Context()
}

func (e copyEndpoint) targetPath() copyTargetPath {
	return copyTargetPath{path: e.path, directory: e.directory}
}

func (s *shellState) copyPath(command string, stdout, stderr io.Writer) error {
	fields, err := splitShellFields(command)
	if err != nil {
		return err
	}
	if len(fields) != 2 {
		return fmt.Errorf("usage: @copy SRC DST")
	}
	src, err := s.parseCopyEndpoint(fields[0])
	if err != nil {
		return err
	}
	dst, err := s.parseCopyEndpoint(fields[1])
	if err != nil {
		return err
	}
	srcHost, srcOK := src.target.LocalPath(src.path)
	dstHost, dstOK := dst.target.LocalPath(dst.path)
	if srcOK && dstOK {
		return wrapCopyPathError(fields[0], fields[1], copyHostPath(srcHost, copyTargetPath{path: dstHost, directory: dst.directory}))
	}
	if srcOK {
		return wrapCopyPathError(fields[0], fields[1], dst.target.CopyFromLocal(srcHost, dst.targetPath(), stderr))
	}
	if dstOK {
		return wrapCopyPathError(fields[0], fields[1], src.target.CopyToLocal(src.targetPath(), copyTargetPath{path: dstHost, directory: dst.directory}, stderr))
	}
	return wrapCopyPathError(fields[0], fields[1], copyRemoteToRemote(src, dst, stderr))
}

func wrapCopyPathError(src, dst string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
}

func copyRemoteToRemote(src, dst copyEndpoint, stderr io.Writer) error {
	tmpRoot, err := os.MkdirTemp("", "vmsh-copy-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)
	staged := filepath.Join(tmpRoot, copyStageBaseName(src.path))
	if err := src.target.CopyToLocal(src.targetPath(), copyTargetPath{path: staged}, stderr); err != nil {
		return err
	}
	return dst.target.CopyFromLocal(staged, dst.targetPath(), stderr)
}

func copyStageBaseName(value string) string {
	name := path.Base(path.Clean(filepath.ToSlash(strings.TrimSpace(value))))
	switch name {
	case "", ".", "/":
		return "copy"
	default:
		return name
	}
}

func (s *shellState) parseCopyEndpoint(raw string) (copyEndpoint, error) {
	ctx := s.context
	value := raw
	if strings.HasPrefix(raw, "@") {
		target, rest, ok := strings.Cut(strings.TrimPrefix(raw, "@"), ":")
		if !ok {
			return copyEndpoint{}, fmt.Errorf("copy endpoint %q must use @target:path", raw)
		}
		value = rest
		switch target {
		case "host":
			ctx = hostCommandContext(s.context, commandOptions{})
		case "vm":
			vmID, vmPath, ok := strings.Cut(rest, ":")
			if !ok || strings.TrimSpace(vmID) == "" {
				return copyEndpoint{}, fmt.Errorf("copy endpoint %q must use @vm:id:path", raw)
			}
			resolved, err := s.vmCopyEndpointContext(strings.TrimSpace(vmID))
			if err != nil {
				return copyEndpoint{}, err
			}
			ctx = resolved
			value = vmPath
		case "ssh":
			host, sshPath, ok := strings.Cut(rest, ":")
			if !ok || strings.TrimSpace(host) == "" {
				return copyEndpoint{}, fmt.Errorf("copy endpoint %q must use @ssh:host:path", raw)
			}
			ctx = sshCommandContext(s.context, commandOptions{}, strings.TrimSpace(host))
			value = sshPath
		case "image":
			image, imagePath, ok := strings.Cut(rest, ":")
			if !ok || strings.TrimSpace(image) == "" {
				return copyEndpoint{}, fmt.Errorf("copy endpoint %q must use @image:name:path", raw)
			}
			ctx = vmCommandContext(s.context, commandOptions{}, strings.TrimSpace(image))
			value = imagePath
		case "":
			ctx = s.context
		default:
			resolved, err := s.resolveImplicitCopyEndpointTarget(target, raw)
			if err != nil {
				return copyEndpoint{}, err
			}
			ctx = resolved
		}
	}
	target, err := s.targetFor(ctx)
	if err != nil {
		return copyEndpoint{}, err
	}
	return copyEndpoint{
		target:    target,
		path:      target.ResolveCopyPath(value),
		directory: copyEndpointDirectoryHint(value),
	}, nil
}

type copyEndpointTargetMatch struct {
	kind     string
	explicit string
	ctx      commandContext
}

func (s *shellState) resolveImplicitCopyEndpointTarget(name, raw string) (commandContext, error) {
	name = strings.TrimSpace(name)
	var matches []copyEndpointTargetMatch
	if sshCtx, ok := s.sshSessionContext(name); ok {
		matches = append(matches, copyEndpointTargetMatch{
			kind:     "active SSH session",
			explicit: fmt.Sprintf("@ssh:%s:path", name),
			ctx:      sshCtx,
		})
	}
	if vmCtx, ok, err := s.knownVMCopyEndpointContext(name); err != nil {
		return commandContext{}, err
	} else if ok {
		matches = append(matches, copyEndpointTargetMatch{
			kind:     "VM",
			explicit: fmt.Sprintf("@vm:%s:path", name),
			ctx:      vmCtx,
		})
	}
	if imageCtx, ok := s.knownImageCopyEndpointContext(name); ok {
		matches = append(matches, copyEndpointTargetMatch{
			kind:     "image",
			explicit: fmt.Sprintf("@image:%s:path", name),
			ctx:      imageCtx,
		})
	}
	switch len(matches) {
	case 0:
		return commandContext{}, fmt.Errorf("copy endpoint %q does not name an active SSH session, known VM, or known image; use @:path, @host:path, @vm:name:path, @ssh:host:path, or @image:name:path", raw)
	case 1:
		return matches[0].ctx, nil
	default:
		parts := make([]string, 0, len(matches))
		for _, match := range matches {
			parts = append(parts, match.kind+" ("+match.explicit+")")
		}
		return commandContext{}, fmt.Errorf("copy endpoint %q is ambiguous: %s", raw, strings.Join(parts, ", "))
	}
}

func (s *shellState) knownImageCopyEndpointContext(name string) (commandContext, bool) {
	name = strings.TrimSpace(name)
	if name == "" || s.api == nil {
		return commandContext{}, false
	}
	state, err := s.api.GetImage(name)
	if err != nil || strings.TrimSpace(state.Name) == "" {
		return commandContext{}, false
	}
	ctx := vmCommandContext(s.context, commandOptions{}, name)
	if s.contextCWD != nil {
		if cwd := s.contextCWD[contextCWDKey(ctx)]; cwd != "" {
			ctx.CWD = cwd
		}
	}
	return ctx, true
}

func (s *shellState) knownVMCopyEndpointContext(id string) (commandContext, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return commandContext{}, false, nil
	}
	if s.context.Mode == modeVM && backendVMID(s.context) == backendVMIDFor(id, false) && s.context.Image != "" {
		return s.context, true, nil
	}
	for _, ctx := range s.contextStack {
		if ctx.Mode == modeVM && backendVMID(ctx) == backendVMIDFor(id, false) && ctx.Image != "" {
			return ctx, true, nil
		}
	}
	if s.api == nil {
		return commandContext{}, false, nil
	}
	state, err := s.api.InstanceStatusOf(id)
	if err != nil {
		return commandContext{}, false, err
	}
	if strings.TrimSpace(state.Image) == "" {
		return commandContext{}, false, nil
	}
	ctx := commandContext{
		Mode:       modeVM,
		VMID:       firstNonEmpty(state.ID, id),
		Image:      state.Image,
		InitSystem: state.InitSystem,
		Kernel:     state.Kernel,
		MemoryMB:   state.MemoryMB,
		CPUs:       state.CPUs,
		NestedVirt: state.NestedVirt,
		Network:    state.NetworkIPv4 != "",
		Isolated:   strings.HasSuffix(firstNonEmpty(state.ID, id), "-isolated"),
	}
	if s.contextCWD != nil {
		if cwd := s.contextCWD[contextCWDKey(ctx)]; cwd != "" {
			ctx.CWD = cwd
		}
	}
	return ctx, true, nil
}

func (s *shellState) vmCopyEndpointContext(id string) (commandContext, error) {
	if s.context.Mode == modeVM && backendVMID(s.context) == backendVMIDFor(id, false) {
		return s.context, nil
	}
	for _, ctx := range s.contextStack {
		if ctx.Mode == modeVM && backendVMID(ctx) == backendVMIDFor(id, false) {
			return ctx, nil
		}
	}
	state, err := s.api.InstanceStatusOf(id)
	if err != nil {
		return commandContext{}, err
	}
	if strings.TrimSpace(state.Image) == "" {
		return commandContext{}, fmt.Errorf("VM %s has no image; use @<image>:path or switch to the VM first", id)
	}
	ctx := commandContext{
		Mode:       modeVM,
		VMID:       firstNonEmpty(state.ID, id),
		Image:      state.Image,
		InitSystem: state.InitSystem,
		Kernel:     state.Kernel,
		MemoryMB:   state.MemoryMB,
		CPUs:       state.CPUs,
		NestedVirt: state.NestedVirt,
		Network:    state.NetworkIPv4 != "",
	}
	if s.contextCWD != nil {
		if cwd := s.contextCWD[contextCWDKey(ctx)]; cwd != "" {
			ctx.CWD = cwd
		}
	}
	return ctx, nil
}

func copyEndpointDirectoryHint(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == "." || value == "~" || strings.HasSuffix(value, "/")
}

func (s *shellState) resolveHostCopyPath(value string) string {
	if value == "" {
		value = "."
	}
	if strings.HasPrefix(value, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			switch {
			case value == "~":
				value = home
			case strings.HasPrefix(value, "~/"):
				value = filepath.Join(home, value[2:])
			}
		}
	}
	value = os.ExpandEnv(value)
	if !filepath.IsAbs(value) {
		value = filepath.Join(s.hostCWD, value)
	}
	return filepath.Clean(value)
}

func (s *shellState) resolveGuestCopyPath(ctx commandContext, value string) string {
	if value == "" {
		value = "."
	}
	home := guestHomeDir(ctx)
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return path.Clean(path.Join(home, value[2:]))
	}
	if strings.HasPrefix(value, "/") {
		return path.Clean(value)
	}
	return path.Clean(path.Join(s.currentGuestCWD(ctx), filepath.ToSlash(value)))
}

func (s *shellState) resolveSSHCopyPath(ctx commandContext, value string) string {
	value = filepath.ToSlash(strings.TrimSpace(value))
	if value == "" {
		value = "."
	}
	if strings.HasPrefix(value, "~") || strings.HasPrefix(value, "/") {
		return path.Clean(value)
	}
	return path.Clean(path.Join(s.currentSSHCWD(ctx), value))
}

func (s *shellState) currentGuestCWD(ctx commandContext) string {
	if ctx.Mode != modeVM {
		return ""
	}
	if ctx.CWD != "" {
		return ctx.CWD
	}
	if s.contextCWD != nil {
		if cwd := s.contextCWD[contextCWDKey(ctx)]; cwd != "" {
			return cwd
		}
	}
	if ctx.Isolated {
		return guestHomeDir(ctx)
	}
	_, guestCWD, err := guestHostPaths(s.hostCWD)
	if err != nil {
		return guestHomeDir(ctx)
	}
	return guestCWD
}

func (s *shellState) currentSSHCWD(ctx commandContext) string {
	if ctx.Mode != modeSSH {
		return ""
	}
	if ctx.CWD != "" {
		return ctx.CWD
	}
	if shell := s.sshShellForContext(ctx); shell != nil {
		if cwd := shell.cwd(); cwd != "" {
			return cwd
		}
	}
	if s.contextCWD != nil {
		if cwd := s.contextCWD[contextCWDKey(ctx)]; cwd != "" {
			return cwd
		}
	}
	return "~"
}

func copyHostPath(src string, dst copyTargetPath) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		err := writePathTar(pw, src, filepath.Base(src))
		_ = pw.CloseWithError(err)
		errCh <- err
	}()
	extractErr := extractTarToHost(pr, dst)
	_ = pr.CloseWithError(extractErr)
	writeErr := <-errCh
	if extractErr != nil {
		return extractErr
	}
	return writeErr
}

func (s *shellState) copyLocalToGuest(src string, ctx commandContext, dst copyTargetPath, stderr io.Writer) error {
	if err := s.ensureGuestCopyReady(ctx, stderr); err != nil {
		return err
	}
	var archive bytes.Buffer
	if err := writePathTar(&archive, src, filepath.Base(src)); err != nil {
		return err
	}
	return s.guestFSExtract(ctx, dst, archive.Bytes(), stderr)
}

func (s *shellState) guestFSMkdir(ctx commandContext, dir string, stderr io.Writer) error {
	return s.runGuestFSRequest(ctx, client.ExecRequest{
		Kind:  "fs_mkdir",
		Image: localImageName(ctx.Image, ctx.Arch),
		Path:  dir,
		User:  guestRunUser(ctx),
	}, stderr)
}

func (s *shellState) guestFSExtract(ctx commandContext, dst copyTargetPath, data []byte, stderr io.Writer) error {
	return s.runGuestFSRequest(ctx, client.ExecRequest{
		Kind:      "fs_extract",
		Image:     localImageName(ctx.Image, ctx.Arch),
		Path:      dst.path,
		Directory: dst.directory,
		User:      guestRunUser(ctx),
		Stdin:     append([]byte(nil), data...),
	}, stderr)
}

func (s *shellState) runGuestFSRequest(ctx commandContext, req client.ExecRequest, stderr io.Writer) error {
	var exitSeen bool
	var exitCode int
	var eventErr error
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	if err := s.execStreamInContext(runCtx, backendVMID(ctx), req, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stderr":
			writeExecEventOutput(stderr, event)
		case "error":
			eventErr = fmt.Errorf("%s", firstNonEmpty(event.Error, execEventText(event)))
		case "exit":
			exitSeen = true
			exitCode = event.ExitCode
		}
		return nil
	}); err != nil {
		if interrupted.Load() {
			return persistentShellExit{code: 130}
		}
		return err
	}
	if interrupted.Load() {
		return persistentShellExit{code: 130}
	}
	if eventErr != nil {
		return eventErr
	}
	if exitSeen && exitCode != 0 {
		return persistentShellExit{code: exitCode}
	}
	if !exitSeen {
		return fmt.Errorf("guest copy did not report completion")
	}
	return nil
}

func (s *shellState) copyGuestToLocal(ctx commandContext, src, dst copyTargetPath, stderr io.Writer) error {
	if err := s.ensureGuestCopyReady(ctx, stderr); err != nil {
		return err
	}
	var tarData bytes.Buffer
	var exitSeen bool
	var exitCode int
	var eventErr error
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	if err := s.execStreamInContext(runCtx, backendVMID(ctx), client.ExecRequest{
		Kind:  "fs_archive",
		Image: localImageName(ctx.Image, ctx.Arch),
		Path:  src.path,
		User:  guestRunUser(ctx),
	}, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			_, _ = tarData.Write(execEventBytes(event))
		case "stderr":
			writeExecEventOutput(stderr, event)
		case "error":
			eventErr = fmt.Errorf("%s", firstNonEmpty(event.Error, execEventText(event)))
		case "exit":
			exitSeen = true
			exitCode = event.ExitCode
		}
		return nil
	}); err != nil {
		if interrupted.Load() {
			return persistentShellExit{code: 130}
		}
		return err
	}
	if interrupted.Load() {
		return persistentShellExit{code: 130}
	}
	if eventErr != nil {
		return eventErr
	}
	if exitSeen && exitCode != 0 {
		return persistentShellExit{code: exitCode}
	}
	if !exitSeen {
		return fmt.Errorf("guest copy did not report completion")
	}
	return extractTarToHost(bytes.NewReader(tarData.Bytes()), dst)
}

func (s *shellState) ensureGuestCopyReady(ctx commandContext, stderr io.Writer) error {
	if ctx.Mode != modeVM {
		return nil
	}
	if ctx.Image == "" {
		return fmt.Errorf("no guest image selected; run @<oci-tag> or set one with @<oci-tag>")
	}
	if err := s.ensureImageAvailable(ctx, stderr); err != nil {
		return err
	}
	if err := s.ensureVMRunning(ctx, stderr); err != nil {
		return err
	}
	s.closeGuestShellForControlRequest(ctx)
	return nil
}

func (s *shellState) closeGuestShellForControlRequest(ctx commandContext) {
	if s.guestShell == nil {
		return
	}
	if !strings.HasPrefix(s.guestShell.key, backendVMID(ctx)+"\x00") {
		return
	}
	s.guestShell.close()
	s.guestShell = nil
}

func writePathTar(w io.Writer, src, rootName string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	defer tw.Close()
	rootName = filepath.ToSlash(filepath.Base(rootName))
	return filepath.WalkDir(src, func(filePath string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		fileInfo, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Dir(src), filePath)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = filepath.Base(src)
			fileInfo = info
		}
		name := filepath.ToSlash(rel)
		if rootName != "" {
			parts := strings.SplitN(name, "/", 2)
			if len(parts) == 1 {
				name = rootName
			} else {
				name = rootName + "/" + parts[1]
			}
		}
		link := ""
		if fileInfo.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(fileInfo, link)
		if err != nil {
			return err
		}
		header.Name = name
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func extractTarToHost(r io.Reader, dst copyTargetPath) error {
	mode := hostCopyDestMode(dst.path, dst.directory)
	tr := tar.NewReader(r)
	var dirs []tarDirMtime
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return restoreTarDirMtimes(dirs)
		}
		if err != nil {
			return err
		}
		target, err := hostTarTarget(dst.path, mode, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode).Perm()); err != nil {
				return err
			}
			_ = os.Chmod(target, os.FileMode(header.Mode).Perm())
			dirs = append(dirs, tarDirMtime{path: target, mtime: header.ModTime})
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
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
			perm := os.FileMode(header.Mode).Perm()
			if err := os.Chmod(target, perm); err != nil {
				return err
			}
			if err := os.Chtimes(target, header.ModTime, header.ModTime); err != nil {
				return err
			}
		default:
			continue
		}
	}
}

type tarDirMtime struct {
	path  string
	mtime time.Time
}

func restoreTarDirMtimes(dirs []tarDirMtime) error {
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		if dir.mtime.IsZero() {
			continue
		}
		if err := os.Chtimes(dir.path, dir.mtime, dir.mtime); err != nil {
			return err
		}
	}
	return nil
}

type copyDestMode int

const (
	copyDestIntoDir copyDestMode = iota
	copyDestExact
)

func hostCopyDestMode(dst string, directoryHint bool) copyDestMode {
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		return copyDestIntoDir
	}
	if directoryHint || strings.HasSuffix(dst, string(filepath.Separator)) {
		return copyDestIntoDir
	}
	return copyDestExact
}

func hostTarTarget(dst string, mode copyDestMode, name string) (string, error) {
	cleanName := path.Clean(strings.TrimPrefix(filepath.ToSlash(name), "/"))
	if cleanName == "." || strings.HasPrefix(cleanName, "../") || cleanName == ".." {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	if mode == copyDestIntoDir {
		return filepath.Join(dst, filepath.FromSlash(cleanName)), nil
	}
	parts := strings.SplitN(cleanName, "/", 2)
	if len(parts) == 1 {
		return dst, nil
	}
	return filepath.Join(dst, filepath.FromSlash(parts[1])), nil
}

func copyHostDir(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(dst, mode.Perm()); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode().IsRegular() {
			return copyHostFile(path, target, info.Mode())
		}
		return nil
	})
}

func copyHostFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (s *shellState) activateContext(ctx commandContext) {
	s.rememberContextCWD(s.context)
	if (ctx.Mode == modeVM || ctx.Mode == modeSSH) && ctx.CWD == "" {
		if cwd := s.contextCWD[contextCWDKey(ctx)]; cwd != "" {
			ctx.CWD = cwd
		}
	}
	if ctx.Mode == modeSSH {
		if shell := s.sshShellForContext(ctx); shell != nil {
			if cwd := shell.cwd(); cwd != "" {
				ctx.CWD = cwd
			}
		}
	}
	s.context = ctx
}

func (s *shellState) enterSubshell(ctx commandContext) {
	s.contextStack = append(s.contextStack, s.context)
	s.activateContext(ctx)
}

func (s *shellState) enterSudoSubshell(ctx commandContext, stdout, stderr io.Writer) error {
	ctx = sudoSubshellContext(ctx)
	tty, cols, rows := terminalRequestSize(stdout)
	if tty {
		req, err := s.prepareGuestRunRequest(ctx, ":", true, cols, rows, stderr)
		if err != nil {
			return err
		}
		session, err := s.guestPersistentShell(ctx, req)
		if err != nil {
			return err
		}
		if cwd := session.cwd(); cwd != "" {
			ctx.CWD = cwd
		}
	} else {
		if ctx.Image == "" {
			return fmt.Errorf("no guest image selected; run @<oci-tag> first")
		}
		if err := s.ensureImageAvailable(ctx, stderr); err != nil {
			return err
		}
	}
	s.enterSubshell(ctx)
	return nil
}

func (s *shellState) exitSubshell() bool {
	if len(s.contextStack) == 0 {
		return false
	}
	last := len(s.contextStack) - 1
	ctx := s.contextStack[last]
	s.contextStack = s.contextStack[:last]
	s.activateContext(ctx)
	return true
}

type exitResource struct {
	Kind   string
	Name   string
	Detail string
}

func (s *shellState) confirmExitIfNeeded(stderr io.Writer) (bool, error) {
	if s.confirmExit == nil {
		return true, nil
	}
	resources, err := s.activeExitResources()
	if err != nil {
		return false, err
	}
	if len(resources) == 0 {
		return true, nil
	}
	return s.confirmExit(resources, stderr)
}

func (s *shellState) activeExitResources() ([]exitResource, error) {
	var resources []exitResource
	if s.api != nil {
		states, err := s.api.InstanceStatuses()
		if err != nil {
			return nil, err
		}
		for _, state := range states {
			status := strings.TrimSpace(state.Status)
			if status == "" || strings.EqualFold(status, "stopped") {
				continue
			}
			kind := "VM"
			name := firstNonEmpty(strings.TrimSpace(state.ID), "default")
			if strings.HasSuffix(name, isolatedVMSuffix) {
				kind = "isolated VM"
			}
			var details []string
			details = append(details, status)
			if state.Image != "" {
				details = append(details, "image="+state.Image)
			}
			resources = append(resources, exitResource{
				Kind:   kind,
				Name:   name,
				Detail: strings.Join(details, ", "),
			})
		}
	}
	for _, session := range s.sshSessionStates() {
		var details []string
		if session.User != "" {
			details = append(details, "user="+session.User)
		}
		if session.CWD != "" {
			details = append(details, "cwd="+session.CWD)
		}
		resources = append(resources, exitResource{
			Kind:   "SSH session",
			Name:   session.Name,
			Detail: strings.Join(details, ", "),
		})
	}
	for _, conn := range s.sshConnectionStates() {
		resources = append(resources, exitResource{
			Kind:   "SSH connection",
			Name:   conn.Name,
			Detail: conn.Detail,
		})
	}
	s.jobsMu.Lock()
	for _, job := range s.jobs {
		if job.Done {
			continue
		}
		resources = append(resources, exitResource{
			Kind:   "background job",
			Name:   fmt.Sprintf("[%d]", job.ID),
			Detail: strings.TrimSpace(job.Command),
		})
	}
	s.jobsMu.Unlock()
	sort.SliceStable(resources, func(i, j int) bool {
		if resources[i].Kind != resources[j].Kind {
			return resources[i].Kind < resources[j].Kind
		}
		return resources[i].Name < resources[j].Name
	})
	return resources, nil
}

func (s *shellState) rememberContextCWD(ctx commandContext) {
	if (ctx.Mode != modeVM && ctx.Mode != modeSSH) || ctx.CWD == "" {
		return
	}
	if s.contextCWD == nil {
		s.contextCWD = map[string]string{}
	}
	s.contextCWD[contextCWDKey(ctx)] = ctx.CWD
}

func contextCWDKey(ctx commandContext) string {
	return strings.Join([]string{
		string(ctx.Mode),
		ctx.VMID,
		localImageName(ctx.Image, ctx.Arch),
		ctx.SSHHost,
		strconv.FormatBool(ctx.Isolated),
		contextUserKey(ctx),
	}, "\x00")
}

func contextUserKey(ctx commandContext) string {
	if ctx.Mode == modeSSH {
		return strings.TrimSpace(ctx.User)
	}
	return guestRunUser(ctx)
}

func backendVMID(ctx commandContext) string {
	return backendVMIDFor(ctx.VMID, ctx.Isolated)
}

func backendVMIDFor(id string, isolated bool) string {
	id = normalizedVMID(id)
	if isolated {
		return id + isolatedVMSuffix
	}
	return id
}

func normalizedVMID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "default"
	}
	return id
}

func sessionLastCode(err error) int {
	var exit persistentShellExit
	if errors.As(err, &exit) {
		return exit.code
	}
	return exitCode(err)
}

var (
	errPersistentGuestShellExited = errors.New("persistent guest shell exited")
	errPersistentGuestShellClosed = errors.New("persistent guest shell closed")
)

type persistentShellExit struct {
	code int
}

func (e persistentShellExit) Error() string {
	return fmt.Sprintf("exit status %d", e.code)
}

func persistentHostCommandAllowed(line string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	return persistentShellCommandAllowed(line)
}

func persistentShellCommandAllowed(line string) bool {
	if strings.ContainsAny(line, "\r\n") {
		return false
	}
	fields, err := splitShellFields(line)
	return err == nil && len(fields) > 0
}

func (s *shellState) hostPersistentShell(env []string, cols, rows int, stderr io.Writer) (*persistentHostShell, error) {
	if s.hostShell != nil {
		return s.hostShell, nil
	}
	session, err := startPersistentHostShell(s.hostCWD, env, cols, rows, s.hostCommandPrelude(true))
	if err != nil {
		return nil, err
	}
	s.hostShell = session
	return session, nil
}

func startPersistentHostShell(cwd string, env []string, cols, rows int, prelude string) (*persistentHostShell, error) {
	script := prelude + persistentHostShellScript()
	cmd := exec.Command(hostShell(), "-ic", script)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer controlWrite.Close()
	cmd.ExtraFiles = []*os.File{controlWrite}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		_ = controlRead.Close()
		return nil, err
	}
	session := &persistentHostShell{
		cmd:         cmd,
		tty:         tty,
		stdin:       tty,
		stdout:      bufio.NewReader(tty),
		control:     bufio.NewReader(controlRead),
		controlFile: controlRead,
		lastCWD:     cwd,
		done:        make(chan error, 1),
	}
	go func() {
		session.done <- cmd.Wait()
	}()
	go session.forwardPTYOutput()
	if err := session.waitReady(); err != nil {
		session.close()
		return nil, err
	}
	return session, nil
}

func persistentHostShellScript() string {
	lines := []string{
		"set -m 2>/dev/null || true",
		"stty -echo 2>/dev/null || true",
	}
	if filepath.Base(hostShell()) == "bash" {
		lines = append(lines, bashHostShellOptionsPrelude())
	}
	lines = append(lines, []string{
		"__vmsh_control_fd=3",
		"__vmsh_report() {",
		"  printf '%s\\t%s\\t%s\\n' \"$1\" \"$2\" \"$PWD\" >&$__vmsh_control_fd",
		"}",
		"__vmsh_run() {",
		"  stty echo 2>/dev/null || true",
		"  eval \"$1\"",
		"  __vmsh_status=$?",
		"  stty -echo 2>/dev/null || true",
		"  __vmsh_report done \"$__vmsh_status\"",
		"}",
		"__vmsh_report ready 0",
		"while IFS= read -r __vmsh_line; do eval \"$__vmsh_line\"; done",
	}...)
	return strings.Join(lines, "\n")
}

func (p *persistentHostShell) waitReady() error {
	for {
		record, err := p.readControlRecord()
		if err != nil {
			return err
		}
		if record.kind == "ready" {
			p.lastCWD = record.cwd
			return nil
		}
	}
}

type persistentHostControlRecord struct {
	kind string
	code int
	cwd  string
}

func (p *persistentHostShell) readControlRecord() (persistentHostControlRecord, error) {
	return readPersistentControlRecord(p.control)
}

func readPersistentControlRecord(reader *bufio.Reader) (persistentHostControlRecord, error) {
	text, err := reader.ReadString('\n')
	if err != nil {
		return persistentHostControlRecord{}, err
	}
	return parsePersistentControlRecord(text)
}

func parsePersistentControlRecord(text string) (persistentHostControlRecord, error) {
	text = strings.TrimRight(text, "\r\n")
	parts := strings.SplitN(text, "\t", 3)
	if len(parts) != 3 {
		return persistentHostControlRecord{}, fmt.Errorf("invalid host shell control record %q", text)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return persistentHostControlRecord{}, fmt.Errorf("invalid host shell control status %q", parts[1])
	}
	return persistentHostControlRecord{kind: parts[0], code: code, cwd: parts[2]}, nil
}

func (p *persistentHostShell) forwardPTYOutput() {
	buf := make([]byte, 4096)
	for {
		n, err := p.stdout.Read(buf)
		if n > 0 {
			p.outputMu.Lock()
			output := p.output
			if output != nil {
				writePTYOutput(output, buf[:n])
			}
			p.outputMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func writePTYOutput(output io.Writer, data []byte) {
	for len(data) > 0 {
		n, err := output.Write(data)
		if n > 0 {
			data = data[n:]
		}
		if err != nil || n <= 0 {
			return
		}
	}
}

func (p *persistentHostShell) setOutput(output io.Writer) {
	p.outputMu.Lock()
	p.output = output
	p.outputMu.Unlock()
}

func (p *persistentHostShell) clearOutputSoon() {
	time.Sleep(20 * time.Millisecond)
	p.setOutput(nil)
}

func (p *persistentHostShell) run(line string, stdout, stderr io.Writer, startForwarding func() (func(), error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	stopForwarding := func() {}
	if startForwarding != nil {
		stop, err := startForwarding()
		if err != nil {
			return err
		}
		if stop != nil {
			stopForwarding = stop
		}
	}
	p.setOutput(stdout)
	defer func() {
		p.clearOutputSoon()
		stopForwarding()
	}()
	if _, err := fmt.Fprintln(p.stdin, "__vmsh_run "+shellQuote(line)); err != nil {
		return err
	}
	for {
		record, err := p.readControlRecord()
		if err != nil {
			return err
		}
		if record.kind != "done" {
			continue
		}
		p.lastCWD = record.cwd
		if record.code != 0 {
			return persistentShellExit{code: record.code}
		}
		return nil
	}
}

const persistentDoneMarkerPrefix = "__VMSH_DONE__:"

func persistentMarkerPrefixSuffixLen(text string) int {
	max := len(persistentDoneMarkerPrefix) - 1
	if len(text) < max {
		max = len(text)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(persistentDoneMarkerPrefix, text[len(text)-n:]) {
			return n
		}
	}
	return 0
}

func (p *persistentHostShell) cwd() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCWD
}

func (s *shellState) closeSessions() {
	if s.guestShell != nil {
		s.guestShell.close()
		s.guestShell = nil
	}
	if s.hostShell != nil {
		s.hostShell.close()
		s.hostShell = nil
	}
	s.closeSSHClients()
}

func (s *shellState) closeGuestSession() {
	if s.guestShell == nil {
		return
	}
	if cwd := s.guestShell.cwd(); cwd != "" && s.context.Mode == modeVM {
		s.context.CWD = cwd
		s.rememberContextCWD(s.context)
	}
	s.guestShell.close()
	s.guestShell = nil
}

func (p *persistentHostShell) close() {
	if p.tty != nil {
		_ = p.tty.Close()
	} else if p.stdin != nil {
		_ = p.stdin.Close()
	}
	if p.controlFile != nil {
		_ = p.controlFile.Close()
	}
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	}
}

func (s *shellState) hostCommandPrelude(tty bool) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if !tty {
		return ""
	}
	s.hostInit.once.Do(func() {
		prelude, err := captureHostShellPrelude()
		s.hostInit.prelude, s.hostInit.fallback = hostCommandPreludeFromCapture(prelude, err)
	})
	return s.hostInit.prelude
}

func hostCommandPreludeFromCapture(prelude string, err error) (string, bool) {
	if err != nil || strings.TrimSpace(prelude) == "" || len(prelude) > maxEmbeddedHostInitPreludeBytes {
		return hostShellHookPrelude(), true
	}
	return prelude, false
}

func hostShellCommand(line string, tty bool, prelude string) []string {
	if runtime.GOOS == "windows" {
		return []string{hostShell(), "/D", "/S", "/C", line}
	}
	command := line
	if tty {
		command = prelude + hostShellPrelude() + "eval " + shellQuote(line)
	}
	return []string{hostShell(), "-lc", command}
}

func captureHostShellPrelude() (string, error) {
	const begin = "__VMSH_HOST_INIT_BEGIN__"
	const end = "__VMSH_HOST_INIT_END__"
	var script string
	switch filepath.Base(hostShell()) {
	case "bash":
		script = "printf '%s\\n' " + shellQuote(begin) + "; alias -p; declare -pf; printf '%s\\n' " + shellQuote(end)
	case "zsh":
		script = "print -r -- " + shellQuote(begin) + "; alias -L; functions; print -r -- " + shellQuote(end)
	default:
		return "", fmt.Errorf("host shell init snapshot is unsupported for %s", hostShell())
	}
	cmd := exec.Command(hostShell(), "-ic", script)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	text := string(out)
	start := strings.Index(text, begin)
	stop := strings.LastIndex(text, end)
	if start < 0 || stop < 0 || stop < start {
		return "", fmt.Errorf("host shell init markers not found")
	}
	body := strings.TrimSpace(text[start+len(begin) : stop])
	if body == "" {
		return "", nil
	}
	if filepath.Base(hostShell()) == "bash" {
		return bashHostShellOptionsPrelude() + "\n" + body + "\n", nil
	}
	return body + "\n", nil
}

func hostShellHookPrelude() string {
	switch filepath.Base(hostShell()) {
	case "bash":
		return bashHostShellOptionsPrelude() + "\nif [ -r \"$HOME/.bashrc\" ]; then . \"$HOME/.bashrc\"; fi\n"
	case "zsh":
		return "if [ -r \"${ZDOTDIR:-$HOME}/.zshrc\" ]; then . \"${ZDOTDIR:-$HOME}/.zshrc\"; fi\n"
	default:
		return ""
	}
}

func hostShellPrelude() string {
	switch filepath.Base(hostShell()) {
	case "bash":
		return colorPrelude("ls -G", "ls --color=auto", true)
	case "zsh":
		return colorPrelude("ls -G", "ls --color=auto", false)
	default:
		return colorPrelude("ls --color=auto", "ls -G", false)
	}
}

func colorPrelude(primaryLS, fallbackLS string, bash bool) string {
	var b strings.Builder
	if bash {
		b.WriteString(bashHostShellOptionsPrelude())
		b.WriteByte('\n')
	}
	b.WriteString("alias ls >/dev/null 2>&1 || { ")
	b.WriteString(shellQuoteCommandProbe(primaryLS))
	b.WriteString(" && alias ls=")
	b.WriteString(shellQuote(primaryLS))
	b.WriteString("; } || { ")
	b.WriteString(shellQuoteCommandProbe(fallbackLS))
	b.WriteString(" && alias ls=")
	b.WriteString(shellQuote(fallbackLS))
	b.WriteString("; } || true\n")
	return b.String()
}

func bashHostShellOptionsPrelude() string {
	return "shopt -s expand_aliases extglob 2>/dev/null || true"
}

func shellQuoteCommandProbe(command string) string {
	return command + " >/dev/null 2>&1"
}

func mergedEnv(base, overrides []string) []string {
	out := append([]string(nil), base...)
	index := make(map[string]int, len(out))
	for i, entry := range out {
		if key, _, ok := strings.Cut(entry, "="); ok {
			index[key] = i
		}
	}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if i, exists := index[key]; exists {
			out[i] = entry
			continue
		}
		index[key] = len(out)
		out = append(out, entry)
	}
	return out
}

func shellEnv(vars map[string]string) []string {
	if len(vars) == 0 {
		return nil
	}
	keys := sortedMapKeys(vars)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+vars[key])
	}
	return env
}

func hostCommandEnv(vars map[string]string, terminal []string) []string {
	return mergedEnv(mergedEnv(os.Environ(), terminal), shellEnv(vars))
}

func guestCommandEnv(ctx commandContext, vars map[string]string, terminal []string) []string {
	return mergedEnv(mergedEnv(guestShellEnv(ctx), terminal), shellEnv(vars))
}

func guestShellEnv(ctx commandContext) []string {
	user := strings.TrimSpace(guestRunUser(ctx))
	switch {
	case user == "root" || strings.HasPrefix(user, "0:"):
		return []string{"HOME=/root", "USER=root", "LOGNAME=root"}
	case user == defaultGuestUser || user == strconv.Itoa(defaultGuestUID) || strings.HasPrefix(user, strconv.Itoa(defaultGuestUID)+":"):
		name := defaultGuestUserName(ctx)
		return []string{"HOME=" + guestHomeDir(ctx), "USER=" + name, "LOGNAME=" + name}
	case user != "" && !strings.ContainsAny(user, ":0123456789"):
		return []string{"HOME=/home/" + user, "USER=" + user, "LOGNAME=" + user}
	default:
		return nil
	}
}

func guestHomeDir(ctx commandContext) string {
	user := strings.TrimSpace(guestRunUser(ctx))
	switch {
	case user == "root" || strings.HasPrefix(user, "0:"):
		return "/root"
	case user == defaultGuestUser || user == strconv.Itoa(defaultGuestUID) || strings.HasPrefix(user, strconv.Itoa(defaultGuestUID)+":"):
		if home := defaultGuestUserHome(ctx); home != "" {
			return home
		}
		return "/home/" + defaultGuestUserName(ctx)
	case user != "" && !strings.ContainsAny(user, ":0123456789"):
		return "/home/" + user
	default:
		return "/"
	}
}

func defaultGuestUserName(ctx commandContext) string {
	image := strings.ToLower(strings.TrimSpace(ctx.Image))
	if image == "ubuntu" || strings.HasPrefix(image, "ubuntu:") || strings.HasSuffix(image, "/ubuntu") || strings.Contains(image, "/ubuntu:") {
		return "ubuntu"
	}
	if current, err := user.Current(); err == nil && strings.TrimSpace(current.Username) != "" {
		name := filepath.Base(filepath.ToSlash(current.Username))
		if name != "." && name != "/" && strings.TrimSpace(name) != "" {
			return name
		}
	}
	return "cc"
}

func defaultGuestUserHome(ctx commandContext) string {
	image := strings.ToLower(strings.TrimSpace(ctx.Image))
	if image == "ubuntu" || strings.HasPrefix(image, "ubuntu:") || strings.HasSuffix(image, "/ubuntu") || strings.Contains(image, "/ubuntu:") {
		return "/home/ubuntu"
	}
	if current, err := user.Current(); err == nil && strings.HasPrefix(current.HomeDir, "/") {
		return path.Clean(current.HomeDir)
	}
	return ""
}

func (s *shellState) runGuest(ctx commandContext, line string, stdout, stderr io.Writer) error {
	tty, cols, rows := terminalRequestSize(stdout)
	req, err := s.prepareGuestRunRequest(ctx, line, tty, cols, rows, stderr)
	if err != nil {
		return err
	}
	if tty && persistentGuestCommandAllowed(line) {
		session, err := s.guestPersistentShell(ctx, req)
		if err != nil {
			s.lastCode = 1
			return err
		}
		var interrupted atomic.Bool
		interrupts := newCommandInterruptEscalator(line, stderr, nil, func() {
			if s.guestShell == session {
				s.guestShell = nil
			}
			go session.close()
		})
		err = session.run(line, stdout, stderr, func() (func(), error) {
			return s.startGuestInputForwarding(req.TTY, true, session.inputs, stdout, stderr, func(name string) {
				if name == "INT" {
					interrupted.Store(true)
					interrupts.Interrupt()
				}
			})
		})
		if interrupted.Load() && persistentGuestShellEnded(err) {
			s.guestShell = nil
			err = persistentShellExit{code: 130}
		}
		s.lastCode = sessionLastCode(err)
		if session.cwd() != "" {
			s.context.CWD = session.cwd()
			s.rememberContextCWD(s.context)
		}
		if err == nil || s.lastCode >= 0 {
			return nil
		}
		return err
	}
	return s.streamGuestRun(backendVMID(ctx), req, stdout, stderr)
}

func (s *shellState) prepareGuestRunRequest(ctx commandContext, line string, tty bool, cols, rows int, stderr io.Writer) (client.RunRequest, error) {
	if ctx.Image == "" {
		return client.RunRequest{}, fmt.Errorf("no guest image selected; run @<oci-tag> or set one with @<oci-tag>")
	}
	if strings.TrimSpace(ctx.Kernel) == "" {
		ctx.Kernel = defaultKernelForImage(ctx.Image)
	}
	if err := s.ensureImageAvailable(ctx, stderr); err != nil {
		return client.RunRequest{}, err
	}
	if err := s.ensureVMRunning(ctx, stderr); err != nil {
		return client.RunRequest{}, err
	}
	workDir := ctx.CWD
	req := client.RunRequest{
		Image:      localImageName(ctx.Image, ctx.Arch),
		InitSystem: ctx.InitSystem,
		Kernel:     ctx.Kernel,
		Command:    guestCommand(line, tty),
		WorkDir:    workDir,
		User:       guestRunUser(ctx),
		MemoryMB:   ctx.MemoryMB,
		CPUs:       ctx.CPUs,
		NestedVirt: ctx.NestedVirt,
	}
	if ctx.Isolated {
		req.WorkDir = firstNonEmpty(req.WorkDir, guestHomeDir(ctx))
	} else {
		hostRoot, hostGuestCWD, err := guestHostPaths(s.hostCWD)
		if err != nil {
			return client.RunRequest{}, err
		}
		req.WorkDir = firstNonEmpty(req.WorkDir, hostGuestCWD)
		req.Shares = []client.ShareMount{{
			Source:   hostRoot,
			Mount:    guestHostMount,
			Writable: true,
			MapOwner: true,
			OwnerUID: defaultGuestUID,
			OwnerGID: defaultGuestGID,
			Cache:    "strict",
		}}
	}
	if tty {
		req.TTY = true
		req.Cols = cols
		req.Rows = rows
	}
	terminal := []string(nil)
	if tty {
		terminal = terminalEnv(cols, rows)
	}
	req.Env = guestCommandEnv(ctx, s.env, terminal)
	if ctx.Network {
		req.Network = networkConfigForContext(ctx)
	}
	return req, nil
}

func persistentGuestCommandAllowed(line string) bool {
	return persistentShellCommandAllowed(line)
}

func (s *shellState) guestPersistentShell(ctx commandContext, req client.RunRequest) (*persistentGuestShell, error) {
	key := guestPersistentShellKey(ctx, req)
	if s.guestShell != nil && s.guestShell.key == key {
		return s.guestShell, nil
	}
	if s.guestShell != nil {
		s.guestShell.close()
		s.guestShell = nil
	}
	req.Command = guestPersistentCommand()
	req.TTY = true
	req.ControlFD = true
	if req.Cols == 0 {
		req.Cols = 80
	}
	if req.Rows == 0 {
		req.Rows = 24
	}
	inputs := make(chan client.ExecInput, 8)
	events := make(chan client.ExecEvent, 32)
	done := make(chan error, 1)
	session := &persistentGuestShell{
		key:     key,
		inputs:  inputs,
		events:  events,
		done:    done,
		lastCWD: req.WorkDir,
	}
	go func() {
		err := s.api.RunInteractiveStreamIn(backendVMID(ctx), req, inputs, func(event client.ExecEvent) error {
			events <- event
			return nil
		})
		close(events)
		done <- err
	}()
	if err := session.waitReady(); err != nil {
		session.close()
		return nil, err
	}
	s.guestShell = session
	return session, nil
}

func guestPersistentShellKey(ctx commandContext, req client.RunRequest) string {
	return strings.Join([]string{
		backendVMID(ctx),
		req.Image,
		req.User,
		shareMountKey(req.Shares),
	}, "\x00")
}

func shareMountKey(shares []client.ShareMount) string {
	if len(shares) == 0 {
		return ""
	}
	parts := make([]string, 0, len(shares))
	for _, share := range shares {
		parts = append(parts, strings.Join([]string{
			share.Source,
			share.Mount,
			strconv.FormatBool(share.Writable),
			strconv.FormatBool(share.MapOwner),
			strconv.FormatUint(uint64(share.OwnerUID), 10),
			strconv.FormatUint(uint64(share.OwnerGID), 10),
			share.Cache,
		}, "\x1f"))
	}
	return strings.Join(parts, "\x1e")
}

func guestPersistentCommand() []string {
	return []string{"sh", "-lc", guestShellPrelude() + strings.Join([]string{
		"stty -echo 2>/dev/null || true",
		colorPrelude("ls --color=always -C -w ${COLUMNS:-80}", "ls -G -C", false),
		"__vmsh_control_fd=3",
		"__vmsh_report() {",
		"  printf '%s\\t%s\\t%s\\n' \"$1\" \"$2\" \"$PWD\" >&$__vmsh_control_fd",
		"}",
		"__vmsh_run() {",
		"  stty echo 2>/dev/null || true",
		"  eval \"$1\"",
		"  __vmsh_status=$?",
		"  stty -echo 2>/dev/null || true",
		"  __vmsh_report done \"$__vmsh_status\"",
		"}",
		"__vmsh_report ready 0",
		"while IFS= read -r __vmsh_line; do eval \"$__vmsh_line\"; done",
	}, "\n")}
}

func (p *persistentGuestShell) waitReady() error {
	timer := time.NewTimer(defaultGuestShellReadyTimeout)
	defer timer.Stop()
	var startup strings.Builder
	for {
		select {
		case event, ok := <-p.events:
			if !ok {
				if msg := persistentStartupMessage(startup.String()); msg != "" {
					return fmt.Errorf("persistent guest shell closed before ready: %s", msg)
				}
				return fmt.Errorf("persistent guest shell closed before ready")
			}
			switch event.Kind {
			case "control":
				record, err := parsePersistentControlRecord(execEventText(event))
				if err != nil {
					appendPersistentStartupOutput(&startup, err.Error())
					continue
				}
				if record.kind == "ready" {
					p.lastCWD = record.cwd
					return nil
				}
			case "stdout", "output":
				text := execEventText(event)
				appendPersistentStartupOutput(&startup, text)
			case "stderr":
				appendPersistentStartupOutput(&startup, execEventText(event))
			case "exit":
				if msg := persistentStartupMessage(startup.String()); msg != "" {
					return fmt.Errorf("persistent guest shell exited before ready: %s", msg)
				}
				return fmt.Errorf("persistent guest shell exited before ready")
			case "error":
				if event.Error != "" {
					return fmt.Errorf("%s", event.Error)
				}
				if msg := persistentStartupMessage(startup.String()); msg != "" {
					return fmt.Errorf("persistent guest shell failed before ready: %s", msg)
				}
				return fmt.Errorf("persistent guest shell failed before ready")
			}
		case err := <-p.done:
			if err != nil {
				return err
			}
			if msg := persistentStartupMessage(startup.String()); msg != "" {
				return fmt.Errorf("persistent guest shell exited before ready: %s", msg)
			}
			return fmt.Errorf("persistent guest shell exited before ready")
		case <-timer.C:
			if msg := persistentStartupMessage(startup.String()); msg != "" {
				return fmt.Errorf("persistent guest shell did not become ready: %s", msg)
			}
			return fmt.Errorf("persistent guest shell did not become ready")
		}
	}
}

func appendPersistentStartupOutput(dst *strings.Builder, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if dst.Len() > 0 {
		dst.WriteByte('\n')
	}
	dst.WriteString(text)
}

func persistentStartupMessage(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	const max = 1000
	if len(text) <= max {
		return text
	}
	const head = 600
	const tail = 400
	return text[:head] + "\n...\n" + text[len(text)-tail:]
}

func parsePersistentReady(text string) (string, bool) {
	idx := strings.Index(text, "__VMSH_READY__:")
	if idx < 0 {
		return "", false
	}
	rest := text[idx+len("__VMSH_READY__:"):]
	if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return rest, true
}

func (p *persistentGuestShell) run(line string, stdout, stderr io.Writer, startForwarding func() (func(), error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inputs <- client.ExecInput{Kind: "stdin", Data: []byte("__vmsh_run " + shellQuote(line) + "\n")}
	stopForwarding := func() {}
	if startForwarding != nil {
		stop, err := startForwarding()
		if err != nil {
			return err
		}
		if stop != nil {
			stopForwarding = stop
		}
	}
	defer stopForwarding()
	for event := range p.events {
		switch event.Kind {
		case "control":
			record, err := parsePersistentControlRecord(execEventText(event))
			if err != nil {
				return err
			}
			if record.kind != "done" {
				continue
			}
			p.lastCWD = record.cwd
			if record.code != 0 {
				return persistentShellExit{code: record.code}
			}
			return nil
		case "stdout", "output":
			writeExecEventOutput(stdout, event)
		case "stderr":
			writeExecEventOutput(stderr, event)
		case "exit":
			return errPersistentGuestShellExited
		case "error":
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return fmt.Errorf("persistent guest shell failed")
		}
	}
	return errPersistentGuestShellClosed
}

func persistentGuestShellEnded(err error) bool {
	return errors.Is(err, errPersistentGuestShellExited) || errors.Is(err, errPersistentGuestShellClosed)
}

func parsePersistentMarker(line string) (int, string, bool) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "__VMSH_DONE__:") {
		return 0, "", false
	}
	rest := strings.TrimPrefix(line, "__VMSH_DONE__:")
	codeText, cwd, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, "", false
	}
	code, err := strconv.Atoi(codeText)
	if err != nil {
		return 0, "", false
	}
	return code, cwd, true
}

func execEventText(event client.ExecEvent) string {
	if len(event.Data) > 0 {
		return string(event.Data)
	}
	return event.Output
}

func execEventBytes(event client.ExecEvent) []byte {
	if len(event.Data) > 0 {
		return event.Data
	}
	return []byte(event.Output)
}

func (p *persistentGuestShell) cwd() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastCWD
}

func (p *persistentGuestShell) close() {
	select {
	case p.inputs <- client.ExecInput{Kind: "stdin_close"}:
	default:
	}
	close(p.inputs)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

func (s *shellState) streamGuestRun(id string, req client.RunRequest, stdout, stderr io.Writer) error {
	if !req.TTY {
		exitCode := 0
		runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
		defer stopInterrupts()
		if err := s.api.RunStreamInContext(runCtx, id, req, func(event client.ExecEvent) error {
			switch event.Kind {
			case "stdout", "output":
				writeExecEventOutput(stdout, event)
			case "stderr":
				writeExecEventOutput(stderr, event)
			case "exit":
				exitCode = event.ExitCode
			case "error":
				if event.Error != "" {
					return fmt.Errorf("%s", event.Error)
				}
				return fmt.Errorf("guest command failed")
			}
			return nil
		}); err != nil {
			if interrupted.Load() {
				s.lastCode = 130
				return nil
			}
			s.lastCode = 1
			return err
		}
		if interrupted.Load() {
			s.lastCode = 130
			return nil
		}
		s.lastCode = exitCode
		return nil
	}

	inputs := make(chan client.ExecInput, 8)
	var interrupted atomic.Bool
	runCtx, cancel := context.WithCancel(context.Background())
	interrupts := newCommandInterruptEscalator(commandDisplay(req.Command), stderr, nil, cancel)
	stopInterrupts, contextInterrupted := s.startInterruptWatcher(func() {
		interrupted.Store(true)
		interrupts.Interrupt()
	})
	defer func() {
		stopInterrupts()
		cancel()
	}()
	stopForwarding, err := s.startGuestInputForwarding(req.TTY, true, inputs, stdout, stderr, func(name string) {
		if name == "INT" {
			interrupted.Store(true)
			interrupts.Interrupt()
		}
	})
	if err != nil {
		return err
	}
	defer func() {
		stopForwarding()
		close(inputs)
	}()

	exitCode := 0
	if err := s.api.RunInteractiveStreamInContext(runCtx, id, req, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			writeExecEventOutput(stdout, event)
		case "stderr":
			writeExecEventOutput(stderr, event)
		case "exit":
			exitCode = event.ExitCode
		case "error":
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return fmt.Errorf("guest command failed")
		}
		return nil
	}); err != nil {
		if interrupted.Load() || contextInterrupted.Load() {
			s.lastCode = 130
			return nil
		}
		s.lastCode = 1
		return err
	}
	if interrupted.Load() || contextInterrupted.Load() {
		s.lastCode = 130
		return nil
	}
	s.lastCode = exitCode
	return nil
}

func (s *shellState) execStreamInContext(ctx context.Context, id string, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	if api, ok := s.api.(execStreamContextAPI); ok {
		return api.ExecStreamInContext(ctx, id, req, inputs, onEvent)
	}
	return s.api.ExecStreamIn(id, req, inputs, onEvent)
}

func (s *shellState) streamGuestRunWithInput(id string, req client.RunRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	req.TTY = false
	req.Cols = 0
	req.Rows = 0
	runStream := func(req client.RunRequest) error {
		exitCode := 0
		runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
		defer stopInterrupts()
		if err := s.api.RunStreamInContext(runCtx, id, req, func(event client.ExecEvent) error {
			switch event.Kind {
			case "stdout", "output":
				writeExecEventOutput(stdout, event)
			case "stderr":
				writeExecEventOutput(stderr, event)
			case "exit":
				exitCode = event.ExitCode
			case "error":
				if event.Error != "" {
					return fmt.Errorf("%s", event.Error)
				}
				return fmt.Errorf("guest command failed")
			}
			return nil
		}); err != nil {
			if interrupted.Load() {
				return persistentShellExit{code: 130}
			}
			return err
		}
		if interrupted.Load() {
			return persistentShellExit{code: 130}
		}
		if exitCode != 0 {
			return persistentShellExit{code: exitCode}
		}
		return nil
	}
	if stdin == nil {
		return runStream(req)
	}

	inputs := make(chan client.ExecInput, 8)
	inputErr := make(chan error, 1)
	done := make(chan struct{})
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	var closeDoneOnce sync.Once
	closeDone := func() {
		closeDoneOnce.Do(func() {
			close(done)
		})
	}
	go func() {
		inputErr <- streamReaderToGuestInput(stdin, inputs, done)
		close(inputs)
	}()
	go func() {
		<-runCtx.Done()
		closeDone()
	}()
	exitCode := 0
	err := s.api.RunInteractiveStreamInContext(runCtx, id, req, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			writeExecEventOutput(stdout, event)
		case "stderr":
			writeExecEventOutput(stderr, event)
		case "exit":
			exitCode = event.ExitCode
		case "error":
			if event.Error != "" {
				return fmt.Errorf("%s", event.Error)
			}
			return fmt.Errorf("guest command failed")
		}
		return nil
	})
	closeDone()
	if inErr := <-inputErr; err == nil && inErr != nil {
		err = inErr
	}
	if interrupted.Load() {
		return persistentShellExit{code: 130}
	}
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return persistentShellExit{code: exitCode}
	}
	return nil
}

func streamReaderToGuestInput(r io.Reader, out chan<- client.ExecInput, done <-chan struct{}) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if !sendGuestInputBlocking(out, done, client.ExecInput{Kind: "stdin", Data: append([]byte(nil), buf[:n]...)}) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func sendGuestInputBlocking(out chan<- client.ExecInput, done <-chan struct{}, input client.ExecInput) bool {
	select {
	case <-done:
		return false
	case out <- input:
		return true
	}
}

func (s *shellState) startGuestInputForwarding(tty, forwardStdin bool, inputs chan<- client.ExecInput, stdout, stderr io.Writer, onSignal ...func(string)) (func(), error) {
	restore := func() {}
	done := make(chan struct{})
	cancelRead := func() {}
	var producers sync.WaitGroup
	if tty && forwardStdin {
		file, ok := terminalWriterFile(stdout)
		if ok && terminal.IsTerminalFD(int(file.Fd())) && terminal.IsTerminalFD(int(os.Stdin.Fd())) {
			terminalRestore, err := terminal.MakeAttachedRaw(os.Stdin)
			if err != nil {
				return nil, err
			}
			inputCancel, err := newPTYInputCanceller(os.Stdin)
			if err != nil {
				terminalRestore()
				return nil, err
			}
			restore = terminalRestore
			cancelRead = inputCancel.cancel
			producers.Add(1)
			go func() {
				defer producers.Done()
				defer inputCancel.close()
				streamGuestStdin(os.Stdin, inputs, done, inputCancel, terminalWriterRecorder(stdout), onSignal...)
			}()
		}
	}

	producers.Add(1)
	go func() {
		defer producers.Done()
		forwardGuestSignals(inputs, done, tty, stdout, stderr, onSignal...)
	}()
	return stopGuestInputForwarding(restore, func() {
		close(done)
		cancelRead()
		producers.Wait()
	}), nil
}

func stopGuestInputForwarding(restore func(), stopProducers func()) func() {
	return func() {
		if stopProducers != nil {
			stopProducers()
		}
		if restore != nil {
			restore()
		}
	}
}

func streamGuestStdin(file *os.File, out chan<- client.ExecInput, done <-chan struct{}, inputCancel *ptyInputCanceller, recorder *asciinemaRecorder, onSignal ...func(string)) {
	var buf [4096]byte
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := readPTYInput(file, buf[:], done, inputCancel)
		if n > 0 {
			if recorder != nil {
				recorder.recordInput(buf[:n])
			}
			if bytes.Contains(buf[:n], []byte{0x03}) {
				for _, fn := range onSignal {
					if fn != nil {
						fn("INT")
					}
				}
			}
			sendGuestInput(out, done, client.ExecInput{Kind: "stdin", Data: append([]byte(nil), buf[:n]...)})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				sendGuestInput(out, done, client.ExecInput{Kind: "stdin_close"})
			}
			return
		}
	}
}

func forwardGuestSignals(out chan<- client.ExecInput, done <-chan struct{}, tty bool, stdout, stderr io.Writer, onSignal ...func(string)) {
	signals := terminal.HostSignals(tty)
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, signals...)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-done:
			return
		case sig := <-sigCh:
			if sig == nil {
				continue
			}
			if terminal.IsResizeSignal(sig) {
				file, ok := terminalWriterFile(stdout)
				if !ok {
					continue
				}
				cols, rows, err := terminal.Size(file)
				if err != nil {
					continue
				}
				sendGuestInput(out, done, client.ExecInput{Kind: "resize", Cols: cols, Rows: rows})
				continue
			}
			name, ok := terminal.SignalName(sig)
			if !ok {
				continue
			}
			for _, fn := range onSignal {
				if fn != nil {
					fn(name)
				}
			}
			if name == "INT" {
				fmt.Fprintln(stderr)
			}
			sendGuestInput(out, done, client.ExecInput{Kind: "signal", Signal: name})
		}
	}
}

func sendGuestInput(out chan<- client.ExecInput, done <-chan struct{}, input client.ExecInput) {
	select {
	case <-done:
	case out <- input:
	}
}

func (s *shellState) startHostPTYForwarding(tty bool, session *persistentHostShell, stdout, stderr io.Writer, onInterrupt ...func()) (func(), *atomic.Bool, error) {
	interrupted := &atomic.Bool{}
	if session == nil || session.tty == nil {
		return func() {}, interrupted, nil
	}

	done := make(chan struct{})
	restore := func() {}
	cancelRead := func() {}
	var producers sync.WaitGroup
	if tty {
		file, ok := terminalWriterFile(stdout)
		if ok && terminal.IsTerminalFD(int(file.Fd())) && terminal.IsTerminalFD(int(os.Stdin.Fd())) {
			terminalRestore, err := terminal.MakeAttachedRaw(os.Stdin)
			if err != nil {
				return nil, interrupted, err
			}
			inputCancel, err := newPTYInputCanceller(os.Stdin)
			if err != nil {
				terminalRestore()
				return nil, interrupted, err
			}
			restore = terminalRestore
			cancelRead = inputCancel.cancel
			producers.Add(1)
			go func() {
				defer producers.Done()
				defer inputCancel.close()
				streamHostPTYStdin(os.Stdin, session.tty, done, inputCancel, interrupted, terminalWriterRecorder(stdout), onInterrupt...)
			}()
		}
	}

	producers.Add(1)
	go func() {
		defer producers.Done()
		forwardHostPTYSignals(session.tty, done, tty, stdout, stderr, interrupted, onInterrupt...)
	}()

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			cancelRead()
			producers.Wait()
			restore()
		})
	}
	return stop, interrupted, nil
}

func streamHostPTYStdin(in *os.File, out *os.File, done <-chan struct{}, inputCancel *ptyInputCanceller, interrupted *atomic.Bool, recorder *asciinemaRecorder, onInterrupt ...func()) {
	var buf [4096]byte
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := readPTYInput(in, buf[:], done, inputCancel)
		if n > 0 {
			if recorder != nil {
				recorder.recordInput(buf[:n])
			}
			if bytes.Contains(buf[:n], []byte{0x03}) {
				interrupted.Store(true)
				for _, fn := range onInterrupt {
					if fn != nil {
						fn()
					}
				}
			}
			if !writeHostPTYInput(out, done, buf[:n]) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func writeHostPTYInput(out *os.File, done <-chan struct{}, data []byte) bool {
	for len(data) > 0 {
		select {
		case <-done:
			return false
		default:
		}
		n, err := out.Write(data)
		if err != nil {
			return false
		}
		if n <= 0 {
			return false
		}
		data = data[n:]
	}
	return true
}

func forwardHostPTYSignals(out *os.File, done <-chan struct{}, tty bool, stdout, stderr io.Writer, interrupted *atomic.Bool, onInterrupt ...func()) {
	signals := terminal.HostSignals(tty)
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, signals...)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-done:
			return
		case sig := <-sigCh:
			if sig == nil {
				continue
			}
			if terminal.IsResizeSignal(sig) {
				resizeHostPTY(out, stdout)
				continue
			}
			name, ok := terminal.SignalName(sig)
			if !ok {
				continue
			}
			if name == "INT" {
				interrupted.Store(true)
				fmt.Fprintln(stderr)
				for _, fn := range onInterrupt {
					if fn != nil {
						fn()
					}
				}
			}
			writeHostPTYSignal(out, name)
		}
	}
}

func resizeHostPTY(out *os.File, stdout io.Writer) {
	file, ok := terminalWriterFile(stdout)
	if !ok {
		return
	}
	cols, rows, err := terminal.Size(file)
	if err != nil || cols <= 0 || rows <= 0 {
		return
	}
	_ = pty.Setsize(out, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func writeHostPTYSignal(out *os.File, name string) {
	switch name {
	case "INT":
		_, _ = out.Write([]byte{0x03})
	case "QUIT":
		_, _ = out.Write([]byte{0x1c})
	}
}

type commandInterruptEscalator struct {
	command string
	stderr  io.Writer
	soft    func()
	hard    func()
	count   atomic.Int32
}

func newCommandInterruptEscalator(command string, stderr io.Writer, soft, hard func()) *commandInterruptEscalator {
	return &commandInterruptEscalator{
		command: command,
		stderr:  stderr,
		soft:    soft,
		hard:    hard,
	}
}

func (e *commandInterruptEscalator) Interrupt() {
	if e == nil {
		return
	}
	count := e.count.Add(1)
	e.handleCount(count, true)
}

func (e *commandInterruptEscalator) ForwardedInterrupt() {
	if e == nil {
		return
	}
	count := e.count.Add(1)
	e.handleCount(count, false)
}

func (e *commandInterruptEscalator) handleCount(count int32, runSoft bool) {
	switch count {
	case 1:
		if runSoft && e.soft != nil {
			e.soft()
		}
	case 2:
		fmt.Fprintf(e.stderr, "\nvmsh: command %q is not responding to SIGINT; press Ctrl+C again to hard terminate it\n", compactCommandForMessage(e.command))
		if runSoft && e.soft != nil {
			e.soft()
		}
	default:
		if e.hard != nil {
			e.hard()
		}
	}
}

func compactCommandForMessage(command string) string {
	command = compactStatusMessage(command)
	const max = 120
	if len(command) <= max {
		return command
	}
	return command[:max-3] + "..."
}

func commandDisplay(command []string) string {
	if len(command) == 0 {
		return ""
	}
	parts := make([]string, 0, len(command))
	for _, part := range command {
		parts = append(parts, shellQuote(part))
	}
	return strings.Join(parts, " ")
}

func (s *shellState) interruptibleCommandContext() (context.Context, func(), *atomic.Bool) {
	ctx, cancel := context.WithCancel(context.Background())
	stop, interrupted := s.startInterruptWatcher(cancel)
	return ctx, func() {
		stop()
		cancel()
	}, interrupted
}

func (s *shellState) startInterruptWatcher(onInterrupt func()) (func(), *atomic.Bool) {
	interrupted := &atomic.Bool{}
	signals := s.interruptSignals
	drainInterruptSignals(signals)
	if signals == nil {
		return func() {}, interrupted
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-signals:
				if !isInterruptSignal(sig) {
					continue
				}
				interrupted.Store(true)
				if onInterrupt != nil {
					onInterrupt()
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(done)
		})
	}, interrupted
}

func drainInterruptSignals(signals <-chan os.Signal) {
	for {
		select {
		case <-signals:
		default:
			return
		}
	}
}

func isInterruptSignal(sig os.Signal) bool {
	if sig == nil {
		return false
	}
	if sig == os.Interrupt {
		return true
	}
	name, ok := terminal.SignalName(sig)
	return ok && name == "INT"
}

func sleepOrDone(done <-chan struct{}, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func writeExecEventOutput(w io.Writer, event client.ExecEvent) {
	if len(event.Data) > 0 {
		_, _ = w.Write(event.Data)
		return
	}
	if event.Output != "" {
		_, _ = fmt.Fprint(w, event.Output)
	}
}

func guestCommand(line string, tty bool) []string {
	prelude := guestShellPrelude()
	if !tty {
		return []string{"sh", "-lc", prelude + line}
	}
	return []string{"sh", "-lc", prelude + colorPrelude("ls --color=always -C -w ${COLUMNS:-80}", "ls -G -C", false) + line}
}

func guestShellPrelude() string {
	return `__vmsh_uid="$(id -u 2>/dev/null || printf '')"
__vmsh_passwd="$(awk -F: -v u="$__vmsh_uid" '$3 == u { print $1 ":" $6; exit }' /etc/passwd 2>/dev/null || true)"
if [ -n "$__vmsh_passwd" ]; then
  USER="${__vmsh_passwd%%:*}"
  LOGNAME="$USER"
  HOME="${__vmsh_passwd#*:}"
  export USER LOGNAME HOME
fi
unset __vmsh_uid __vmsh_passwd
`
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func promptPullConfirmation(in *os.File, stderr io.Writer, source string) (bool, error) {
	if in == nil || !terminal.IsTerminalFD(int(in.Fd())) {
		return false, nil
	}
	fmt.Fprintf(stderr, "do you want to pull %s (y/n) [n]: ", source)
	reader := bufio.NewReader(in)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func promptVMRestartConfirmation(in *os.File, stderr io.Writer, id string) (bool, error) {
	if in == nil || !terminal.IsTerminalFD(int(in.Fd())) {
		return false, nil
	}
	fmt.Fprintf(stderr, "restart VM %s (y/n) [n]: ", emptyText(id, "default"))
	reader := bufio.NewReader(in)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func promptExitConfirmation(in *os.File, stderr io.Writer, resources []exitResource) (bool, error) {
	if in == nil || !terminal.IsTerminalFD(int(in.Fd())) {
		return false, nil
	}
	fmt.Fprintln(stderr, "vmsh still has active resources:")
	for _, resource := range resources {
		line := fmt.Sprintf("  - %s %s", resource.Kind, resource.Name)
		if resource.Detail != "" {
			line += " (" + resource.Detail + ")"
		}
		fmt.Fprintln(stderr, line)
	}
	fmt.Fprint(stderr, "Exit anyway? (yes/no) [no]: ")
	reader := bufio.NewReader(in)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func promptSSHPassword(in *os.File, stderr io.Writer, cfg resolvedSSHConfig) (string, error) {
	if in == nil || !terminal.IsTerminalFD(int(in.Fd())) {
		return "", fmt.Errorf("ssh password auth requires an interactive terminal")
	}
	prompt := "SSH password"
	userHost := cfg.User
	if cfg.HostName != "" {
		if userHost != "" {
			userHost += "@"
		}
		userHost += cfg.HostName
	}
	if userHost != "" {
		prompt += " for " + userHost
	}
	fmt.Fprintf(stderr, "%s: ", prompt)
	password, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", err
	}
	return string(password), nil
}

func promptSSHKeyboardInteractive(in *os.File, stderr io.Writer, cfg resolvedSSHConfig, name, instruction string, questions []string, echos []bool) ([]string, error) {
	if in == nil || !terminal.IsTerminalFD(int(in.Fd())) {
		return nil, fmt.Errorf("ssh keyboard-interactive auth requires an interactive terminal")
	}
	if name = strings.TrimSpace(name); name != "" {
		fmt.Fprintln(stderr, name)
	}
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		fmt.Fprintln(stderr, instruction)
	}
	answers := make([]string, 0, len(questions))
	reader := bufio.NewReader(in)
	for i, question := range questions {
		if strings.TrimSpace(question) == "" {
			question = "SSH keyboard-interactive response: "
		}
		echo := false
		if i < len(echos) {
			echo = echos[i]
		}
		fmt.Fprint(stderr, question)
		if echo {
			answer, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			answers = append(answers, strings.TrimRight(answer, "\r\n"))
			continue
		}
		answer, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return nil, err
		}
		answers = append(answers, string(answer))
	}
	return answers, nil
}

func promptSSHHostKeyConfirmation(in *os.File, stderr io.Writer, cfg resolvedSSHConfig, hostname string, remote net.Addr, key ssh.PublicKey) (bool, error) {
	if in == nil || !terminal.IsTerminalFD(int(in.Fd())) {
		return false, fmt.Errorf("ssh host key for %s is unknown", hostname)
	}
	display := cfg.HostName
	if display == "" {
		display = hostname
	}
	if remote != nil && remote.String() != "" {
		display += " (" + remote.String() + ")"
	}
	fmt.Fprintf(stderr, "The authenticity of host %q can't be established.\n", display)
	fmt.Fprintf(stderr, "%s key fingerprint is %s.\n", key.Type(), ssh.FingerprintSHA256(key))
	fmt.Fprint(stderr, "Trust this host and add it to known_hosts? (yes/no) [no]: ")
	reader := bufio.NewReader(in)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "yes", nil
}

func displayPullSource(source string) string {
	if ubuntuCloudRootFSSource(source, "") != nil {
		return "Ubuntu 24.04 cloud rootfs"
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return source
	}
	lower := strings.ToLower(source)
	if strings.HasPrefix(lower, "cvmfs:") || strings.HasPrefix(lower, "docker-archive:") || strings.Contains(source, "://") {
		return source
	}
	name := source
	tag := ""
	if at := strings.Index(name, "@"); at >= 0 {
		return source
	}
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		tag = name[lastColon+1:]
		name = name[:lastColon]
	}
	parts := strings.Split(name, "/")
	first := parts[0]
	hasRegistry := strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost"
	if !hasRegistry {
		if len(parts) == 1 {
			name = "library/" + name
		}
		name = "docker.io/" + name
	}
	if tag == "" {
		tag = "latest"
	}
	return name + ":" + tag
}

func normalizeVMSHArchitecture(architecture string) string {
	switch strings.ToLower(strings.TrimSpace(architecture)) {
	case "", "native":
		return ""
	case "amd64", "x86_64", "x64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return ""
	}
}

func localImageName(image, architecture string) string {
	arch := normalizeVMSHArchitecture(architecture)
	if arch == "" {
		return image
	}
	return image + "@" + arch
}

func pullImageRequestForContext(ctx commandContext) client.PullImageRequest {
	if source := ubuntuCloudRootFSSource(ctx.Image, ctx.Arch); source != nil {
		return client.PullImageRequest{SourceRef: source, Architecture: ubuntuCloudRootFSArchitecture(ctx.Arch)}
	}
	return client.PullImageRequest{Source: ctx.Image, Architecture: ctx.Arch}
}

func ubuntuCloudRootFSSource(image, architecture string) *client.ImageSource {
	if strings.TrimSpace(image) != "ubuntu" {
		return nil
	}
	arch := ubuntuCloudRootFSArchitecture(architecture)
	return &client.ImageSource{
		Type: "rootfs-tar",
		Path: fmt.Sprintf("%s/ubuntu-24.04-server-cloudimg-%s-root.tar.xz", ubuntuCloudRootFSBaseURL, arch),
	}
}

func ubuntuCloudRootFSArchitecture(architecture string) string {
	if arch := normalizeVMSHArchitecture(architecture); arch != "" {
		return arch
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	default:
		return "amd64"
	}
}

func (s *shellState) ensureImageAvailable(ctx commandContext, stderr io.Writer) error {
	image := localImageName(ctx.Image, ctx.Arch)
	req := pullImageRequestForContext(ctx)
	requiredSource, requiredKind := requiredImageSource(req)
	requireExactSource := requiredSource != ""
	if !requireExactSource && s.imageCache != nil && s.imageCache[image] {
		return nil
	}
	if state, err := s.api.GetImage(image); err == nil && imageStateSatisfiesSource(state, requiredSource, requiredKind) {
		if s.imageCache == nil {
			s.imageCache = map[string]bool{}
		}
		s.imageCache[image] = true
		return nil
	}
	source := displayPullSource(ctx.Image)
	if ctx.Arch != "" {
		source += " (" + ctx.Arch + ")"
	}
	if s.confirmPull == nil {
		return fmt.Errorf("image %s is not locally cached", source)
	}
	ok, err := s.confirmPull(source, stderr)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("image pull cancelled for %s", source)
	}
	progress := newTerminalHoldStatus(stderr, "Pull "+image+": preparing")
	defer progress.Close()
	report := func(event client.ProgressEvent) error {
		message := formatDetailedProgressEvent(event, image)
		if message != "" {
			if event.Error != "" {
				progress.finishWith(message)
				return nil
			}
			progress.Update(message)
		}
		return nil
	}
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	var pullErr error
	if api, ok := s.api.(imagePullContextAPI); ok {
		pullErr = api.PullImageStreamContext(runCtx, image, req, report)
	} else {
		pullErr = s.api.PullImageStream(image, req, report)
	}
	if interrupted.Load() {
		return persistentShellExit{code: 130}
	}
	if pullErr != nil {
		return pullErr
	}
	if s.imageCache == nil {
		s.imageCache = map[string]bool{}
	}
	s.imageCache[image] = true
	return nil
}

func requiredImageSource(req client.PullImageRequest) (source, kind string) {
	if req.SourceRef == nil {
		return "", ""
	}
	source, err := req.SourceString()
	if err != nil {
		return "", ""
	}
	return source, strings.ToLower(strings.TrimSpace(req.SourceRef.Type))
}

func imageStateSatisfiesSource(state client.ImageState, source, kind string) bool {
	if source == "" {
		return true
	}
	if state.Source == "" && state.SourceKind == "" {
		return true
	}
	return state.Source == source && state.SourceKind == kind
}

func guestHostPaths(hostCWD string) (hostRoot, guestCWD string, err error) {
	abs, err := filepath.Abs(hostCWD)
	if err != nil {
		return "", "", err
	}
	volume := filepath.VolumeName(abs)
	if volume != "" {
		hostRoot = volume + string(filepath.Separator)
		rel, err := filepath.Rel(hostRoot, abs)
		if err != nil {
			return "", "", err
		}
		guestCWD = path.Join(guestHostMount, filepath.ToSlash(rel))
		return hostRoot, guestCWD, nil
	}
	hostRoot = string(filepath.Separator)
	rel := strings.TrimPrefix(filepath.ToSlash(abs), "/")
	guestCWD = path.Join(guestHostMount, rel)
	return hostRoot, guestCWD, nil
}

func (s *shellState) ensureVMRunning(ctx commandContext, stderr io.Writer) error {
	if strings.TrimSpace(ctx.Kernel) == "" {
		ctx.Kernel = defaultKernelForImage(ctx.Image)
	}
	id := backendVMID(ctx)
	if s.vmRunning != nil && s.vmRunning[id] && strings.TrimSpace(ctx.InitSystem) == "" && strings.TrimSpace(ctx.Kernel) == "" {
		return nil
	}
	state, err := s.api.InstanceStatusOf(id)
	if err != nil {
		return err
	}
	if state.Status == "running" {
		if err := validateRunningVMContext(id, ctx, state); err != nil {
			return err
		}
		if s.vmRunning == nil {
			s.vmRunning = map[string]bool{}
		}
		s.vmRunning[id] = true
		return nil
	}
	if err := s.validateVMNameNamespaceAvailable(ctx); err != nil {
		return err
	}
	return s.startVM(id, ctx, stderr)
}

func (s *shellState) validateVMNameNamespaceAvailable(ctx commandContext) error {
	name := normalizedVMID(ctx.VMID)
	otherID := backendVMIDFor(name, !ctx.Isolated)
	state, err := s.api.InstanceStatusOf(otherID)
	if err != nil {
		return err
	}
	if !instanceStateIsLive(state) {
		return nil
	}
	runningKind := "isolated"
	requestedKind := "shared"
	if ctx.Isolated {
		runningKind = "shared"
		requestedKind = "isolated"
	}
	return fmt.Errorf("VM name %q is already running as %s VM; stop it before starting it as %s VM", name, vmKindArticle(runningKind), vmKindArticle(requestedKind))
}

func vmKindArticle(kind string) string {
	if kind == "isolated" {
		return "an isolated"
	}
	return "a " + kind
}

func validateRunningVMContext(id string, ctx commandContext, state client.InstanceState) error {
	wantInit := strings.TrimSpace(ctx.InitSystem)
	gotInit := strings.TrimSpace(state.InitSystem)
	wantKernel := strings.TrimSpace(ctx.Kernel)
	gotKernel := strings.TrimSpace(state.Kernel)
	if kernelStateEqual(wantKernel, gotKernel) && wantInit == gotInit {
		return nil
	}
	displayID := firstNonEmpty(state.ID, id)
	if !kernelStateEqual(wantKernel, gotKernel) {
		if kernelStateIsDefault(wantKernel) {
			return fmt.Errorf("VM %q is already running with kernel %q; run @restart or @stop before using --kernel default", displayID, gotKernel)
		}
		if kernelStateIsDefault(gotKernel) {
			return fmt.Errorf("VM %q is already running with the default kernel; run @restart or @stop before using --kernel %s", displayID, wantKernel)
		}
		return fmt.Errorf("VM %q is already running with kernel %q, not %q; run @restart or @stop first", displayID, gotKernel, wantKernel)
	}
	if wantInit == "" {
		return fmt.Errorf("VM %q is already running with init %q; stop or restart it before using --no-init", displayID, gotInit)
	}
	if gotInit == "" {
		return fmt.Errorf("VM %q is already running without tracked init %q; run @restart or @stop before using --init", displayID, wantInit)
	}
	return fmt.Errorf("VM %q is already running with init %q, not %q; run @restart or @stop first", displayID, gotInit, wantInit)
}

func (s *shellState) startVM(id string, ctx commandContext, stderr io.Writer) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("vm id is required")
	}
	if strings.TrimSpace(ctx.Kernel) == "" {
		ctx.Kernel = defaultKernelForImage(ctx.Image)
	}
	req := client.StartInstanceRequest{
		Image:          localImageName(ctx.Image, ctx.Arch),
		InitSystem:     ctx.InitSystem,
		Kernel:         ctx.Kernel,
		MemoryMB:       ctx.MemoryMB,
		CPUs:           ctx.CPUs,
		NestedVirt:     ctx.NestedVirt,
		TimeoutSeconds: vmshBootTimeoutSeconds(),
	}
	if ctx.Network {
		req.Network = networkConfigForContext(ctx)
	}
	boot := newBootStatus(stderr)
	defer boot.Close()
	runCtx, stopInterrupts, interrupted := s.interruptibleCommandContext()
	defer stopInterrupts()
	var state client.InstanceState
	var err error
	onEvent := func(event client.BootEvent) error {
		boot.Update(event)
		return nil
	}
	if api, ok := s.api.(instanceStartContextAPI); ok {
		state, err = api.StartInstanceStreamWithIDContext(runCtx, id, req, onEvent)
	} else {
		state, err = s.api.StartInstanceStreamWithID(id, req, onEvent)
	}
	if interrupted.Load() {
		return persistentShellExit{code: 130}
	}
	if err != nil {
		return err
	}
	startedID := firstNonEmpty(state.ID, id)
	if s.vmRunning == nil {
		s.vmRunning = map[string]bool{}
	}
	s.vmRunning[startedID] = true
	return nil
}

func vmshBootTimeoutSeconds() float64 {
	raw := strings.TrimSpace(os.Getenv("VMSH_VM_BOOT_TIMEOUT"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CCX3_VM_BOOT_TIMEOUT"))
	}
	if raw == "" {
		return defaultVMSHBootTimeoutSeconds
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return defaultVMSHBootTimeoutSeconds
	}
	return seconds
}

type bootStatus struct {
	*terminalHoldStatus
}

func newBootStatus(w io.Writer) *bootStatus {
	return &bootStatus{terminalHoldStatus: newTerminalHoldStatus(w, "Boot: starting VM")}
}

func (b *bootStatus) Update(event client.BootEvent) {
	if event.Kind == "serial" {
		if !b.tty && event.Data != "" {
			fmt.Fprint(b.w, event.Data)
		}
		return
	}
	msg := formatBootEvent(event)
	if msg == "" {
		return
	}
	if !b.tty {
		b.terminalHoldStatus.Update(msg)
		return
	}
	switch event.Kind {
	case "ready":
		b.Close()
	case "error":
		b.finishWith(msg)
	default:
		b.terminalHoldStatus.Update(msg)
	}
}

type terminalHoldStatus struct {
	w        io.Writer
	tty      bool
	done     chan struct{}
	finished chan struct{}
	mu       sync.Mutex
	message  string
	fallback string
	active   bool
}

func newTerminalHoldStatus(w io.Writer, fallback string) *terminalHoldStatus {
	b := &terminalHoldStatus{
		w:        w,
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		fallback: fallback,
	}
	if file, ok := terminalWriterFile(w); ok && terminal.IsTerminalFD(int(file.Fd())) {
		b.tty = true
		b.active = true
		go b.spin()
		return b
	}
	close(b.finished)
	return b
}

func (b *terminalHoldStatus) Update(message string) {
	if b == nil || message == "" {
		return
	}
	if !b.tty {
		fmt.Fprintln(b.w, message)
		return
	}
	b.mu.Lock()
	b.message = message
	b.mu.Unlock()
}

func (b *terminalHoldStatus) Close() {
	if b == nil || !b.tty {
		return
	}
	b.mu.Lock()
	if !b.active {
		b.mu.Unlock()
		return
	}
	b.active = false
	close(b.done)
	b.mu.Unlock()
	<-b.finished
	fmt.Fprint(b.w, "\r\033[2K")
}

func (b *terminalHoldStatus) finishWith(message string) {
	b.Close()
	fmt.Fprintln(b.w, message)
}

func (b *terminalHoldStatus) spin() {
	defer close(b.finished)
	frames := []string{"-", "\\", "|", "/"}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.mu.Lock()
			msg := b.message
			b.mu.Unlock()
			if msg == "" {
				msg = b.fallback
			}
			msg = compactStatusMessage(msg)
			fmt.Fprintf(b.w, "\r\033[2K%s %s", frames[i%len(frames)], msg)
			i++
		}
	}
}

func compactStatusMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	lines := strings.Split(message, "\n")
	compact := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			compact = append(compact, line)
		}
	}
	return strings.Join(compact, " | ")
}

func defaultNetworkConfig() *client.NetworkConfig {
	return &client.NetworkConfig{Enabled: true, AllowInternet: true}
}

func networkConfigForContext(ctx commandContext) *client.NetworkConfig {
	cfg := defaultNetworkConfig()
	if ctx.Isolated {
		cfg.BlockHostAccess = true
	}
	return cfg
}

func (s *shellState) stopVM(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("vm id is required")
	}
	s.closeGuestSessionForBackendID(id)
	if err := s.api.ShutdownInstanceWithID(id); err != nil {
		return err
	}
	delete(s.vmRunning, id)
	return nil
}

func (s *shellState) closeGuestSessionForBackendID(id string) {
	if s.guestShell == nil || strings.TrimSpace(id) == "" {
		return
	}
	if !strings.HasPrefix(s.guestShell.key, strings.TrimSpace(id)+"\x00") {
		return
	}
	s.closeGuestSession()
}

func (s *shellState) stopSession(at atLine, stdout io.Writer) error {
	fields, err := splitShellFields(at.Command)
	if err != nil {
		return err
	}
	if len(fields) > 1 || (at.Options.VMID != "" && len(fields) != 0) {
		return fmt.Errorf("usage: @stop [name|vm:name|ssh:name]")
	}
	if len(fields) == 0 {
		if at.Options.VMID != "" {
			ctx := s.context.withOptions(at.Options)
			id := backendVMID(ctx)
			if err := s.stopVMAndReport(id, stdout); err != nil {
				return err
			}
			s.leaveStoppedVM(id)
			return nil
		}
		if s.context.Mode == modeSSH {
			host := s.context.SSHHost
			if key, ok := s.sshSessionKeyForContext(s.context); ok && s.closeSSHSessionKey(key) {
				s.leaveStoppedSSHKey(key)
				_, err := fmt.Fprintf(stdout, "Stopped SSH session %s\n", host)
				return err
			}
			return fmt.Errorf("ssh session %s is not open", host)
		}
		ctx := s.context.withOptions(at.Options)
		id := backendVMID(ctx)
		if err := s.stopVMAndReport(id, stdout); err != nil {
			return err
		}
		s.leaveStoppedContext(ctx)
		return nil
	}

	name := fields[0]
	if forced, vmID := parseExplicitVMStopTarget(name); forced {
		match, err := s.resolveVMStopTarget(vmID)
		if err != nil {
			return err
		}
		if match.kind != stopTargetVM {
			return fmt.Errorf("no running VM named %q", vmID)
		}
		if err := s.stopVMAndReport(match.id, stdout); err != nil {
			return err
		}
		s.leaveStoppedVM(match.id)
		return nil
	}
	if forced, host := parseExplicitSSHStopTarget(name); forced {
		if key, ok := s.sshSessionKeyForName(host); ok && s.closeSSHSessionKey(key) {
			s.leaveStoppedSSHKey(key)
			_, err := fmt.Fprintf(stdout, "Stopped SSH session %s\n", host)
			return err
		}
		return fmt.Errorf("ssh session %s is not open", host)
	}
	match, err := s.resolveStopTarget(name)
	if err != nil {
		return err
	}
	switch match.kind {
	case stopTargetSSH:
		if s.closeSSHSessionKey(match.id) {
			s.leaveStoppedSSHKey(match.id)
			_, err := fmt.Fprintf(stdout, "Stopped SSH session %s\n", name)
			return err
		}
		return fmt.Errorf("ssh session %s is not open", name)
	case stopTargetVM:
		if err := s.stopVMAndReport(match.id, stdout); err != nil {
			return err
		}
		s.leaveStoppedVM(match.id)
		return nil
	default:
		return fmt.Errorf("no running VM or SSH session named %q", name)
	}
}

func (s *shellState) stopVMAndReport(id string, stdout io.Writer) error {
	if err := s.stopVM(id); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "Stopped VM %s\n", id)
	return err
}

type stopTargetKind int

const (
	stopTargetNone stopTargetKind = iota
	stopTargetVM
	stopTargetSSH
)

type stopTargetMatch struct {
	kind stopTargetKind
	id   string
}

func (s *shellState) resolveStopTarget(name string) (stopTargetMatch, error) {
	name = strings.TrimSpace(name)
	sshMatch := false
	sshKey := ""
	if key, ok := s.sshSessionKeyForName(name); ok {
		sshMatch = true
		sshKey = key
	}
	vmTarget, err := s.resolveVMStopTarget(name)
	if err != nil {
		return stopTargetMatch{}, err
	}
	vmMatch := vmTarget.kind == stopTargetVM
	switch {
	case sshMatch && vmMatch:
		return stopTargetMatch{}, fmt.Errorf("stop target %q is ambiguous: use @stop vm:%s for the VM or @stop ssh:%s for the SSH session", name, name, name)
	case sshMatch:
		return stopTargetMatch{kind: stopTargetSSH, id: sshKey}, nil
	case vmMatch:
		return vmTarget, nil
	default:
		return stopTargetMatch{}, nil
	}
}

func (s *shellState) resolveVMStopTarget(name string) (stopTargetMatch, error) {
	name = strings.TrimSpace(name)
	var vmMatches []string
	sharedID := backendVMIDFor(name, false)
	if state, err := s.api.InstanceStatusOf(sharedID); err != nil {
		return stopTargetMatch{}, err
	} else if instanceStateIsLive(state) {
		vmMatches = append(vmMatches, sharedID)
	}
	isolatedID := backendVMIDFor(name, true)
	if isolatedID != sharedID {
		if state, err := s.api.InstanceStatusOf(isolatedID); err != nil {
			return stopTargetMatch{}, err
		} else if instanceStateIsLive(state) {
			vmMatches = append(vmMatches, isolatedID)
		}
	}
	switch {
	case len(vmMatches) > 1:
		return stopTargetMatch{}, fmt.Errorf("stop target %q is ambiguous because both shared and isolated VMs are running with that name; this should only happen for sessions started by older vmsh builds, use @stop --vm %s or @stop --vm %s", name, name, backendVMIDFor(name, true))
	case len(vmMatches) == 1:
		return stopTargetMatch{kind: stopTargetVM, id: vmMatches[0]}, nil
	default:
		return stopTargetMatch{}, nil
	}
}

func (s *shellState) leaveStoppedVM(id string) {
	id = strings.TrimSpace(id)
	if s.context.Mode == modeVM && backendVMID(s.context) == id {
		s.leaveStoppedContext(s.context)
		return
	}
	s.contextStack = filterContextStack(s.contextStack, func(ctx commandContext) bool {
		return !(ctx.Mode == modeVM && backendVMID(ctx) == id)
	})
}

func (s *shellState) leaveStoppedSSHKey(key string) {
	key = strings.TrimSpace(key)
	if contextSSHSessionKey(s.context) == key {
		s.leaveStoppedContext(s.context)
		return
	}
	s.contextStack = filterContextStack(s.contextStack, func(ctx commandContext) bool {
		return contextSSHSessionKey(ctx) != key
	})
}

func (s *shellState) leaveStoppedContext(stopped commandContext) {
	for len(s.contextStack) > 0 {
		last := len(s.contextStack) - 1
		parent := s.contextStack[last]
		s.contextStack = s.contextStack[:last]
		if contextSameSession(parent, stopped) {
			continue
		}
		s.activateContext(parent)
		return
	}
	s.activateContext(hostCommandContext(s.context, commandOptions{}))
}

func filterContextStack(stack []commandContext, keep func(commandContext) bool) []commandContext {
	out := stack[:0]
	for _, ctx := range stack {
		if keep(ctx) {
			out = append(out, ctx)
		}
	}
	return out
}

func contextSameSession(a, b commandContext) bool {
	if a.Mode != b.Mode {
		return false
	}
	switch a.Mode {
	case modeVM:
		return backendVMID(a) == backendVMID(b)
	case modeSSH:
		return strings.TrimSpace(a.SSHHost) == strings.TrimSpace(b.SSHHost)
	case modeHost:
		return true
	default:
		return false
	}
}

func contextSSHSessionKey(ctx commandContext) string {
	if ctx.Mode != modeSSH {
		return ""
	}
	cfg, err := resolveSSHConfig(ctx)
	if err != nil {
		return ""
	}
	return persistentSSHShellKey(cfg.cacheKey(), ctx)
}

func parseExplicitVMStopTarget(name string) (bool, string) {
	name = strings.TrimSpace(name)
	for _, prefix := range []string{"@vm:", "vm:"} {
		if strings.HasPrefix(name, prefix) {
			return true, strings.TrimSpace(strings.TrimPrefix(name, prefix))
		}
	}
	return false, name
}

func (s *shellState) restartVM(id string, ctx commandContext, stderr io.Writer) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("vm id is required")
	}
	if s.confirmVMRestart == nil {
		return fmt.Errorf("restart cancelled for VM %s", id)
	}
	ok, err := s.confirmVMRestart(id, stderr)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("restart cancelled for VM %s", id)
	}
	state, err := s.api.InstanceStatusOf(id)
	if err != nil {
		return err
	}
	ctx = restartContextFromState(ctx, state)
	s.closeGuestSession()
	if err := s.stopVM(id); err != nil {
		return err
	}
	return s.startVM(id, ctx, stderr)
}

func restartContextFromState(ctx commandContext, state client.InstanceState) commandContext {
	if strings.TrimSpace(ctx.Image) == "" {
		ctx.Image = state.Image
	}
	if strings.TrimSpace(ctx.InitSystem) == "" {
		ctx.InitSystem = state.InitSystem
	}
	if strings.TrimSpace(ctx.Kernel) == "" {
		ctx.Kernel = state.Kernel
	}
	if ctx.MemoryMB == 0 {
		ctx.MemoryMB = state.MemoryMB
	}
	if ctx.CPUs == 0 {
		ctx.CPUs = state.CPUs
	}
	if !ctx.NestedVirt {
		ctx.NestedVirt = state.NestedVirt
	}
	if !ctx.Network && strings.TrimSpace(state.NetworkIPv4) != "" {
		ctx.Network = true
	}
	return ctx
}

func (s *shellState) saveVM(at atLine, stdout io.Writer) error {
	fields, err := splitShellFields(at.Command)
	if err != nil {
		return err
	}
	if len(fields) != 1 || hasSaveOnlyUnsupportedOptions(at.Options) {
		return fmt.Errorf("usage: @save [--vm id] tag")
	}
	name := strings.TrimSpace(fields[0])
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("usage: @save [--vm id] tag")
	}
	ctx := s.context.withOptions(at.Options)
	id := backendVMID(ctx)
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("vm id is required")
	}
	s.closeGuestSession()
	state, err := s.api.SaveInstanceImage(id, client.SaveImageRequest{
		Name:  name,
		Image: localImageName(ctx.Image, ctx.Arch),
	})
	if err != nil {
		return err
	}
	if s.imageCache == nil {
		s.imageCache = map[string]bool{}
	}
	s.imageCache[state.Name] = true
	if _, err := fmt.Fprintf(stdout, "Saved %s as %s\n", id, state.Name); err != nil {
		return err
	}
	return nil
}

func hasSaveOnlyUnsupportedOptions(opts commandOptions) bool {
	for _, field := range opts.OptionFields {
		if strings.TrimSpace(field) != "--vm" {
			return true
		}
	}
	return false
}

func (s *shellState) removeImage(at atLine, stdout io.Writer) error {
	fields, err := splitShellFields(at.Command)
	if err != nil {
		return err
	}
	if len(fields) != 1 || hasRMIUnsupportedOptions(at.Options) {
		return fmt.Errorf("usage: @rmi image")
	}
	name := strings.TrimSpace(fields[0])
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("usage: @rmi image")
	}
	if err := s.api.DeleteImage(name); err != nil {
		return err
	}
	if s.imageCache != nil {
		delete(s.imageCache, name)
	}
	if _, err := fmt.Fprintf(stdout, "Removed %s\n", name); err != nil {
		return err
	}
	return nil
}

func hasRMIUnsupportedOptions(opts commandOptions) bool {
	return len(opts.OptionFields) != 0
}

func (s *shellState) startTmux(at atLine) error {
	fields, err := splitShellFields(at.Command)
	if err != nil {
		return err
	}
	if len(fields) > 1 || len(at.Options.OptionFields) != 0 {
		return fmt.Errorf("usage: @tmux [session]")
	}
	session := "vmsh"
	if len(fields) == 1 {
		session = strings.TrimSpace(fields[0])
	}
	if session == "" || strings.HasPrefix(session, "-") {
		return fmt.Errorf("usage: @tmux [session]")
	}
	tmux := "tmux"
	if s.tmuxExec == nil {
		resolved, err := exec.LookPath("tmux")
		if err != nil {
			return fmt.Errorf("tmux not found on PATH")
		}
		tmux = resolved
	}
	command, err := s.tmuxDefaultCommand()
	if err != nil {
		return err
	}
	args := tmuxLaunchArgs(session, command, os.Getenv("TMUX") != "")
	if s.tmuxExec != nil {
		return s.tmuxExec(append([]string{tmux}, args...))
	}
	cmd := exec.Command(tmux, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *shellState) tmuxDefaultCommand() (string, error) {
	vmshPath := strings.TrimSpace(s.vmshPath)
	if vmshPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		vmshPath = exe
	}
	if abs, err := filepath.Abs(vmshPath); err == nil {
		vmshPath = abs
	}
	args := []string{"exec", shellQuote(vmshPath), "-cache-dir", shellQuote(s.rootCache)}
	if strings.TrimSpace(s.ccvmPath) != "" {
		args = append(args, "-ccvm", shellQuote(s.ccvmPath))
	}
	args = append(args, "-vm", shellQuote(firstNonEmpty(strings.TrimSpace(s.context.VMID), "default")))
	if strings.TrimSpace(s.context.Image) != "" {
		args = append(args, "-image", shellQuote(s.context.Image))
	}
	return strings.Join(args, " "), nil
}

func tmuxLaunchArgs(session, command string, insideTmux bool) []string {
	setup := []string{
		"new-session", "-d", "-A", "-s", session, "-n", "vmsh", command,
		";", "set-option", "-t", session, "default-command", command,
	}
	if insideTmux {
		return append(setup, ";", "switch-client", "-t", session)
	}
	return append(setup, ";", "attach-session", "-t", session)
}

func (s *shellState) chdirContext(target string) error {
	shellTarget, err := s.targetFor(s.context)
	if err != nil {
		return err
	}
	return shellTarget.Chdir(target)
}

func (s *shellState) chdirHost(target string) error {
	if target == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		target = home
	}
	if strings.HasPrefix(target, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		switch {
		case target == "~":
			target = home
		case strings.HasPrefix(target, "~/"):
			target = filepath.Join(home, target[2:])
		default:
			return fmt.Errorf("user home expansion is only supported for ~ and ~/ paths")
		}
	}
	target = os.ExpandEnv(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(s.hostCWD, target)
	}
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", target)
	}
	s.hostCWD = target
	if err := os.Chdir(target); err != nil {
		return err
	}
	if s.hostShell != nil {
		if err := s.hostShell.run("cd "+shellQuote(target), io.Discard, io.Discard, nil); err != nil {
			s.hostShell.close()
			s.hostShell = nil
			return nil
		}
		if cwd := s.hostShell.cwd(); cwd != "" {
			s.hostCWD = cwd
		}
	}
	return nil
}

func (s *shellState) chdirGuest(target string) error {
	return s.chdirGuestContext(s.context, target)
}

func (s *shellState) chdirSSHContext(ctx commandContext, target string) error {
	script := sshRemoteCDScript(s.currentSSHCWD(ctx), target)
	var out bytes.Buffer
	var stderr bytes.Buffer
	var err error
	if shell := s.sshShellForContext(ctx); shell != nil {
		err = shell.run(remoteCDCommand(target)+" && pwd -P", &out, &stderr)
	} else {
		err = s.runSSHCommand(ctx, script, nil, &out, &stderr, false, false)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ssh cd %s: %s", ctx.SSHHost, msg)
	}
	cwd := lastNonEmptyLine(out.String())
	if cwd == "" {
		return fmt.Errorf("ssh cd %s: remote pwd returned no path", ctx.SSHHost)
	}
	ctx.CWD = cwd
	s.context = ctx
	s.rememberContextCWD(ctx)
	return nil
}

func sshRemoteCDScript(current, target string) string {
	parts := []string{"set -e"}
	current = strings.TrimSpace(current)
	if current != "" && current != "~" {
		parts = append(parts, remoteCDCommand(current))
	}
	parts = append(parts, remoteCDCommand(target), "pwd -P")
	return strings.Join(parts, "\n")
}

func remoteCDCommand(target string) string {
	switch {
	case target == "" || target == "~":
		return "cd"
	case strings.HasPrefix(target, "~/"):
		return "cd \"$HOME\"/" + shellQuote(strings.TrimPrefix(target, "~/"))
	default:
		return "cd " + shellQuote(shellPathOperandForOptions(target))
	}
}

func shellPathOperandForOptions(target string) string {
	if strings.HasPrefix(target, "-") {
		return "./" + target
	}
	return target
}

func lastNonEmptyLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

func (s *shellState) chdirGuestContext(ctx commandContext, target string) error {
	s.closeGuestSession()
	current := ctx.CWD
	if current == "" {
		if ctx.Isolated {
			current = guestHomeDir(ctx)
		} else {
			_, current, _ = guestHostPaths(s.hostCWD)
		}
	}
	home := guestHomeDir(ctx)
	if target == "" || target == "~" {
		target = home
	}
	if strings.HasPrefix(target, "~/") {
		target = path.Join(home, target[2:])
	}
	if strings.HasPrefix(target, "~") {
		return fmt.Errorf("guest user home expansion is only supported for ~ and ~/ paths")
	}
	if !strings.HasPrefix(target, "/") {
		target = path.Join(current, target)
	}
	target = path.Clean(target)
	if strings.HasPrefix(target, guestHostMount+"/") || target == guestHostMount {
		if ctx.Isolated {
			return fmt.Errorf("%s is not mounted in isolated context", guestHostMount)
		}
		hostPath, ok := guestHostPathToHost(s.hostCWD, target)
		if !ok {
			return fmt.Errorf("cannot map guest host path %q", target)
		}
		if err := s.chdirHost(hostPath); err != nil {
			return err
		}
		ctx.CWD = ""
		s.context = ctx
		return nil
	}
	ctx.CWD = target
	s.context = ctx
	s.rememberContextCWD(ctx)
	return nil
}

func guestHostPathToHost(hostCWD, guestPath string) (string, bool) {
	if guestPath != guestHostMount && !strings.HasPrefix(guestPath, guestHostMount+"/") {
		return "", false
	}
	hostRoot, _, err := guestHostPaths(hostCWD)
	if err != nil {
		return "", false
	}
	rel := strings.TrimPrefix(guestPath, guestHostMount)
	if rel == "" || rel == "/" {
		return hostRoot, true
	}
	return filepath.Join(hostRoot, filepath.FromSlash(strings.TrimPrefix(rel, "/"))), true
}

func (s *shellState) prompt() string {
	promptCWD := s.hostCWD
	if target, err := s.targetFor(s.context); err == nil {
		promptCWD = target.CurrentCWD()
	}
	leaf := displayPathLeaf(promptCWD, s.context.Mode)
	base := colorGreen + "➜" + colorReset + "  "
	cwd := s.promptCWDColor(promptCWD) + leaf + colorReset + " "
	if s.context.Mode == modeSSH {
		target := "(" + s.context.SSHHost + ")"
		return base + colorMagenta + "ssh:" + colorReset + colorYellow + target + colorReset + " " + cwd
	}
	if s.context.Mode == modeVM {
		target := "(" + contextImageText(s.context)
		if s.context.VMID != "" && s.context.VMID != "default" {
			target += ":" + s.context.VMID
		}
		target += ")"
		label := "vm:"
		if s.context.Isolated {
			label = "vm isolated:"
		}
		if isRootGuestContext(s.context) {
			label = "root " + label
		}
		return base + colorMagenta + label + colorReset + colorYellow + target + colorReset + " " + cwd
	}
	return base + colorBlue + localHostPromptName() + colorReset + " " + cwd
}

func localHostPromptName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "localhost"
	}
	return strings.TrimSpace(name)
}

func (s *shellState) promptCWDColor(cwd string) string {
	if s.context.Mode == modeHost {
		return colorCyan
	}
	if s.context.Isolated {
		return colorMagenta
	}
	if cwd == guestHostMount || strings.HasPrefix(cwd, guestHostMount+"/") {
		return colorCyan
	}
	return colorYellow
}

func displayPathLeaf(value string, mode shellMode) string {
	if mode == modeVM || mode == modeSSH {
		leaf := path.Base(value)
		if leaf == "." || leaf == "/" {
			return value
		}
		return leaf
	}
	leaf := filepath.Base(value)
	if leaf == "." || leaf == string(filepath.Separator) {
		return value
	}
	return leaf
}

func contextImageText(ctx commandContext) string {
	if ctx.Image == "" || ctx.Arch == "" {
		return ctx.Image
	}
	return ctx.Image + "@" + ctx.Arch
}

func isRootGuestContext(ctx commandContext) bool {
	user := strings.TrimSpace(guestRunUser(ctx))
	return user == "root" || user == "0" || strings.HasPrefix(user, "0:")
}

func terminalRequestSize(stdout io.Writer) (bool, int, int) {
	file, ok := terminalWriterFile(stdout)
	if !ok || !terminal.IsTerminalFD(int(file.Fd())) {
		return false, 0, 0
	}
	cols, rows, err := terminal.Size(file)
	if err != nil {
		return true, 0, 0
	}
	return true, cols, rows
}

func terminalEnv(cols, rows int) []string {
	keys := []string{
		"TERM",
		"COLORTERM",
		"LS_COLORS",
		"NO_COLOR",
		"CLICOLOR",
		"CLICOLOR_FORCE",
		"FORCE_COLOR",
	}
	env := make([]string, 0, len(keys)+2)
	termSeen := false
	for _, key := range keys {
		value, ok := os.LookupEnv(key)
		if !ok || value == "" {
			continue
		}
		if key == "TERM" {
			termSeen = true
			value = guestTERM(value)
		}
		env = append(env, key+"="+value)
	}
	if !termSeen {
		env = append(env, "TERM=xterm-256color")
	}
	if _, ok := os.LookupEnv("CLICOLOR"); !ok {
		env = append(env, "CLICOLOR=1")
	}
	if cols > 0 {
		env = append(env, "COLUMNS="+strconv.Itoa(cols))
	}
	if rows > 0 {
		env = append(env, "LINES="+strconv.Itoa(rows))
	}
	return env
}

func guestTERM(term string) string {
	switch strings.TrimSpace(term) {
	case "xterm-ghostty":
		return "xterm-256color"
	default:
		return term
	}
}

func (s *shellState) drawPromptStatus(stdout io.Writer) {
	seq := s.statusSeq.Add(1)
	code := s.lastCode
	if code == 0 {
		return
	}
	file, ok := terminalWriterFile(stdout)
	if !ok || !terminal.IsTerminalFD(int(file.Fd())) {
		return
	}
	cols, _, err := terminal.Size(file)
	if err != nil || cols <= 0 {
		return
	}
	status := colorYellow + "exit " + strconv.Itoa(code) + colorReset
	visible := len("exit ") + len(strconv.Itoa(code))
	col := cols - visible + 1
	if col < 1 {
		col = 1
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		if s.statusSeq.Load() != seq {
			return
		}
		fmt.Fprintf(file, "\x1b7\x1b[%dG%s\x1b8", col, status)
	}()
}

func (s *shellState) printStatus(w io.Writer) error {
	chain := s.activeContextChain()
	if _, err := fmt.Fprintln(w, "context chain:"); err != nil {
		return err
	}
	for i, ctx := range chain {
		line, err := s.contextStatusLine(ctx, i == len(chain)-1)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "  %d. %s\n", i+1, line); err != nil {
			return err
		}
	}
	current, err := s.contextStatusLine(s.context, true)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "current: %s\n", current); err != nil {
		return err
	}
	return s.printCurrentStatusDetails(w)
}

func (s *shellState) activeContextChain() []commandContext {
	chain := make([]commandContext, 0, len(s.contextStack)+1)
	chain = append(chain, s.contextStack...)
	chain = append(chain, s.context)
	if len(chain) == 0 || chain[0].Mode != modeHost {
		chain = append([]commandContext{hostCommandContext(s.context, commandOptions{})}, chain...)
	}
	return chain
}

func (s *shellState) contextStatusLine(ctx commandContext, current bool) (string, error) {
	var parts []string
	switch ctx.Mode {
	case modeHost:
		parts = []string{"host", "cwd=" + emptyText(s.hostCWD, "-")}
	case modeVM:
		id := backendVMID(ctx)
		state, err := s.api.InstanceStatusOf(id)
		if err != nil {
			return "", err
		}
		parts = []string{
			"vm",
			emptyText(normalizedVMID(ctx.VMID), "default"),
			"image=" + emptyText(contextImageText(ctx), "-"),
			"backend=" + emptyText(id, "-"),
			"isolated=" + strconv.FormatBool(ctx.Isolated),
			"user=" + emptyText(guestRunUser(ctx), "-"),
			"cwd=" + emptyText(s.currentGuestCWD(ctx), "-"),
			"status=" + emptyText(state.Status, "unknown"),
		}
		if state.InitSystem != "" {
			parts = append(parts, "init="+state.InitSystem)
		}
		if state.Kernel != "" {
			parts = append(parts, "kernel="+state.Kernel)
		}
		if state.NetworkIPv4 != "" {
			parts = append(parts, "addr="+state.NetworkIPv4)
		}
	case modeSSH:
		session := "closed"
		if s.sshShellForContext(ctx) != nil {
			session = "open"
		}
		parts = []string{
			"ssh",
			emptyText(ctx.SSHHost, "-"),
			"user=" + emptyText(s.sshStatusUser(ctx), "-"),
			"cwd=" + emptyText(s.currentSSHCWD(ctx), "-"),
			"session=" + session,
		}
	default:
		parts = []string{string(ctx.Mode)}
	}
	if current {
		parts = append(parts, "[current]")
	}
	return strings.Join(parts, " "), nil
}

func (s *shellState) sshStatusUser(ctx commandContext) string {
	if strings.TrimSpace(ctx.User) != "" {
		return strings.TrimSpace(ctx.User)
	}
	if shell := s.sshShellForContext(ctx); shell != nil && shell.client != nil {
		return shell.client.config.User
	}
	if cfg, err := resolveSSHConfig(ctx); err == nil {
		return cfg.User
	}
	return ""
}

func (s *shellState) printCurrentStatusDetails(w io.Writer) error {
	switch s.context.Mode {
	case modeHost:
		_, err := fmt.Fprintf(w, "context: %s\nhost cwd: %s\n",
			s.context.Mode,
			s.hostCWD,
		)
		return err
	case modeSSH:
		status := "closed"
		if s.sshShellForContext(s.context) != nil {
			status = "open"
		}
		_, err := fmt.Fprintf(w, "context: %s\nssh host: %s\nhost cwd: %s\nssh cwd: %s\nssh session: %s\n",
			s.context.Mode,
			emptyText(s.context.SSHHost, "-"),
			s.hostCWD,
			emptyText(s.currentSSHCWD(s.context), "-"),
			status,
		)
		return err
	case modeVM:
		id := backendVMID(s.context)
		state, err := s.api.InstanceStatusOf(id)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "context: %s\nimage: %s\nvm: %s\nbackend vm: %s\nisolated: %t\nhost cwd: %s\nguest cwd: %s\nvm status: %s\n",
			s.context.Mode,
			emptyText(contextImageText(s.context), "-"),
			emptyText(s.context.VMID, "-"),
			emptyText(id, "-"),
			s.context.Isolated,
			s.hostCWD,
			emptyText(s.currentGuestCWD(s.context), "-"),
			emptyText(state.Status, "unknown"),
		)
		if err != nil {
			return err
		}
		if state.NetworkIPv4 != "" {
			_, err = fmt.Fprintf(w, "vm address: %s\n", state.NetworkIPv4)
		}
		return err
	default:
		_, err := fmt.Fprintf(w, "context: %s\n", s.context.Mode)
		return err
	}
}

func (s *shellState) printVMs(w io.Writer) error {
	states, err := s.api.InstanceStatuses()
	if err != nil {
		return err
	}
	return s.printSessionTree(w, states, s.sshSessionStates(), s.sshConnectionStates())
}

type sessionTreeNode struct {
	label    string
	current  bool
	children []*sessionTreeNode
}

func (s *shellState) printSessionTree(w io.Writer, states []client.InstanceState, sshSessions []sshSessionState, sshConnections []sshConnectionState) error {
	root := &sessionTreeNode{
		label:   "host " + s.hostCWD,
		current: s.context.Mode == modeHost,
	}
	states = append([]client.InstanceState(nil), states...)
	sort.Slice(states, func(i, j int) bool {
		return firstNonEmpty(strings.TrimSpace(states[i].ID), "default") < firstNonEmpty(strings.TrimSpace(states[j].ID), "default")
	})
	vmNodes := map[string]*sessionTreeNode{}
	for _, state := range states {
		if !instanceStateIsLive(state) {
			continue
		}
		id := firstNonEmpty(strings.TrimSpace(state.ID), "default")
		kind := "vm"
		if strings.HasSuffix(id, isolatedVMSuffix) {
			kind = "isolated vm"
		}
		parts := []string{kind, id, emptyText(state.Status, "unknown")}
		if state.Image != "" {
			parts = append(parts, "image="+state.Image)
		}
		if state.InitSystem != "" {
			parts = append(parts, "init="+state.InitSystem)
		}
		if state.Kernel != "" {
			parts = append(parts, "kernel="+state.Kernel)
		}
		if state.NetworkIPv4 != "" {
			parts = append(parts, "addr="+state.NetworkIPv4)
		}
		node := &sessionTreeNode{
			label:   strings.Join(parts, " "),
			current: s.context.Mode == modeVM && backendVMID(s.context) == id,
		}
		root.children = append(root.children, node)
		vmNodes[id] = node
	}
	for _, session := range sshSessions {
		parts := []string{"ssh", session.Name}
		if session.User != "" {
			parts = append(parts, "user="+session.User)
		}
		if session.CWD != "" {
			parts = append(parts, "cwd="+session.CWD)
		}
		node := &sessionTreeNode{
			label:   strings.Join(parts, " "),
			current: s.context.Mode == modeSSH && contextSessionKey(s.context) == contextSessionKey(session.Ctx),
		}
		root.children = append(root.children, node)
	}
	for _, conn := range sshConnections {
		parts := []string{"ssh connection", conn.Name}
		if conn.Detail != "" {
			parts = append(parts, "("+conn.Detail+")")
		}
		root.children = append(root.children, &sessionTreeNode{label: strings.Join(parts, " ")})
	}
	return printSessionTreeNode(w, root, "", true, true)
}

func instanceStateIsLive(state client.InstanceState) bool {
	status := strings.TrimSpace(state.Status)
	return status != "" && !strings.EqualFold(status, "stopped")
}

func contextSessionKey(ctx commandContext) string {
	return strings.Join([]string{
		string(ctx.Mode),
		backendVMID(ctx),
		localImageName(ctx.Image, ctx.Arch),
		ctx.SSHHost,
		contextUserKey(ctx),
	}, "\x00")
}

func printSessionTreeNode(w io.Writer, node *sessionTreeNode, prefix string, last, root bool) error {
	connector := ""
	childPrefix := ""
	if !root {
		if last {
			connector = "`- "
			childPrefix = prefix + "   "
		} else {
			connector = "|- "
			childPrefix = prefix + "|  "
		}
	}
	label := node.label
	if node.current {
		label += " [current]"
	}
	if _, err := fmt.Fprintln(w, prefix+connector+label); err != nil {
		return err
	}
	if root {
		childPrefix = ""
	}
	for i, child := range node.children {
		if err := printSessionTreeNode(w, child, childPrefix, i == len(node.children)-1, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *shellState) printJobs(w io.Writer) error {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if len(s.jobs) == 0 {
		_, err := fmt.Fprintln(w, "No jobs")
		return err
	}
	for _, job := range s.jobs {
		status := "running"
		if job.Done {
			status = "done"
			if job.Err != "" {
				status = "error"
			}
		}
		if job.Done {
			if _, err := fmt.Fprintf(w, "[%d] %s exit=%d %s\n", job.ID, status, job.Code, job.Command); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(w, "[%d] %s %s\n", job.ID, status, job.Command); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *shellState) help(w io.Writer) error {
	_, err := fmt.Fprintln(w, strings.TrimSpace(`
@<oci-tag> [opts] [cmd]  run cmd in an OCI image, or make it current if cmd is omitted
@host [cmd]              run cmd on the host, or make host current if cmd is omitted
@ssh HOST [cmd]          run cmd through host ssh config, or make SSH host current
@ [opts] [cmd]           update or use the current context
@sudo [cmd]              open a root VM subshell, or run cmd as root in the current VM
@alias [name=value]      list aliases, or set one (example: @alias clear=@host clear)
@alias -d name           delete an alias
@ps                      list VMs and SSH sessions
@jobs                    list background jobs
@status                  show vmsh and selected VM state
@start [--vm id]         start a blank VM
@stop [name|vm:name|ssh:name]  stop an SSH session or VM
@restart [--vm id]       restart a VM after confirmation
@save [--vm id] tag      save the selected VM root filesystem as a local image
@rmi image               remove a locally cached image
@copy SRC DST            copy paths between @:path, @host:path, @vm:id:path, @ssh:host:path, @image:name:path, and active @session:path
@agent codex [args]      run Codex inside the current VM with host ~/.codex mounted
@agent --proxy codex     run Codex through a host auth proxy without mounting ~/.codex
@tmux [session]          open tmux with vmsh as the default pane command
@forward H:G             forward host port H to guest port G
opts: --vm id --cwd path --user user --sudo --init --no-init --kernel default|ubuntu --memory-mb n --memory n[m|g] --cpus n --network --no-network --nested --no-nested --isolated --shared --proxy(@agent)
keys: Ctrl+R reverse history search; Esc/Ctrl+G cancel search
cd <dir>                 change the current host, VM, or SSH working directory
exit [--force]           leave the current subshell, or vmsh at top level
`))
	return err
}

func parseAtLine(line string) (atLine, error) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "@"))
	if body == "" {
		return atLine{}, nil
	}
	tokens, err := lexShellTokens(body)
	if err != nil {
		return atLine{}, err
	}
	if len(tokens) == 0 {
		return atLine{}, nil
	}
	var at atLine
	i := 0
	if !strings.HasPrefix(tokens[0].Value, "--") {
		at.Target = tokens[0].Value
		i = 1
	}
	opts, next, err := parseCommandOptions(tokens, i, at.Target)
	if err != nil {
		return atLine{}, err
	}
	at.Options = opts
	if next < len(tokens) {
		at.Command = strings.TrimSpace(body[tokens[next].Start:])
	}
	return at, nil
}

func parseSSHAtCommand(command string) (string, string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", "", fmt.Errorf("usage: @ssh HOST [cmd]")
	}
	tokens, err := lexShellTokens(command)
	if err != nil {
		return "", "", err
	}
	if len(tokens) == 0 || strings.TrimSpace(tokens[0].Value) == "" {
		return "", "", fmt.Errorf("usage: @ssh HOST [cmd]")
	}
	rest := ""
	if len(tokens) > 1 {
		rest = strings.TrimSpace(command[tokens[1].Start:])
	}
	return tokens[0].Value, rest, nil
}

func parseCommandOptions(tokens []shellToken, start int, target string) (commandOptions, int, error) {
	var opts commandOptions
	i := start
	for i < len(tokens) {
		field := tokens[i].Value
		if field == "--" {
			return opts, i + 1, nil
		}
		if !strings.HasPrefix(field, "--") {
			return opts, i, nil
		}
		name, value, hasInlineValue := strings.Cut(field, "=")
		readValue := func() (string, error) {
			if hasInlineValue {
				return value, nil
			}
			if i+1 >= len(tokens) {
				return "", fmt.Errorf("%s requires a value", name)
			}
			i++
			return tokens[i].Value, nil
		}
		opts.OptionFields = append(opts.OptionFields, field)
		switch name {
		case "--vm":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			opts.VMID = v
		case "--cwd":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			opts.CWD = v
		case "--user":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			opts.User = v
		case "--arch":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			arch := normalizeVMSHArchitecture(v)
			if arch == "" {
				return opts, i, fmt.Errorf("invalid --arch value %q", v)
			}
			opts.Arch = arch
		case "--sudo":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--sudo does not take a value")
			}
			opts.Sudo = true
		case "--init":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--init does not take a value")
			}
			initSystem := "systemd"
			opts.InitSystem = &initSystem
		case "--no-init":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--no-init does not take a value")
			}
			initSystem := ""
			opts.InitSystem = &initSystem
		case "--kernel":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			kernel, err := normalizeVMSHKernel(v)
			if err != nil {
				return opts, i, err
			}
			opts.Kernel = &kernel
		case "--proxy":
			if target != "agent" {
				return opts, i, fmt.Errorf("unknown vmsh option %q", name)
			}
			if hasInlineValue {
				return opts, i, fmt.Errorf("--proxy does not take a value")
			}
			opts.AgentProxy = true
		case "--memory-mb":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			memory, err := parseMemoryMB(v)
			if err != nil {
				return opts, i, err
			}
			opts.MemoryMB = memory
		case "--memory":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			memory, err := parseMemoryMB(v)
			if err != nil {
				return opts, i, err
			}
			opts.MemoryMB = memory
		case "--cpus":
			v, err := readValue()
			if err != nil {
				return opts, i, err
			}
			cpus, err := strconv.Atoi(v)
			if err != nil || cpus <= 0 {
				return opts, i, fmt.Errorf("invalid --cpus value %q", v)
			}
			opts.CPUs = cpus
		case "--network":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--network does not take a value")
			}
			enabled := true
			opts.Network = &enabled
		case "--no-network":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--no-network does not take a value")
			}
			enabled := false
			opts.Network = &enabled
		case "--nested":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--nested does not take a value")
			}
			enabled := true
			opts.NestedVirt = &enabled
		case "--no-nested":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--no-nested does not take a value")
			}
			enabled := false
			opts.NestedVirt = &enabled
		case "--isolated":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--isolated does not take a value")
			}
			enabled := true
			opts.Isolated = &enabled
		case "--shared":
			if hasInlineValue {
				return opts, i, fmt.Errorf("--shared does not take a value")
			}
			enabled := false
			opts.Isolated = &enabled
		default:
			return opts, i, fmt.Errorf("unknown vmsh option %q", name)
		}
		i++
	}
	return opts, i, nil
}

func (c commandContext) withOptions(opts commandOptions) commandContext {
	wasIsolated := c.Isolated
	explicitCWD := opts.CWD != ""
	if opts.VMID != "" {
		c.VMID = opts.VMID
	}
	if opts.CWD != "" {
		c.CWD = opts.CWD
	}
	if opts.User != "" {
		c.User = opts.User
	}
	if opts.Arch != "" {
		c.Arch = opts.Arch
	}
	if opts.Sudo {
		c.User = "root"
	}
	if opts.InitSystem != nil {
		c.InitSystem = *opts.InitSystem
	}
	if opts.Kernel != nil {
		c.Kernel = *opts.Kernel
	}
	if opts.MemoryMB != 0 {
		c.MemoryMB = opts.MemoryMB
	}
	if opts.CPUs != 0 {
		c.CPUs = opts.CPUs
	}
	if opts.Network != nil {
		c.Network = *opts.Network
	}
	if opts.NestedVirt != nil {
		c.NestedVirt = *opts.NestedVirt
	}
	if opts.Isolated != nil {
		c.Isolated = *opts.Isolated
	}
	if opts.Isolated != nil && c.Isolated != wasIsolated && !explicitCWD {
		c.CWD = ""
	}
	return c
}

func hostCommandContext(base commandContext, opts commandOptions) commandContext {
	ctx := base.withOptions(opts)
	ctx.Mode = modeHost
	ctx.SSHHost = ""
	ctx.CWD = ""
	return ctx
}

func vmCommandContext(base commandContext, opts commandOptions, image string) commandContext {
	previousKey := ""
	if base.Mode == modeVM {
		previousKey = contextCWDKey(base)
	}
	ctx := base.withOptions(opts)
	ctx.Mode = modeVM
	ctx.Image = image
	ctx.SSHHost = ""
	if opts.InitSystem == nil {
		ctx.InitSystem = defaultInitSystemForImage(image)
	}
	if opts.Kernel == nil {
		ctx.Kernel = defaultKernelForImage(image)
	}
	if opts.CWD == "" && (base.Mode != modeVM || previousKey != contextCWDKey(ctx)) {
		ctx.CWD = ""
	}
	return ctx
}

func defaultInitSystemForImage(image string) string {
	image = strings.ToLower(strings.TrimSpace(image))
	if image == "ubuntu" || strings.HasPrefix(image, "ubuntu:") || strings.HasSuffix(image, "/ubuntu") || strings.Contains(image, "/ubuntu:") {
		return "systemd"
	}
	return ""
}

func defaultKernelForImage(image string) string {
	image = strings.ToLower(strings.TrimSpace(image))
	if image == "ubuntu" || strings.HasPrefix(image, "ubuntu:") || strings.HasSuffix(image, "/ubuntu") || strings.Contains(image, "/ubuntu:") {
		return "ubuntu"
	}
	return ""
}

func normalizeVMSHKernel(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "default", "alpine":
		return "default", nil
	case "ubuntu":
		return "ubuntu", nil
	default:
		return "", fmt.Errorf("invalid --kernel value %q; expected default or ubuntu", value)
	}
}

func kernelStateIsDefault(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "" || value == "default" || value == "alpine"
}

func kernelStateEqual(a, b string) bool {
	if kernelStateIsDefault(a) && kernelStateIsDefault(b) {
		return true
	}
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

func sudoCommandContext(ctx commandContext, command string) (commandContext, string) {
	if ctx.Mode == modeHost {
		return ctx, "sudo " + command
	}
	ctx = sudoSubshellContext(ctx)
	return ctx, command
}

func sudoSubshellContext(ctx commandContext) commandContext {
	ctx.Mode = modeVM
	ctx.User = "root"
	return ctx
}

func sshCommandContext(base commandContext, opts commandOptions, host string) commandContext {
	ctx := base.withOptions(opts)
	sameHost := base.Mode == modeSSH && base.SSHHost == host
	ctx.Mode = modeSSH
	ctx.SSHHost = host
	ctx.Image = ""
	ctx.VMID = ""
	ctx.Arch = ""
	ctx.Isolated = false
	if opts.User == "" && !sameHost {
		ctx.User = ""
	}
	if opts.CWD == "" && !sameHost {
		ctx.CWD = ""
	}
	return ctx
}

const (
	defaultGuestUID = 1000
	defaultGuestGID = 1000
)

func guestRunUser(ctx commandContext) string {
	if strings.TrimSpace(ctx.User) != "" {
		return ctx.User
	}
	return defaultGuestUser
}

func parseMemoryMB(value string) (uint64, error) {
	raw := strings.TrimSpace(strings.ToLower(value))
	if raw == "" {
		return 0, fmt.Errorf("memory value is required")
	}
	multiplier := uint64(1)
	switch {
	case strings.HasSuffix(raw, "gb"):
		multiplier = 1024
		raw = strings.TrimSuffix(raw, "gb")
	case strings.HasSuffix(raw, "g"):
		multiplier = 1024
		raw = strings.TrimSuffix(raw, "g")
	case strings.HasSuffix(raw, "mb"):
		raw = strings.TrimSuffix(raw, "mb")
	case strings.HasSuffix(raw, "m"):
		raw = strings.TrimSuffix(raw, "m")
	}
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	return n * multiplier, nil
}

func parseCD(line string) (string, bool, error) {
	fields, err := splitShellFields(line)
	if err != nil {
		return "", false, err
	}
	if len(fields) == 0 || fields[0] != "cd" {
		return "", false, nil
	}
	if len(fields) > 2 {
		return "", true, fmt.Errorf("usage: cd [dir]")
	}
	if len(fields) == 1 {
		return "", true, nil
	}
	return fields[1], true, nil
}

func parseExitCommand(line string) (bool, bool, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed != "exit" && !strings.HasPrefix(trimmed, "exit ") && !strings.HasPrefix(trimmed, "exit\t") {
		return false, false, nil
	}
	fields, err := splitShellFields(line)
	if err != nil || len(fields) == 0 || fields[0] != "exit" {
		return false, false, err
	}
	if len(fields) == 1 {
		return true, false, nil
	}
	if len(fields) == 2 && fields[1] == "--force" {
		return true, true, nil
	}
	return true, false, fmt.Errorf("usage: exit [--force]")
}

func splitShellFields(input string) ([]string, error) {
	tokens, err := lexShellTokens(input)
	if err != nil {
		return nil, err
	}
	fields := make([]string, 0, len(tokens))
	for _, token := range tokens {
		fields = append(fields, token.Value)
	}
	return fields, nil
}

func lexShellTokens(input string) ([]shellToken, error) {
	var b strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	haveField := false
	fieldStart := 0
	var tokens []shellToken
	for i, r := range input {
		switch {
		case escaped:
			b.WriteRune(r)
			haveField = true
			escaped = false
		case r == '\\' && !inSingle:
			if !haveField {
				fieldStart = i
			}
			escaped = true
			haveField = true
		case r == '\'' && !inDouble:
			if !haveField {
				fieldStart = i
			}
			inSingle = !inSingle
			haveField = true
		case r == '"' && !inSingle:
			if !haveField {
				fieldStart = i
			}
			inDouble = !inDouble
			haveField = true
		case (r == ' ' || r == '\t' || r == '\n') && !inSingle && !inDouble:
			if haveField {
				tokens = append(tokens, shellToken{Value: b.String(), Start: fieldStart, End: i})
				b.Reset()
				haveField = false
			}
		default:
			if !haveField {
				fieldStart = i
			}
			b.WriteRune(r)
			haveField = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("unfinished escape")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote")
	}
	if haveField {
		tokens = append(tokens, shellToken{Value: b.String(), Start: fieldStart, End: len(input)})
	}
	return tokens, nil
}

func hostShell() string {
	if runtime.GOOS == "windows" {
		if shell := strings.TrimSpace(os.Getenv("COMSPEC")); shell != "" {
			return shell
		}
		return "cmd.exe"
	}
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "/bin/sh"
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		code := exitErr.ExitCode()
		if code >= 0 {
			return code
		}
	}
	return -1
}

func parsePortForwardSpec(spec string) (client.PortForward, error) {
	parts := strings.Split(spec, ":")
	if len(parts) != 2 {
		return client.PortForward{}, fmt.Errorf("port forward must be HOST_PORT:GUEST_PORT")
	}
	hostPort, err := parseTCPPort(parts[0], "host")
	if err != nil {
		return client.PortForward{}, err
	}
	guestPort, err := parseTCPPort(parts[1], "guest")
	if err != nil {
		return client.PortForward{}, err
	}
	return client.PortForward{
		Protocol:  "tcp",
		HostAddr:  "127.0.0.1",
		HostPort:  hostPort,
		GuestPort: guestPort,
	}, nil
}

func parseTCPPort(value, label string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("%s port is required", label)
	}
	port, err := net.LookupPort("tcp", value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s port %q", label, value)
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%s port %d out of range", label, port)
	}
	return port, nil
}

func formatDetailedProgressEvent(event client.ProgressEvent, fallbackArtifact string) string {
	artifact := firstNonEmpty(event.Artifact, fallbackArtifact)
	if artifact == "" && event.Status == "" && event.Blob == "" && event.Error == "" {
		return ""
	}
	title := "Pull"
	if artifact != "" {
		title += " " + artifact
	}
	lines := []string{title}
	lines = append(lines, "  status: "+firstNonEmpty(event.Status, "preparing"))
	if event.Artifact != "" && event.Artifact != artifact {
		lines = append(lines, "  artifact: "+event.Artifact)
	}
	if event.Blob != "" {
		lines = append(lines, "  blob: "+event.Blob)
	}
	if event.Progress > 0 {
		lines = append(lines, fmt.Sprintf("  progress: %.1f%%", event.Progress*100))
	}
	if event.BytesDownloaded > 0 || event.BytesTotal > 0 {
		value := formatByteSize(event.BytesDownloaded)
		if event.BytesTotal > 0 {
			value += " / " + formatByteSize(event.BytesTotal)
			if event.BytesDownloaded > 0 {
				value += fmt.Sprintf(" (%.1f%%)", float64(event.BytesDownloaded)*100/float64(event.BytesTotal))
			}
		}
		lines = append(lines, "  bytes: "+value)
	}
	if event.FilesDownloaded > 0 || event.FilesTotal > 0 {
		value := strconv.FormatInt(event.FilesDownloaded, 10)
		if event.FilesTotal > 0 {
			value += " / " + strconv.FormatInt(event.FilesTotal, 10)
			if event.FilesDownloaded > 0 {
				value += fmt.Sprintf(" (%.1f%%)", float64(event.FilesDownloaded)*100/float64(event.FilesTotal))
			}
		}
		lines = append(lines, "  files: "+value)
	}
	if event.RateBytesPerSecond > 0 {
		lines = append(lines, "  rate: "+formatByteSize(int64(event.RateBytesPerSecond))+"/s")
	}
	if event.ETASeconds > 0 {
		lines = append(lines, "  eta: "+formatDurationSeconds(event.ETASeconds))
	}
	if event.Error != "" {
		lines = append(lines, "  error: "+event.Error)
	}
	return strings.Join(lines, "\n")
}

func formatByteSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f EB", value/unit)
}

func formatDurationSeconds(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	return time.Duration(seconds * float64(time.Second)).Round(time.Second).String()
}

func formatBootEvent(event client.BootEvent) string {
	switch event.Kind {
	case "status":
		if event.Message != "" {
			return "Boot: " + event.Message
		}
	case "ready":
		if event.State.Image != "" {
			return "Boot: ready " + event.State.Image
		}
		return "Boot: ready"
	case "error":
		if event.Error != "" {
			return "Boot error: " + event.Error
		}
		return "Boot error"
	}
	return ""
}

func resolveCacheDir(arg string) (string, error) {
	if arg != "" {
		return arg, os.MkdirAll(arg, 0o755)
	}
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	dir := filepath.Join(userCacheDir, "ccx3")
	return dir, os.MkdirAll(dir, 0o755)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func emptyText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func sortedMapKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func readerIsTerminal(r io.Reader) bool {
	file, ok := r.(*os.File)
	return ok && terminal.IsTerminalFD(int(file.Fd()))
}

func writerIsTerminal(w io.Writer) bool {
	file, ok := terminalWriterFile(w)
	return ok && terminal.IsTerminalFD(int(file.Fd()))
}
