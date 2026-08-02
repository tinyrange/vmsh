package tour

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/ptyterm"
	"github.com/tinyrange/vmsh/internal/termui/asciicast"
	"go.starlark.net/starlark"
)

const SchemaVersion = 1

var validTourID = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type Options struct {
	ScriptPath   string
	OutputPath   string
	Command      []string
	Dir          string
	Env          []string
	Size         ptyterm.Size
	Timeout      time.Duration
	TypeDelay    time.Duration
	EnterDelay   time.Duration
	SectionDelay time.Duration
	Version      string
	Commit       string
	Values       map[string]string
}

type Result struct {
	ID       string
	Title    string
	Sections int
	Output   string
}

type scriptMetadata struct {
	id          string
	title       string
	description string
}

func Run(parent context.Context, opts Options) (Result, error) {
	if strings.TrimSpace(opts.ScriptPath) == "" {
		return Result{}, fmt.Errorf("tour script is required")
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return Result{}, fmt.Errorf("tour output path is required")
	}
	if len(opts.Command) == 0 || strings.TrimSpace(opts.Command[0]) == "" {
		return Result{}, fmt.Errorf("tour command is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.Size.Cols <= 0 {
		opts.Size.Cols = 100
	}
	if opts.Size.Rows <= 0 {
		opts.Size.Rows = 30
	}

	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()
	thread := &starlark.Thread{Name: "vmsh-tour:" + filepath.Base(opts.ScriptPath)}
	thread.SetMaxExecutionSteps(10_000_000)
	globals, err := starlark.ExecFile(thread, opts.ScriptPath, nil, nil)
	if err != nil {
		return Result{}, fmt.Errorf("load tour: %w", err)
	}
	metadata, err := readMetadata(globals)
	if err != nil {
		return Result{}, err
	}
	mainValue, ok := globals["main"]
	if !ok {
		return Result{}, fmt.Errorf("tour %q does not define main(ctx)", metadata.id)
	}
	mainFn, ok := mainValue.(starlark.Callable)
	if !ok {
		return Result{}, fmt.Errorf("tour %q main is not callable", metadata.id)
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create tour output directory: %w", err)
	}
	recorder, err := asciicast.CreateTour(opts.OutputPath, opts.Size.Cols, opts.Size.Rows, &asciicast.TourHeader{
		Schema:      SchemaVersion,
		ID:          metadata.id,
		Title:       metadata.title,
		Description: metadata.description,
		VMSHVersion: strings.TrimSpace(opts.Version),
		Commit:      strings.TrimSpace(opts.Commit),
	})
	if err != nil {
		return Result{}, fmt.Errorf("create tour cast: %w", err)
	}
	closedRecorder := false
	defer func() {
		if !closedRecorder {
			_ = recorder.Close()
		}
	}()

	session, err := ptyterm.Start(ctx, ptyterm.Options{
		Command:      append([]string(nil), opts.Command...),
		Dir:          opts.Dir,
		Env:          append([]string(nil), opts.Env...),
		CleanEnv:     true,
		Size:         opts.Size,
		HistoryLimit: 2000,
		Recorder:     recorder,
	})
	if err != nil {
		return Result{}, fmt.Errorf("start tour terminal: %w", err)
	}
	defer session.Close()
	driver := ptyterm.NewDriver(session)
	driver.SetDelay(opts.TypeDelay)
	tourCtx := &contextValue{
		ctx:          ctx,
		driver:       driver,
		session:      session,
		recorder:     recorder,
		values:       cloneValues(opts.Values),
		enterDelay:   opts.EnterDelay,
		sectionDelay: opts.SectionDelay,
	}

	if _, err := starlark.Call(thread, mainFn, starlark.Tuple{tourCtx}, nil); err != nil {
		return Result{}, fmt.Errorf("run tour %q: %w; %s", metadata.id, err, snapshotSummary(session.Snapshot()))
	}
	if !session.Snapshot().Exited {
		return Result{}, fmt.Errorf("tour %q completed without exiting vmsh", metadata.id)
	}
	result := session.Wait(ctx)
	if result.Err != nil || result.ExitCode != 0 {
		return Result{}, fmt.Errorf("tour %q terminal exited with code %d: %v", metadata.id, result.ExitCode, result.Err)
	}
	if tourCtx.sections == 0 {
		return Result{}, fmt.Errorf("tour %q did not define any guided sections", metadata.id)
	}
	if err := recorder.Close(); err != nil {
		return Result{}, fmt.Errorf("close tour cast: %w", err)
	}
	closedRecorder = true
	if err := RedactCast(opts.OutputPath, privacyRedactions(opts.Dir, opts.Env)); err != nil {
		return Result{}, fmt.Errorf("redact tour cast: %w", err)
	}
	if err := ValidateCast(opts.OutputPath); err != nil {
		return Result{}, fmt.Errorf("validate tour cast: %w", err)
	}
	return Result{ID: metadata.id, Title: metadata.title, Sections: tourCtx.sections, Output: opts.OutputPath}, nil
}

func privacyRedactions(workingDir string, environment []string) []Redaction {
	redactions := make([]Redaction, 0, 6)
	addPath := func(value, replacement string) {
		value = strings.TrimSpace(value)
		if value != "" && value != string(filepath.Separator) {
			redactions = append(redactions, LiteralRedaction(value, replacement))
		}
	}
	addIdentity := func(value, replacement string) {
		value = strings.TrimSpace(value)
		if value != "" {
			redactions = append(redactions, LiteralRedaction(value, replacement))
		}
	}
	addPath(workingDir, "/workspace")
	if base := filepath.Base(filepath.Clean(workingDir)); base != "." && base != string(filepath.Separator) && base != "vmsh" {
		addIdentity(base, "workspace")
	}
	if home, err := os.UserHomeDir(); err == nil {
		addPath(home, "/home/user")
	}
	for _, entry := range environment {
		if home, ok := strings.CutPrefix(entry, "HOME="); ok {
			addPath(home, "/home/user")
		}
	}
	if hostname, err := os.Hostname(); err == nil {
		addIdentity(hostname, "host")
	}
	if current, err := user.Current(); err == nil {
		addIdentity(current.Name, "User")
		addIdentity(current.Username, "user")
	}
	addIdentity(os.Getenv("USER"), "user")
	return redactions
}

func readMetadata(globals starlark.StringDict) (scriptMetadata, error) {
	read := func(name string, required bool) (string, error) {
		value, ok := globals[name]
		if !ok {
			if required {
				return "", fmt.Errorf("tour must define %s", name)
			}
			return "", nil
		}
		str, ok := starlark.AsString(value)
		if !ok {
			return "", fmt.Errorf("tour %s must be a string", name)
		}
		str = strings.TrimSpace(str)
		if required && str == "" {
			return "", fmt.Errorf("tour %s must not be empty", name)
		}
		return str, nil
	}
	id, err := read("tour_id", true)
	if err != nil {
		return scriptMetadata{}, err
	}
	if !validTourID.MatchString(id) {
		return scriptMetadata{}, fmt.Errorf("tour_id %q must be lowercase kebab-case", id)
	}
	title, err := read("tour_title", true)
	if err != nil {
		return scriptMetadata{}, err
	}
	description, err := read("tour_description", false)
	if err != nil {
		return scriptMetadata{}, err
	}
	return scriptMetadata{id: id, title: title, description: description}, nil
}

type contextValue struct {
	ctx          context.Context
	driver       *ptyterm.Driver
	session      *ptyterm.Session
	recorder     *asciicast.Recorder
	values       map[string]string
	enterDelay   time.Duration
	sectionDelay time.Duration
	lastPosition int64
	sections     int
}

func (c *contextValue) String() string        { return "<vmsh tour context>" }
func (c *contextValue) Type() string          { return "tour_context" }
func (c *contextValue) Freeze()               {}
func (c *contextValue) Truth() starlark.Bool  { return starlark.True }
func (c *contextValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: tour_context") }

func (c *contextValue) Attr(name string) (starlark.Value, error) {
	methods := map[string]func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error){
		"enter":         c.enter,
		"expect_line":   c.expectLine,
		"expect_output": c.expectOutput,
		"key":           c.key,
		"pause":         c.pause,
		"resize":        c.resize,
		"section":       c.section,
		"type":          c.typeText,
		"value":         c.value,
		"wait_exit":     c.waitExit,
		"wait_prompt":   c.waitPrompt,
		"wait_title":    c.waitTitle,
	}
	method, ok := methods[name]
	if !ok {
		return nil, nil
	}
	return starlark.NewBuiltin("ctx."+name, method), nil
}

func (c *contextValue) AttrNames() []string {
	return []string{"enter", "expect_line", "expect_output", "key", "pause", "resize", "section", "type", "value", "wait_exit", "wait_prompt", "wait_title"}
}

func (c *contextValue) typeText(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var text string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "text", &text); err != nil {
		return nil, err
	}
	if err := c.driver.Type(text); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (c *contextValue) enter(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs); err != nil {
		return nil, err
	}
	if err := c.wait(c.enterDelay); err != nil {
		return nil, err
	}
	c.lastPosition = c.session.Snapshot().BytesRead
	if err := c.driver.Key(ptyterm.KeyEnter); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (c *contextValue) key(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var chord string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "chord", &chord); err != nil {
		return nil, err
	}
	if err := c.driver.Chord(chord); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (c *contextValue) waitPrompt(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	timeout := 30
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "timeout_seconds?", &timeout); err != nil {
		return nil, err
	}
	ctx, cancel := c.operationContext(timeout)
	defer cancel()
	if _, err := c.driver.WaitRawAfter(ctx, c.lastPosition, []byte("\x1b[?2004h")); err != nil {
		return nil, fmt.Errorf("wait for vmsh prompt: %w", err)
	}
	return starlark.None, nil
}

func (c *contextValue) expectLine(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var line string
	timeout := 30
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "line", &line, "timeout_seconds?", &timeout); err != nil {
		return nil, err
	}
	ctx, cancel := c.operationContext(timeout)
	defer cancel()
	if _, err := c.driver.WaitLineExact(ctx, line); err != nil {
		return nil, fmt.Errorf("wait for exact line %q: %w", line, err)
	}
	return starlark.None, nil
}

func (c *contextValue) expectOutput(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var text string
	timeout := 30
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "text", &text, "timeout_seconds?", &timeout); err != nil {
		return nil, err
	}
	ctx, cancel := c.operationContext(timeout)
	defer cancel()
	if _, err := c.driver.WaitRawAfter(ctx, c.lastPosition, []byte(text)); err != nil {
		return nil, fmt.Errorf("wait for output %q: %w", text, err)
	}
	return starlark.None, nil
}

func (c *contextValue) waitTitle(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var title string
	timeout := 30
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "title", &title, "timeout_seconds?", &timeout); err != nil {
		return nil, err
	}
	ctx, cancel := c.operationContext(timeout)
	defer cancel()
	if _, err := c.driver.WaitTitle(ctx, title); err != nil {
		return nil, fmt.Errorf("wait for title %q: %w", title, err)
	}
	return starlark.None, nil
}

func (c *contextValue) waitExit(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	timeout := 30
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "timeout_seconds?", &timeout); err != nil {
		return nil, err
	}
	ctx, cancel := c.operationContext(timeout)
	defer cancel()
	if _, err := c.driver.WaitExit(ctx); err != nil {
		return nil, fmt.Errorf("wait for vmsh exit: %w", err)
	}
	return starlark.None, nil
}

func (c *contextValue) section(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var title, markdown string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "title", &title, "markdown", &markdown); err != nil {
		return nil, err
	}
	title = strings.TrimSpace(title)
	markdown = strings.TrimSpace(markdown)
	if title == "" || markdown == "" {
		return nil, fmt.Errorf("section title and markdown must not be empty")
	}
	if len(markdown) > 64<<10 {
		return nil, fmt.Errorf("section markdown exceeds 64 KiB")
	}
	c.sections++
	c.recorder.Metadata("vmsh.tour.section", map[string]any{
		"index":    c.sections,
		"title":    title,
		"markdown": markdown,
	})
	if err := c.wait(c.sectionDelay); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (c *contextValue) resize(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var cols, rows int
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "cols", &cols, "rows", &rows); err != nil {
		return nil, err
	}
	if err := c.session.Resize(ptyterm.Size{Cols: cols, Rows: rows}); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (c *contextValue) pause(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	milliseconds := 500
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "milliseconds?", &milliseconds); err != nil {
		return nil, err
	}
	if milliseconds < 0 || milliseconds > 10000 {
		return nil, fmt.Errorf("pause must be between 0 and 10000 milliseconds")
	}
	if err := c.wait(time.Duration(milliseconds) * time.Millisecond); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (c *contextValue) wait(delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *contextValue) value(_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, fallback string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "name", &name, "fallback?", &fallback); err != nil {
		return nil, err
	}
	value, ok := c.values[name]
	if !ok {
		value = fallback
	}
	return starlark.String(value), nil
}

func (c *contextValue) operationContext(timeoutSeconds int) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	return context.WithTimeout(c.ctx, time.Duration(timeoutSeconds)*time.Second)
}

func cloneValues(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func snapshotSummary(snapshot ptyterm.Snapshot) string {
	lines := append(append([]string{}, snapshot.History...), snapshot.Lines...)
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " ")
	}
	return "terminal snapshot:\n" + strings.Join(lines, "\n")
}
