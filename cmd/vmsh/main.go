package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
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
	"j5.nz/cc/client"
)

const guestHostMount = "/host"
const defaultGuestUser = "1000:1000"
const defaultVMSHBootTimeoutSeconds = 60
const defaultGuestShellReadyTimeout = 5 * time.Second
const internalCCVMEnv = "VMSH_INTERNAL_CCVM"
const internalCCVMSidecarModeEnv = "CCX3_CCVM_SIDECAR_MODE"
const internalCCVMSidecarMode = "vmsh-internal"

const (
	colorReset   = "\x1b[0m"
	colorGreen   = "\x1b[32m"
	colorCyan    = "\x1b[36m"
	colorBlue    = "\x1b[34m"
	colorMagenta = "\x1b[35m"
	colorYellow  = "\x1b[33m"
)

type daemonState struct {
	Addr string `json:"addr"`
}

type ccvmLaunch struct {
	Path string
	Args []string
	Env  []string
}

type shellMode string

const (
	modeHost shellMode = "host"
	modeVM   shellMode = "vm"
)

type shellState struct {
	api              vmshAPI
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
	lastCode         int
	promptOut        io.Writer
	history          string
	env              map[string]string
	aliases          map[string]string
	confirmPull      func(string, io.Writer) (bool, error)
	confirmVMRestart func(string, io.Writer) (bool, error)
	jobs             []shellJob
	nextJobID        int
	jobsMu           sync.Mutex
	statusSeq        atomic.Uint64
	completion       *vmshCompleter
	tmuxExec         func([]string) error
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
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	seq     uint64
	lastCWD string
	pending string
	done    chan error
}

type persistentGuestShell struct {
	mu      sync.Mutex
	key     string
	inputs  chan client.ExecInput
	events  chan client.ExecEvent
	done    chan error
	lastCWD string
	pending string
}

type vmshCompleter struct {
	shell *shellState
}

type completionKind string

const (
	completionNone    completionKind = ""
	completionAt      completionKind = "at"
	completionOption  completionKind = "option"
	completionCommand completionKind = "command"
	completionPath    completionKind = "path"
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
	prefix := string(line[:pos])
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
		candidates = suffixCompletions(vmshOptionWords(), token)
		return candidates, len([]rune(token)), completionOption
	}
	if c.shouldCompleteRMIImage(prefix, tokenStart) {
		candidates = suffixCompletions(c.cachedImageNames(), token)
		return candidates, len([]rune(typedToken)), completionAt
	}
	if c.shouldCompleteCommand(prefix, tokenStart, isFirstToken, token) {
		candidates = c.commandCandidates(token)
		return candidates, len([]rune(typedToken)), completionCommand
	}
	if !isFirstToken || token == "" || strings.Contains(token, "/") || token == "." || token == ".." || strings.HasPrefix(token, "~") {
		candidates = c.pathCandidates(token, completionCtx)
		return candidates, pathCompletionReplaceLen(typedToken), completionPath
	}
	return nil, 0, completionNone
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
			ctx.Mode = modeVM
			ctx.User = "root"
		}
	case "host":
		ctx = ctx.withOptions(at.Options)
		ctx.Mode = modeHost
	case "help", "ps", "jobs", "alias", "status", "where", "start", "stop", "restart", "forward", "tmux":
	default:
		ctx = ctx.withOptions(at.Options)
		ctx.Mode = modeVM
		ctx.Image = at.Target
	}
	return ctx
}

func pathCompletionReplaceLen(token string) int {
	if token == "" {
		return 0
	}
	return len([]rune(filepath.Base(token)))
}

func (c *vmshCompleter) atTargetWords() []string {
	words := []string{"@alias", "@help", "@host", "@jobs", "@ps", "@restart", "@status", "@start", "@stop", "@forward", "@rmi", "@sudo", "@tmux"}
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

func vmshOptionWords() []string {
	return []string{
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
	}
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

func (c *vmshCompleter) commandCandidates(token string) []string {
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
	return c.hostPathCandidates(token, ctx)
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
	status, err := c.shell.api.InstanceStatusOf(ctx.VMID)
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
	err = c.shell.api.RunStreamInContext(runCtx, ctx.VMID, req, func(event client.ExecEvent) error {
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

type vmshAPI interface {
	HealthCheck() error
	Capabilities() (client.CapabilitiesResponse, error)
	GetImage(string) (client.ImageState, error)
	PullImageStream(string, client.PullImageRequest, func(client.ProgressEvent) error) error
	DeleteImage(string) error
	SaveInstanceImage(string, client.SaveImageRequest) (client.ImageState, error)
	StartInstanceStreamWithID(string, client.StartInstanceRequest, func(client.BootEvent) error) (client.InstanceState, error)
	ShutdownInstanceWithID(string) error
	InstanceStatusOf(string) (client.InstanceState, error)
	InstanceStatuses() ([]client.InstanceState, error)
	AddPortForwardTo(string, client.PortForward) error
	CreateWatchdogLease(client.WatchdogLeaseRequest) (client.WatchdogLeaseResponse, error)
	FeedWatchdogLease(string) error
	ReleaseWatchdogLease(string) error
	RunStreamIn(string, client.RunRequest, func(client.ExecEvent) error) error
	RunStreamInContext(context.Context, string, client.RunRequest, func(client.ExecEvent) error) error
	RunInteractiveStreamIn(string, client.RunRequest, <-chan client.ExecInput, func(client.ExecEvent) error) error
}

type commandContext struct {
	Mode       shellMode `json:"mode"`
	Image      string    `json:"image,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	VMID       string    `json:"vm,omitempty"`
	CWD        string    `json:"cwd,omitempty"`
	User       string    `json:"user,omitempty"`
	MemoryMB   uint64    `json:"memory_mb,omitempty"`
	CPUs       int       `json:"cpus,omitempty"`
	Network    bool      `json:"network,omitempty"`
	NestedVirt bool      `json:"nested_virtualization,omitempty"`
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
	MemoryMB     uint64
	CPUs         int
	Network      *bool
	NestedVirt   *bool
	OptionFields []string
}

type shellToken struct {
	Value string
	Start int
	End   int
}

func main() {
	if runInternalCCVMFromEnv() {
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "vmsh:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	ccvmPath := fs.String("ccvm", "", "Path to ccvm binary")
	cacheDir := fs.String("cache-dir", "", "Cache directory")
	image := fs.String("image", "", "Initial image for VM commands")
	vmID := fs.String("vm", "default", "Initial VM id")
	startVM := fs.Bool("start", false, "Start the selected blank VM before entering the shell")
	script := fs.String("script", "", "Internal test hook: read vmsh commands from this file")
	if err := fs.Parse(os.Args[1:]); err != nil {
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
	ccvmLaunch, err := resolveCCVMPath(*ccvmPath)
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
	api, err := connectBackend(ccvmLaunch, rootCache, statePath)
	if err != nil {
		return err
	}
	caps, _ := api.Capabilities()
	stopLease, err := startDaemonLease(api)
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
		promptOut:  os.Stdout,
		history:    filepath.Join(rootCache, "vmsh_history"),
		env:        map[string]string{},
		aliases:    map[string]string{},
		confirmPull: func(source string, stderr io.Writer) (bool, error) {
			return promptPullConfirmation(os.Stdin, stderr, source)
		},
		confirmVMRestart: func(id string, stderr io.Writer) (bool, error) {
			return promptVMRestartConfirmation(os.Stdin, stderr, id)
		},
	}
	sh.completion = newVMSHCompleter(sh)
	defer sh.closeSessions()
	if err := sh.loadVMSHRC(defaultVMSHRCPath()); err != nil {
		return err
	}
	if *startVM {
		if err := sh.startVM(sh.context.VMID, sh.context, os.Stderr); err != nil {
			return err
		}
	}
	if *script != "" {
		f, err := os.Open(*script)
		if err != nil {
			return err
		}
		defer f.Close()
		return sh.runScript(f, os.Stdout, os.Stderr)
	}
	return sh.loop(os.Stdin, os.Stdout, os.Stderr)
}

func defaultContext(vmID, image string, nestedVirt bool) commandContext {
	return commandContext{
		Mode:       modeHost,
		VMID:       firstNonEmpty(vmID, "default"),
		Image:      image,
		Network:    true,
		NestedVirt: nestedVirt,
	}
}

func (s *shellState) loop(in io.Reader, stdout, stderr io.Writer) error {
	if !readerIsTerminal(in) || !writerIsTerminal(stdout) {
		return fmt.Errorf("vmsh requires an interactive terminal")
	}
	if outFile, ok := stdout.(*os.File); ok {
		restoreOutput := prepareTerminalOutput(outFile)
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
	editor := newLineEditor(in, stdout, s.history, s.completion)
	for {
		s.drawPromptStatus(stdout)
		line, err := editor.ReadLine(s.prompt())
		s.statusSeq.Add(1)
		switch {
		case errors.Is(err, errLineInterrupted):
			continue
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		if err := s.eval(line, stdout, stderr); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
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
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if err := s.eval(line, stdout, stderr); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			s.lastCode = 1
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
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
		return s.runPipeline(s.context, segments, stdout, stderr)
	}
	if isExitCommand(line) {
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
	rest := strings.TrimLeft(line[first.End:], " \t")
	if rest == "" {
		return replacement, true, nil
	}
	return strings.TrimRight(replacement, " \t") + " " + rest, true, nil
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
	switch ctx.Mode {
	case modeHost:
		return s.runHost(line, stdout, stderr)
	case modeVM:
		return s.runGuest(ctx, line, stdout, stderr)
	default:
		return fmt.Errorf("unknown shell mode %q", ctx.Mode)
	}
}

func (s *shellState) runMaybeBackground(ctx commandContext, line string, stdout, stderr io.Writer) error {
	if command, ok, err := stripBackground(line); ok || err != nil {
		if err != nil {
			return err
		}
		return s.startBackgroundJob(ctx, command, stdout, stderr)
	}
	if segments, ok, err := splitPipelineLine(line); ok || err != nil {
		if err != nil {
			return err
		}
		return s.runPipeline(ctx, segments, stdout, stderr)
	}
	return s.runInContext(ctx, line, stdout, stderr)
}

type pipelineStage struct {
	ctx  commandContext
	line string
	req  client.RunRequest
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
			ctx.Mode = modeHost
		case "sudo":
			if line == "" {
				return pipelineStage{}, fmt.Errorf("usage: @sudo <cmd>")
			}
			ctx, line = sudoCommandContext(ctx, line)
		default:
			if isControlAtTarget(at.Target) {
				return pipelineStage{}, fmt.Errorf("@%s cannot be used in a pipeline", at.Target)
			}
			ctx.Mode = modeVM
			ctx.Image = at.Target
			if at.Options.Sudo {
				ctx.User = "root"
			}
		}
		if line == "" {
			return pipelineStage{}, fmt.Errorf("pipeline segment %q has no command", segment)
		}
	}
	stage := pipelineStage{ctx: ctx, line: line}
	if ctx.Mode == modeVM {
		req, err := s.prepareGuestRunRequest(ctx, line, false, 0, 0, stderr)
		if err != nil {
			return pipelineStage{}, err
		}
		stage.req = req
	}
	return stage, nil
}

func isControlAtTarget(target string) bool {
	switch target {
	case "help", "?", "ps", "jobs", "alias", "status", "where", "start", "stop", "restart", "save", "rmi", "tmux", "forward":
		return true
	default:
		return false
	}
}

func (s *shellState) runPipelineStage(stage pipelineStage, stdin io.Reader, stdout, stderr io.Writer) error {
	switch stage.ctx.Mode {
	case modeHost:
		return s.runHostWithInput(stage.line, stdin, stdout, stderr)
	case modeVM:
		return s.streamGuestRunWithInput(stage.ctx.VMID, stage.req, stdin, stdout, stderr)
	default:
		return fmt.Errorf("unknown shell mode %q", stage.ctx.Mode)
	}
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
			s.context = ctx
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
		ctx := s.context.withOptions(at.Options)
		ctx.Mode = modeHost
		if at.Options.Sudo {
			return fmt.Errorf("usage: @host [cmd]")
		}
		if at.Command == "" {
			s.context = ctx
			return nil
		}
		return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
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
			return fmt.Errorf("usage: @sudo <cmd>")
		}
		ctx, command := sudoCommandContext(ctx, at.Command)
		return s.runMaybeBackground(ctx, command, stdout, stderr)
	case "start":
		if at.Command != "" {
			return fmt.Errorf("usage: @start [--vm id]")
		}
		ctx := s.context.withOptions(at.Options)
		id := firstNonEmpty(ctx.VMID, s.context.VMID)
		return s.startVM(id, ctx, stderr)
	case "stop":
		if at.Command != "" {
			return fmt.Errorf("usage: @stop [--vm id]")
		}
		id := firstNonEmpty(at.Options.VMID, s.context.VMID)
		return s.stopVM(id)
	case "restart":
		if at.Command != "" {
			return fmt.Errorf("usage: @restart [--vm id]")
		}
		ctx := s.context.withOptions(at.Options)
		id := firstNonEmpty(ctx.VMID, s.context.VMID)
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
		id := firstNonEmpty(at.Options.VMID, s.context.VMID)
		return s.api.AddPortForwardTo(id, forward)
	default:
		ctx := s.context.withOptions(at.Options)
		ctx.Mode = modeVM
		ctx.Image = at.Target
		if at.Options.Sudo {
			ctx.User = "root"
		}
		if at.Command == "" {
			if at.Options.Sudo {
				return fmt.Errorf("usage: @%s --sudo <cmd>", at.Target)
			}
			if err := s.ensureImageAvailable(ctx, stderr); err != nil {
				return err
			}
			s.context = ctx
			return nil
		}
		return s.runMaybeBackground(ctx, at.Command, stdout, stderr)
	}
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
			err = session.run(line, stdout, stderr)
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
	err := cmd.Run()
	s.lastCode = exitCode(err)
	if err != nil && s.lastCode < 0 {
		return err
	}
	return nil
}

func (s *shellState) runHostWithInput(line string, stdin io.Reader, stdout, stderr io.Writer) error {
	args := hostShellCommand(line, false, s.hostCommandPrelude(false))
	cmd := exec.Command(args[0], args[1:]...)
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
	code := exitCode(err)
	if err != nil && code < 0 {
		return err
	}
	if code != 0 {
		return persistentShellExit{code: code}
	}
	return nil
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
	fields, err := splitShellFields(line)
	if err != nil || len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "vi", "vim", "nvim", "nano", "emacs", "less", "more", "man", "ssh", "top", "htop", "watch", "fg", "bg", "jobs":
		return false
	case "cat", "python", "python3", "node", "ruby", "irb", "php", "sqlite3", "mysql", "psql":
		return len(fields) > 1
	default:
		return true
	}
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
	cmd := exec.Command(hostShell(), "-lc", script)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
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
		return nil, err
	}
	session := &persistentHostShell{
		cmd:     cmd,
		stdin:   tty,
		stdout:  bufio.NewReader(tty),
		lastCWD: cwd,
		done:    make(chan error, 1),
	}
	go func() {
		session.done <- cmd.Wait()
	}()
	if err := session.waitReady(); err != nil {
		session.close()
		return nil, err
	}
	return session, nil
}

func persistentHostShellScript() string {
	lines := []string{
		"set +m 2>/dev/null || true",
		"stty -echo 2>/dev/null || true",
	}
	if filepath.Base(hostShell()) == "bash" {
		lines = append(lines, bashHostShellOptionsPrelude())
	}
	lines = append(lines, []string{
		"printf '__VMSH_READY__:%s\\n' \"$PWD\"",
		"while IFS= read -r __vmsh_line; do",
		"  stty echo 2>/dev/null || true",
		"  eval \"$__vmsh_line\"",
		"  __vmsh_status=$?",
		"  stty -echo 2>/dev/null || true",
		"  printf '__VMSH_DONE__:%s:%s\\n' \"$__vmsh_status\" \"$PWD\"",
		"done",
	}...)
	return strings.Join(lines, "\n")
}

func (p *persistentHostShell) waitReady() error {
	for {
		text, err := p.stdout.ReadString('\n')
		if text != "" {
			if cwd, ok := parsePersistentReady(text); ok {
				p.lastCWD = cwd
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
}

func (p *persistentHostShell) run(line string, stdout, stderr io.Writer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := fmt.Fprintln(p.stdin, line); err != nil {
		return err
	}
	for {
		text, err := p.stdout.ReadString('\n')
		if text != "" {
			if before, code, cwd, ok := p.consumeOutput(text); ok {
				if before != "" {
					_, _ = io.WriteString(stdout, before)
				}
				p.lastCWD = cwd
				if code != 0 {
					return persistentShellExit{code: code}
				}
				return nil
			}
			_, _ = io.WriteString(stdout, text)
		}
		if err != nil {
			return err
		}
	}
}

func (p *persistentHostShell) consumeOutput(text string) (string, int, string, bool) {
	p.pending += text
	idx := strings.Index(p.pending, "__VMSH_DONE__:")
	if idx < 0 {
		out := p.pending
		p.pending = ""
		return out, 0, "", false
	}
	newline := strings.IndexAny(p.pending[idx:], "\r\n")
	if newline < 0 {
		if idx > 0 {
			out := p.pending[:idx]
			p.pending = p.pending[idx:]
			return out, 0, "", false
		}
		return "", 0, "", false
	}
	lineEnd := idx + newline
	before := p.pending[:idx]
	marker := p.pending[idx:lineEnd]
	p.pending = strings.TrimLeft(p.pending[lineEnd:], "\r\n")
	code, cwd, ok := parsePersistentMarker(marker)
	return before, code, cwd, ok
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
}

func (p *persistentHostShell) close() {
	if p.stdin != nil {
		_ = p.stdin.Close()
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
		if err != nil || strings.TrimSpace(prelude) == "" {
			s.hostInit.prelude = hostShellHookPrelude()
			s.hostInit.fallback = true
			return
		}
		s.hostInit.prelude = prelude
	})
	return s.hostInit.prelude
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
	return "cc"
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
		err = session.run(line, stdout, stderr, func() (func(), error) {
			return s.startGuestInputForwarding(req.TTY, false, session.inputs, stdout, stderr, func(name string) {
				if name == "INT" {
					interrupted.Store(true)
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
		}
		if err == nil || s.lastCode >= 0 {
			return nil
		}
		return err
	}
	return s.streamGuestRun(ctx.VMID, req, stdout, stderr)
}

func (s *shellState) prepareGuestRunRequest(ctx commandContext, line string, tty bool, cols, rows int, stderr io.Writer) (client.RunRequest, error) {
	if ctx.Image == "" {
		return client.RunRequest{}, fmt.Errorf("no guest image selected; run @<oci-tag> or set one with @<oci-tag>")
	}
	if err := s.ensureImageAvailable(ctx, stderr); err != nil {
		return client.RunRequest{}, err
	}
	if err := s.ensureVMRunning(ctx, stderr); err != nil {
		return client.RunRequest{}, err
	}
	hostRoot, hostGuestCWD, err := guestHostPaths(s.hostCWD)
	if err != nil {
		return client.RunRequest{}, err
	}
	workDir := firstNonEmpty(ctx.CWD, hostGuestCWD)
	req := client.RunRequest{
		Image:   localImageName(ctx.Image, ctx.Arch),
		Command: guestCommand(line, tty),
		Shares: []client.ShareMount{{
			Source:   hostRoot,
			Mount:    guestHostMount,
			Writable: true,
			MapOwner: true,
			OwnerUID: defaultGuestUID,
			OwnerGID: defaultGuestGID,
			Cache:    "strict",
		}},
		WorkDir:    workDir,
		User:       guestRunUser(ctx),
		MemoryMB:   ctx.MemoryMB,
		CPUs:       ctx.CPUs,
		NestedVirt: ctx.NestedVirt,
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
		req.Network = defaultNetworkConfig()
	}
	return req, nil
}

func persistentGuestCommandAllowed(line string) bool {
	return persistentShellCommandAllowed(line)
}

func (s *shellState) guestPersistentShell(ctx commandContext, req client.RunRequest) (*persistentGuestShell, error) {
	key := strings.Join([]string{ctx.VMID, localImageName(ctx.Image, ctx.Arch), req.User}, "\x00")
	if s.guestShell != nil && s.guestShell.key == key {
		return s.guestShell, nil
	}
	if s.guestShell != nil {
		s.guestShell.close()
		s.guestShell = nil
	}
	req.Command = guestPersistentCommand()
	req.TTY = true
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
		err := s.api.RunInteractiveStreamIn(ctx.VMID, req, inputs, func(event client.ExecEvent) error {
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

func guestPersistentCommand() []string {
	return []string{"sh", "-lc", guestShellPrelude() + strings.Join([]string{
		"stty -echo 2>/dev/null || true",
		colorPrelude("ls --color=always -C -w ${COLUMNS:-80}", "ls -G -C", false),
		"printf '__VMSH_READY__:%s\\n' \"$PWD\"",
		"while IFS= read -r __vmsh_line; do",
		"  stty echo 2>/dev/null || true",
		"  eval \"$__vmsh_line\"",
		"  __vmsh_status=$?",
		"  stty -echo 2>/dev/null || true",
		"  printf '__VMSH_DONE__:%s:%s\\n' \"$__vmsh_status\" \"$PWD\"",
		"done",
	}, "\n")}
}

func (p *persistentGuestShell) waitReady() error {
	timer := time.NewTimer(defaultGuestShellReadyTimeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-p.events:
			if !ok {
				return fmt.Errorf("persistent guest shell closed before ready")
			}
			switch event.Kind {
			case "stdout", "output":
				text := execEventText(event)
				if cwd, ok := parsePersistentReady(text); ok {
					p.lastCWD = cwd
					return nil
				}
			case "exit":
				return fmt.Errorf("persistent guest shell exited before ready")
			case "error":
				if event.Error != "" {
					return fmt.Errorf("%s", event.Error)
				}
				return fmt.Errorf("persistent guest shell failed before ready")
			}
		case err := <-p.done:
			if err != nil {
				return err
			}
			return fmt.Errorf("persistent guest shell exited before ready")
		case <-timer.C:
			return fmt.Errorf("persistent guest shell did not become ready")
		}
	}
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
	p.inputs <- client.ExecInput{Kind: "stdin", Data: []byte(line + "\n")}
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
		case "stdout", "output":
			text := execEventText(event)
			if before, code, cwd, ok := p.consumeOutput(text); ok {
				if before != "" {
					_, _ = io.WriteString(stdout, before)
				}
				p.lastCWD = cwd
				if code != 0 {
					return persistentShellExit{code: code}
				}
				return nil
			}
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

func (p *persistentGuestShell) consumeOutput(text string) (string, int, string, bool) {
	p.pending += text
	idx := strings.Index(p.pending, "__VMSH_DONE__:")
	if idx < 0 {
		out := p.pending
		p.pending = ""
		return out, 0, "", false
	}
	newline := strings.IndexAny(p.pending[idx:], "\r\n")
	if newline < 0 {
		if idx > 0 {
			out := p.pending[:idx]
			p.pending = p.pending[idx:]
			return out, 0, "", false
		}
		return "", 0, "", false
	}
	lineEnd := idx + newline
	before := p.pending[:idx]
	marker := p.pending[idx:lineEnd]
	p.pending = strings.TrimLeft(p.pending[lineEnd:], "\r\n")
	code, cwd, ok := parsePersistentMarker(marker)
	return before, code, cwd, ok
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
		if err := s.api.RunStreamInContext(context.Background(), id, req, func(event client.ExecEvent) error {
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
			s.lastCode = 1
			return err
		}
		s.lastCode = exitCode
		return nil
	}

	inputs := make(chan client.ExecInput, 8)
	stopForwarding, err := s.startGuestInputForwarding(req.TTY, true, inputs, stdout, stderr)
	if err != nil {
		return err
	}
	defer func() {
		stopForwarding()
		close(inputs)
	}()

	exitCode := 0
	if err := s.api.RunInteractiveStreamIn(id, req, inputs, func(event client.ExecEvent) error {
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
		s.lastCode = 1
		return err
	}
	s.lastCode = exitCode
	return nil
}

func (s *shellState) streamGuestRunWithInput(id string, req client.RunRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	req.TTY = false
	req.Cols = 0
	req.Rows = 0
	if stdin == nil {
		exitCode := 0
		if err := s.api.RunStreamInContext(context.Background(), id, req, func(event client.ExecEvent) error {
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
			return err
		}
		if exitCode != 0 {
			return persistentShellExit{code: exitCode}
		}
		return nil
	}

	inputs := make(chan client.ExecInput, 8)
	inputErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		inputErr <- streamReaderToGuestInput(stdin, inputs, done)
		close(inputs)
	}()
	exitCode := 0
	err := s.api.RunInteractiveStreamIn(id, req, inputs, func(event client.ExecEvent) error {
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
	close(done)
	if inErr := <-inputErr; err == nil && inErr != nil {
		err = inErr
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
				_ = sendGuestInputBlocking(out, done, client.ExecInput{Kind: "stdin_close"})
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
		file, ok := stdout.(*os.File)
		if ok && isTerminalFD(int(file.Fd())) && isTerminalFD(int(os.Stdin.Fd())) {
			terminalRestore, err := makeRawTerminal(os.Stdin)
			if err != nil {
				return nil, err
			}
			restore = terminalRestore
			cancelRead = func() { interruptTerminalRead(os.Stdin) }
			producers.Add(1)
			go func() {
				defer producers.Done()
				streamGuestStdin(os.Stdin, inputs, done)
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

func streamGuestStdin(file *os.File, out chan<- client.ExecInput, done <-chan struct{}) {
	var buf [4096]byte
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := file.Read(buf[:])
		if n > 0 {
			sendGuestInput(out, done, client.ExecInput{Kind: "stdin", Data: append([]byte(nil), buf[:n]...)})
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				sleepOrDone(done, 10*time.Millisecond)
				continue
			}
			if errors.Is(err, io.EOF) {
				sendGuestInput(out, done, client.ExecInput{Kind: "stdin_close"})
			}
			return
		}
	}
}

func forwardGuestSignals(out chan<- client.ExecInput, done <-chan struct{}, tty bool, stdout, stderr io.Writer, onSignal ...func(string)) {
	signals := hostSignals(tty)
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
			if isResizeSignal(sig) {
				file, ok := stdout.(*os.File)
				if !ok {
					continue
				}
				cols, rows, err := terminalSize(file)
				if err != nil {
					continue
				}
				sendGuestInput(out, done, client.ExecInput{Kind: "resize", Cols: cols, Rows: rows})
				continue
			}
			name, ok := signalName(sig)
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
	if in == nil || !isTerminalFD(int(in.Fd())) {
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
	if in == nil || !isTerminalFD(int(in.Fd())) {
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

func displayPullSource(source string) string {
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

func (s *shellState) ensureImageAvailable(ctx commandContext, stderr io.Writer) error {
	image := localImageName(ctx.Image, ctx.Arch)
	if s.imageCache != nil && s.imageCache[image] {
		return nil
	}
	if _, err := s.api.GetImage(image); err == nil {
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
	if err := s.api.PullImageStream(image, client.PullImageRequest{Source: ctx.Image, Architecture: ctx.Arch}, report); err != nil {
		return err
	}
	if s.imageCache == nil {
		s.imageCache = map[string]bool{}
	}
	s.imageCache[image] = true
	return nil
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
	id := ctx.VMID
	if s.vmRunning != nil && s.vmRunning[id] {
		return nil
	}
	state, err := s.api.InstanceStatusOf(id)
	if err != nil {
		return err
	}
	if state.Status == "running" {
		if s.vmRunning == nil {
			s.vmRunning = map[string]bool{}
		}
		s.vmRunning[id] = true
		return nil
	}
	return s.startVM(id, ctx, stderr)
}

func (s *shellState) startVM(id string, ctx commandContext, stderr io.Writer) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("vm id is required")
	}
	req := client.StartInstanceRequest{
		Image:          localImageName(ctx.Image, ctx.Arch),
		MemoryMB:       ctx.MemoryMB,
		CPUs:           ctx.CPUs,
		NestedVirt:     ctx.NestedVirt,
		TimeoutSeconds: vmshBootTimeoutSeconds(),
	}
	if ctx.Network {
		req.Network = defaultNetworkConfig()
	}
	boot := newBootStatus(stderr)
	defer boot.Close()
	state, err := s.api.StartInstanceStreamWithID(id, req, func(event client.BootEvent) error {
		boot.Update(event)
		return nil
	})
	if err != nil {
		return err
	}
	s.context.VMID = firstNonEmpty(state.ID, id)
	if s.vmRunning == nil {
		s.vmRunning = map[string]bool{}
	}
	s.vmRunning[s.context.VMID] = true
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
	if file, ok := w.(*os.File); ok && isTerminalFD(int(file.Fd())) {
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

func (s *shellState) stopVM(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("vm id is required")
	}
	if err := s.api.ShutdownInstanceWithID(id); err != nil {
		return err
	}
	delete(s.vmRunning, id)
	return nil
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
	if s.guestShell != nil {
		s.guestShell.close()
		s.guestShell = nil
	}
	if err := s.stopVM(id); err != nil {
		return err
	}
	ctx.VMID = id
	return s.startVM(id, ctx, stderr)
}

func restartContextFromState(ctx commandContext, state client.InstanceState) commandContext {
	if strings.TrimSpace(ctx.Image) == "" {
		ctx.Image = state.Image
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
	id := firstNonEmpty(at.Options.VMID, s.context.VMID)
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("vm id is required")
	}
	state, err := s.api.SaveInstanceImage(id, client.SaveImageRequest{
		Name:  name,
		Image: localImageName(s.context.Image, s.context.Arch),
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
	if s.context.Mode == modeVM {
		return s.chdirGuest(target)
	}
	return s.chdirHost(target)
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
		if err := s.hostShell.run("cd "+shellQuote(target), io.Discard, io.Discard); err != nil {
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
	if s.guestShell != nil {
		s.guestShell.close()
		s.guestShell = nil
	}
	current := s.context.CWD
	if current == "" {
		_, current, _ = guestHostPaths(s.hostCWD)
	}
	home := guestHomeDir(s.context)
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
		hostPath, ok := guestHostPathToHost(s.hostCWD, target)
		if !ok {
			return fmt.Errorf("cannot map guest host path %q", target)
		}
		if err := s.chdirHost(hostPath); err != nil {
			return err
		}
		s.context.CWD = ""
		return nil
	}
	s.context.CWD = target
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
	if s.context.Mode == modeVM && s.context.CWD != "" {
		promptCWD = s.context.CWD
	}
	leaf := filepath.Base(promptCWD)
	if leaf == "." || leaf == string(filepath.Separator) {
		leaf = promptCWD
	}
	base := colorGreen + "➜" + colorReset + "  " + colorCyan + leaf + colorReset
	if s.context.Mode == modeVM {
		target := "(" + contextImageText(s.context)
		if s.context.VMID != "" && s.context.VMID != "default" {
			target += ":" + s.context.VMID
		}
		target += ")"
		return base + " " + colorMagenta + "vm:" + colorReset + colorYellow + target + colorReset + " "
	}
	return base + " " + colorBlue + "host" + colorReset + " "
}

func contextImageText(ctx commandContext) string {
	if ctx.Image == "" || ctx.Arch == "" {
		return ctx.Image
	}
	return ctx.Image + "@" + ctx.Arch
}

func terminalRequestSize(stdout io.Writer) (bool, int, int) {
	file, ok := stdout.(*os.File)
	if !ok || !isTerminalFD(int(file.Fd())) {
		return false, 0, 0
	}
	cols, rows, err := terminalSize(file)
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

func (s *shellState) drawPromptStatus(stdout io.Writer) {
	seq := s.statusSeq.Add(1)
	code := s.lastCode
	if code == 0 {
		return
	}
	file, ok := stdout.(*os.File)
	if !ok || !isTerminalFD(int(file.Fd())) {
		return
	}
	cols, _, err := terminalSize(file)
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
	state, err := s.api.InstanceStatusOf(s.context.VMID)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "context: %s\nimage: %s\nvm: %s\nhost cwd: %s\nvm status: %s\n",
		s.context.Mode,
		emptyText(contextImageText(s.context), "-"),
		emptyText(s.context.VMID, "-"),
		s.hostCWD,
		emptyText(state.Status, "unknown"),
	)
	if state.NetworkIPv4 != "" {
		_, err = fmt.Fprintf(w, "vm address: %s\n", state.NetworkIPv4)
	}
	return err
}

func (s *shellState) printVMs(w io.Writer) error {
	states, err := s.api.InstanceStatuses()
	if err != nil {
		return err
	}
	if len(states) == 0 {
		_, err = fmt.Fprintln(w, "No VMs")
		return err
	}
	for _, state := range states {
		parts := []string{emptyText(state.ID, "default"), emptyText(state.Status, "unknown")}
		if state.Image != "" {
			parts = append(parts, "image="+state.Image)
		}
		if state.NetworkIPv4 != "" {
			parts = append(parts, "addr="+state.NetworkIPv4)
		}
		if _, err := fmt.Fprintln(w, strings.Join(parts, " ")); err != nil {
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
@ [opts] [cmd]           update or use the current context
@sudo <cmd>              run cmd as root in the current VM
@alias [name=value]      list aliases, or set one (example: @alias clear=@host clear)
@alias -d name           delete an alias
@ps                      list VMs
@jobs                    list background jobs
@status                  show vmsh and selected VM state
@start [--vm id]         start a blank VM
@stop [--vm id]          stop a VM
@restart [--vm id]       restart a VM after confirmation
@save [--vm id] tag      save the selected VM root filesystem as a local image
@rmi image               remove a locally cached image
@tmux [session]          open tmux with vmsh as the default pane command
@forward H:G             forward host port H to guest port G
opts: --vm id --cwd path --user user --sudo --memory-mb n --memory n[m|g] --cpus n --network --no-network --nested --no-nested
cd <dir>                 change the current host or VM working directory
exit                     leave vmsh
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
	opts, next, err := parseCommandOptions(tokens, i)
	if err != nil {
		return atLine{}, err
	}
	at.Options = opts
	if next < len(tokens) {
		at.Command = strings.TrimSpace(body[tokens[next].Start:])
	}
	return at, nil
}

func parseCommandOptions(tokens []shellToken, start int) (commandOptions, int, error) {
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
		default:
			return opts, i, fmt.Errorf("unknown vmsh option %q", name)
		}
		i++
	}
	return opts, i, nil
}

func (c commandContext) withOptions(opts commandOptions) commandContext {
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
	return c
}

func sudoCommandContext(ctx commandContext, command string) (commandContext, string) {
	if ctx.Mode == modeHost {
		return ctx, "sudo " + command
	}
	ctx.Mode = modeVM
	ctx.User = "root"
	return ctx, command
}

const (
	defaultGuestUID = 1000
	defaultGuestGID = 1000
)

func guestRunUser(ctx commandContext) string {
	if strings.TrimSpace(ctx.User) != "" {
		return ctx.User
	}
	if runtime.GOOS == "windows" {
		return "root"
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

func isExitCommand(line string) bool {
	fields, err := splitShellFields(line)
	return err == nil && len(fields) == 1 && fields[0] == "exit"
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
		return exitErr.ExitCode()
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
	case "serial":
		if event.Data != "" {
			return event.Data
		}
	}
	return ""
}

func resolveCCVMPath(path string) (ccvmLaunch, error) {
	if path != "" {
		return ccvmLaunch{Path: path}, nil
	}
	exePath, err := os.Executable()
	if err != nil {
		return ccvmLaunch{}, err
	}
	for _, candidate := range ccvmPathCandidates(exePath) {
		if _, err := os.Stat(candidate); err == nil {
			return ccvmLaunch{Path: candidate}, nil
		}
	}
	if bundledCCVMAvailable() {
		return ccvmLaunch{Path: exePath, Env: []string{internalCCVMEnv + "=1"}}, nil
	}
	if found, err := exec.LookPath("ccvm"); err == nil {
		return ccvmLaunch{Path: found}, nil
	}
	return ccvmLaunch{}, fmt.Errorf("ccvm binary not found next to %s, bundled in vmsh, or on PATH; pass -ccvm", exePath)
}

func ccvmPathCandidates(exePath string) []string {
	return []string{
		filepath.Join(filepath.Dir(exePath), hostExecutableName("ccvm")),
		companionExecutablePath(exePath, "vm"),
	}
}

func hostExecutableName(name string) string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		return name + ".exe"
	}
	return name
}

func companionExecutablePath(exePath, suffix string) string {
	if runtime.GOOS != "windows" {
		return exePath + suffix
	}
	ext := filepath.Ext(exePath)
	if ext == "" {
		return exePath + suffix + ".exe"
	}
	return strings.TrimSuffix(exePath, ext) + suffix + ext
}

func ccvmLaunchName(launch ccvmLaunch) string {
	if len(launch.Args) == 0 {
		return launch.Path
	}
	return launch.Path + " " + strings.Join(launch.Args, " ")
}

func connectBackend(launch ccvmLaunch, cacheDir, statePath string) (*client.Client, error) {
	if state, err := readDaemonState(statePath); err == nil {
		api := newClient(state.Addr)
		if err := api.HealthCheck(); err == nil {
			return api, nil
		}
		_ = os.Remove(statePath)
	}

	args := append([]string{}, launch.Args...)
	args = append(args, "-cache-dir", cacheDir)
	proc := exec.Command(launch.Path, args...)
	if len(launch.Env) != 0 {
		proc.Env = append(os.Environ(), launch.Env...)
	}
	proc.Stderr = os.Stderr
	detachDaemonCommand(proc)
	stdout, err := proc.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("prepare ccvm stdout pipe for %s: %w", ccvmLaunchName(launch), err)
	}
	if err := proc.Start(); err != nil {
		return nil, fmt.Errorf("start ccvm daemon %s with cache %s: %w", ccvmLaunchName(launch), cacheDir, err)
	}

	var hello client.ServerHello
	if err := json.NewDecoder(stdout).Decode(&hello); err != nil {
		_ = proc.Wait()
		return nil, fmt.Errorf("ccvm daemon did not send a startup banner from %s: %w", ccvmLaunchName(launch), err)
	}
	if err := validateServerHello(hello, cacheDir); err != nil {
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, err
	}
	if err := writeDaemonState(statePath, daemonState{Addr: hello.Addr}); err != nil {
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("write daemon state %s for %s: %w", statePath, hello.Addr, err)
	}
	api := newClient(hello.Addr)
	if err := api.HealthCheck(); err != nil {
		_ = os.Remove(statePath)
		_ = proc.Process.Kill()
		_ = proc.Wait()
		return nil, fmt.Errorf("ccvm daemon started at %s but health check failed: %w", hello.Addr, err)
	}
	return api, nil
}

func validateServerHello(hello client.ServerHello, cacheDir string) error {
	if hello.Error != "" || hello.Kind == "error" {
		detail := firstNonEmpty(hello.Detail, hello.Error, "unknown startup error")
		return fmt.Errorf("ccvm daemon failed to start using cache %s: %s", cacheDir, detail)
	}
	if strings.TrimSpace(hello.Addr) == "" {
		return fmt.Errorf("ccvm daemon sent a startup banner without an address: %+v", hello)
	}
	return nil
}

func newClient(addr string) *client.Client {
	return client.NewClient("http://"+addr, func() (net.Conn, error) {
		return net.Dial("tcp", addr)
	})
}

func startDaemonLease(api *client.Client) (func(), error) {
	const timeout = 10 * time.Second
	lease, err := api.CreateWatchdogLease(client.WatchdogLeaseRequest{TimeoutSeconds: timeout.Seconds()})
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(timeout / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				_ = api.FeedWatchdogLease(lease.LeaseID)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
		_ = api.ReleaseWatchdogLease(lease.LeaseID)
	}, nil
}

func readDaemonState(path string) (daemonState, error) {
	var state daemonState
	buf, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(buf, &state); err != nil {
		return state, err
	}
	if state.Addr == "" {
		return state, fmt.Errorf("daemon state missing address")
	}
	return state, nil
}

func writeDaemonState(path string, state daemonState) error {
	buf, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
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
	return ok && isTerminalFD(int(file.Fd()))
}

func writerIsTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && isTerminalFD(int(file.Fd()))
}
