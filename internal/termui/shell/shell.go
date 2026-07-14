package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/termui/editor"
	"github.com/tinyrange/vmsh/internal/termui/progress"
	"github.com/tinyrange/vmsh/internal/termui/terminal"
	waitui "github.com/tinyrange/vmsh/internal/termui/wait"
)

var ErrExit = errors.New("shell exit")

type ExternalRunner func(context.Context, []string, io.Reader, io.Writer, io.Writer) error

type MetadataRecorder interface {
	Metadata(name string, fields map[string]any)
}

type Shell struct {
	Editor   *editor.Editor
	Stdout   io.Writer
	Stderr   io.Writer
	Stdin    io.Reader
	External ExternalRunner
	Prompt   func() string
	Recorder MetadataRecorder
}

func New(ed *editor.Editor, stdin io.Reader, stdout, stderr io.Writer) *Shell {
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	return &Shell{
		Editor:   ed,
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
		External: RunExternal,
		Prompt: func() string {
			cwd, _ := os.Getwd()
			return filepath.Base(cwd) + "$ "
		},
	}
}

func (s *Shell) Loop(ctx context.Context) error {
	for {
		line, err := s.Editor.ReadLine(ctx, s.Prompt())
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, editor.ErrLineInterrupted) {
			continue
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		started := time.Now()
		err = s.RunLine(ctx, line)
		elapsed := time.Since(started)
		threshold := (waitui.Spec{}).FirstStatusAfter()
		if elapsed > threshold && s.Recorder != nil {
			s.Recorder.Metadata("termui.slow_interaction", map[string]any{
				"line":         line,
				"elapsed_ms":   elapsed.Milliseconds(),
				"threshold_ms": threshold.Milliseconds(),
			})
		}
		if err != nil {
			if errors.Is(err, ErrExit) {
				return nil
			}
			if errors.Is(err, waitui.ErrInterrupted) || errors.Is(err, context.Canceled) {
				continue
			}
			if waitui.IsDisplayed(err) {
				continue
			}
			fmt.Fprintln(s.Stderr, err)
		}
	}
}

func (s *Shell) RunLine(ctx context.Context, line string) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case "exit", "quit":
		return ErrExit
	case "cd":
		dir := ""
		if len(fields) > 1 {
			dir = fields[1]
		} else {
			dir, _ = os.UserHomeDir()
		}
		return os.Chdir(dir)
	case "pwd":
		cwd, err := os.Getwd()
		if err == nil {
			fmt.Fprintln(s.Stdout, cwd)
		}
		return err
	case "sleep":
		return s.runSleep(ctx, fields[1:])
	case "spin":
		return s.runSpin(ctx, fields[1:])
	case "progress":
		return s.runProgress(ctx, fields[1:])
	case "failwait":
		return s.runFailWait(ctx, fields[1:])
	case "timeout":
		return s.runTimeout(ctx, fields[1:])
	case "help":
		fmt.Fprintln(s.Stdout, "builtins: cd pwd sleep spin progress failwait timeout help exit")
		return nil
	default:
		if s.External == nil {
			return fmt.Errorf("%s: external commands disabled", fields[0])
		}
		return s.External(ctx, fields, s.Stdin, s.Stdout, s.Stderr)
	}
}

func (s *Shell) runSpin(ctx context.Context, args []string) error {
	duration := parseDurationArg(args, 3*time.Second)
	return s.Editor.Wait(ctx, waitui.Spec{
		Message:           "warming up",
		CompletionMessage: "spin complete",
		InterruptMessage:  "spin interrupted",
	}, func(ctx context.Context, r *progress.Reporter) error {
		started := time.Now()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		ticks := int64(0)
		deadline := time.NewTimer(duration)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return waitui.ErrInterrupted
			case <-deadline.C:
				return nil
			case <-ticker.C:
				ticks++
				r.Update(progress.Snapshot{
					Operation: "warming up",
					Unit:      "ticks",
					Done:      ticks,
					Started:   started,
				})
			}
		}
	})
}

func (s *Shell) runProgress(ctx context.Context, args []string) error {
	total := int64(20)
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
			total = int64(n)
		}
	}
	return s.Editor.Wait(ctx, waitui.Spec{
		Message:           "processing records",
		CompletionMessage: "progress complete",
		InterruptMessage:  "progress interrupted",
	}, func(ctx context.Context, r *progress.Reporter) error {
		started := time.Now()
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for i := int64(0); i <= total; i++ {
			r.Update(progress.Snapshot{
				Operation: "processing",
				Unit:      "records",
				Done:      i,
				Total:     total,
				Started:   started,
			})
			if i == total {
				return nil
			}
			select {
			case <-ctx.Done():
				return waitui.ErrInterrupted
			case <-ticker.C:
			}
		}
		return nil
	})
}

func (s *Shell) runFailWait(ctx context.Context, args []string) error {
	duration := parseDurationArg(args, 1500*time.Millisecond)
	return s.Editor.Wait(ctx, waitui.Spec{
		Message:       "checking fragile thing",
		FailurePrefix: "check failed",
	}, func(ctx context.Context, r *progress.Reporter) error {
		started := time.Now()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(duration)
		defer deadline.Stop()
		attempt := int64(0)
		for {
			select {
			case <-ctx.Done():
				return waitui.ErrInterrupted
			case <-deadline.C:
				return errors.New("demo failure after partial work")
			case <-ticker.C:
				attempt++
				r.Update(progress.Snapshot{
					Operation: "checking",
					Unit:      "attempts",
					Done:      attempt,
					Started:   started,
				})
			}
		}
	})
}

func (s *Shell) runTimeout(ctx context.Context, args []string) error {
	timeout := parseDurationArg(args, time.Second)
	return s.Editor.Wait(ctx, waitui.Spec{
		Message:          "waiting for a demo timeout",
		Timeout:          timeout,
		InterruptMessage: "timeout stopped the wait",
		FailurePrefix:    "timeout",
	}, func(ctx context.Context, r *progress.Reporter) error {
		started := time.Now()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		ticks := int64(0)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				ticks++
				r.Update(progress.Snapshot{
					Operation: "still waiting",
					Unit:      "ticks",
					Done:      ticks,
					Started:   started,
				})
			}
		}
	})
}

func parseDurationArg(args []string, fallback time.Duration) time.Duration {
	if len(args) == 0 {
		return fallback
	}
	if d, err := time.ParseDuration(args[0]); err == nil && d >= 0 {
		return d
	}
	if n, err := strconv.Atoi(args[0]); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

func (s *Shell) runSleep(ctx context.Context, args []string) error {
	seconds := 5
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 0 {
			return fmt.Errorf("sleep: duration must be a non-negative number of seconds")
		}
		seconds = n
	}
	total := int64(seconds * 10)
	return s.Editor.Wait(ctx, waitui.Spec{
		Message:           fmt.Sprintf("sleeping for %ds", seconds),
		CompletionMessage: "sleep complete",
		InterruptMessage:  "sleep interrupted",
	}, func(ctx context.Context, r *progress.Reporter) error {
		started := time.Now()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for i := int64(0); i <= total; i++ {
			r.Update(progress.Snapshot{
				Operation: "sleeping",
				Unit:      "ticks",
				Done:      i,
				Total:     total,
				Started:   started,
			})
			if i == total {
				return nil
			}
			select {
			case <-ctx.Done():
				return waitui.ErrInterrupted
			case <-ticker.C:
			}
		}
		return nil
	})
}

func RunExternal(ctx context.Context, fields []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fields = colorizeExternal(fields, stdout)
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	restore := ignoreParentInterrupt()
	defer restore()
	if err := cmd.Wait(); err != nil {
		if externalInterrupted(err) {
			return waitui.ErrInterrupted
		}
		return err
	}
	return nil
}

func ignoreParentInterrupt() func() {
	signal.Ignore(os.Interrupt)
	return func() {
		signal.Reset(os.Interrupt)
	}
}

func colorizeExternal(fields []string, stdout io.Writer) []string {
	if len(fields) == 0 || fields[0] != "ls" || !isTerminalWriter(stdout) {
		return fields
	}
	switch runtime.GOOS {
	case "darwin":
		if hasExactArg(fields[1:], "-G") {
			return fields
		}
		return append([]string{"ls", "-G"}, fields[1:]...)
	case "linux":
		if hasLongOption(fields[1:], "--color") {
			return fields
		}
		return append([]string{"ls", "--color=auto"}, fields[1:]...)
	default:
		return fields
	}
}

func isTerminalWriter(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && terminal.IsTerminal(file)
}

func hasExactArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func hasLongOption(args []string, prefix string) bool {
	for _, arg := range args {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}

type CommandCompleter struct{}

func (CommandCompleter) Complete(line []rune, pos int) ([]string, int, editor.CompletionKind) {
	before := string(line[:pos])
	token := currentToken(before)
	if strings.Contains(strings.TrimLeft(before, " \t"), " ") {
		return completePath(token), len([]rune(token)), editor.CompletionPath
	}
	commands := append([]string{}, builtinCommands()...)
	commands = append(commands, pathCommands()...)
	var out []string
	for _, cmd := range commands {
		if strings.HasPrefix(cmd, token) {
			out = append(out, cmd)
		}
	}
	sort.Strings(out)
	return out, len([]rune(token)), editor.CompletionCommand
}

func builtinCommands() []string {
	return []string{"cd", "pwd", "sleep", "spin", "progress", "failwait", "timeout", "help", "exit"}
}

func currentToken(before string) string {
	before = strings.TrimLeft(before, " \t")
	idx := strings.LastIndexAny(before, " \t")
	if idx < 0 {
		return before
	}
	return before[idx+1:]
}

func pathCommands() []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if seen[name] {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.IsDir() || !isExecutableCommand(name, info.Mode()) {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func isExecutableCommand(name string, mode os.FileMode) bool {
	if runtime.GOOS != "windows" {
		return mode&0o111 != 0
	}
	ext := strings.ToUpper(filepath.Ext(name))
	if ext == "" {
		return false
	}
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	for _, allowed := range strings.Split(pathext, ";") {
		if strings.EqualFold(ext, strings.TrimSpace(allowed)) {
			return true
		}
	}
	return false
}

func completePath(token string) []string {
	dir, prefix := filepath.Split(token)
	searchDir := dir
	if searchDir == "" {
		searchDir = "."
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, prefix) {
			item := dir + name
			if entry.IsDir() {
				item += string(os.PathSeparator)
			}
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
