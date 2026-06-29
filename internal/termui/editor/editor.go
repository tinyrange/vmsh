package editor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tinyrange/vmsh/internal/termui/progress"
	"github.com/tinyrange/vmsh/internal/termui/terminal"
	waitui "github.com/tinyrange/vmsh/internal/termui/wait"
)

var (
	ErrLineInterrupted = errors.New("line interrupted")
	ErrInterrupted     = waitui.ErrInterrupted
)

type CompletionKind string

const (
	CompletionNone    CompletionKind = ""
	CompletionAt      CompletionKind = "at"
	CompletionCommand CompletionKind = "command"
	CompletionPath    CompletionKind = "path"
	CompletionOption  CompletionKind = "option"
)

type Completer interface {
	Complete(line []rune, pos int) ([]string, int, CompletionKind)
}

type Options struct {
	In           *os.File
	Out          *os.File
	Reader       io.Reader
	Writer       io.Writer
	Capabilities *terminal.Capabilities
	HistoryPath  string
	HistoryLimit int
	Completer    Completer
}

type Editor struct {
	inFile    *os.File
	outFile   *os.File
	in        io.Reader
	plain     *bufio.Reader
	out       io.Writer
	forcedCap *terminal.Capabilities
	caps      terminal.Capabilities
	history   *history
	completer Completer
	queued    []rune
	pending   []byte
}

type historySearch struct {
	active         bool
	query          []rune
	originalBuf    []rune
	originalCursor int
	matchPos       int
}

type completionMenu struct {
	active     bool
	items      []string
	filtered   []string
	replaceLen int
	query      []rune
	selected   int
}

func New(opts Options) *Editor {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}
	if opts.Reader == nil {
		opts.Reader = opts.In
	}
	if opts.Writer == nil {
		opts.Writer = opts.Out
	}
	limit := opts.HistoryLimit
	if limit <= 0 {
		limit = 1000
	}
	caps := terminal.Detect(opts.In, opts.Out)
	if opts.Capabilities != nil {
		caps = *opts.Capabilities
	}
	return &Editor{
		inFile:    opts.In,
		outFile:   opts.Out,
		in:        opts.Reader,
		plain:     bufio.NewReader(opts.Reader),
		out:       opts.Writer,
		forcedCap: opts.Capabilities,
		caps:      caps,
		history:   loadHistory(opts.HistoryPath, limit),
		completer: opts.Completer,
	}
}

func (e *Editor) Capabilities() terminal.Capabilities {
	if e.forcedCap != nil {
		e.caps = *e.forcedCap
		return e.caps
	}
	if e.inFile == nil || e.outFile == nil {
		e.caps = terminal.Capabilities{Mode: terminal.ModeNonInteractive, Width: 80, Height: 24}
		return e.caps
	}
	e.caps = terminal.Detect(e.inFile, e.outFile)
	return e.caps
}

func (e *Editor) ReadLine(ctx context.Context, prompt string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if line, ok := e.dequeueCompleteLine(prompt); ok {
		return line, nil
	}
	if e.caps.Mode == terminal.ModeNonInteractive {
		return e.readPlainLine(ctx, prompt)
	}
	if e.inFile == nil || e.outFile == nil {
		return "", errors.New("interactive mode requires input and output files")
	}
	restore, err := terminal.MakeRaw(e.inFile)
	if err != nil {
		return "", err
	}
	defer restore()
	return e.readPreparedLine(ctx, prompt)
}

func (e *Editor) ReadLinePrepared(ctx context.Context, prompt string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if line, ok := e.dequeueCompleteLine(prompt); ok {
		return line, nil
	}
	if e.caps.Mode == terminal.ModeNonInteractive {
		return e.readPlainLine(ctx, prompt)
	}
	if e.inFile == nil || e.outFile == nil {
		return "", errors.New("interactive mode requires input and output files")
	}
	return e.readPreparedLine(ctx, prompt)
}

func (e *Editor) dequeueCompleteLine(prompt string) (string, bool) {
	if idx := indexRune(e.queued, '\n'); idx >= 0 {
		line := string(e.queued[:idx])
		e.queued = append([]rune(nil), e.queued[idx+1:]...)
		if prompt != "" && e.Capabilities().Mode != terminal.ModeNonInteractive {
			terminal.WriteString(e.out, prompt+line+"\n")
		}
		e.history.add(line)
		return line, true
	}
	return "", false
}

func (e *Editor) readPreparedLine(ctx context.Context, prompt string) (string, error) {
	restoreOut := terminal.PrepareOutput(e.outFile)
	defer restoreOut()
	terminal.WriteString(e.out, "\x1b[?2004h")
	defer terminal.WriteString(e.out, "\x1b[?2004l")

	st := lineState{
		prompt: prompt,
		buf:    append([]rune(nil), e.queued...),
		cursor: len(e.queued),
		width:  e.Capabilities().Width,
		height: e.Capabilities().Height,
	}
	e.queued = nil
	hpos := len(e.history.items)
	draft := string(st.buf)
	search := historySearch{}
	menu := completionMenu{}
	e.refresh(&st, "")
	for {
		ev, err := e.readKey(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				e.queued = append([]rune(nil), st.buf...)
			}
			if errors.Is(err, io.EOF) {
				terminal.WriteString(e.out, "\r\n")
			}
			return "", err
		}
		if search.active {
			accepted, interrupted := e.handleHistorySearch(&st, &search, ev)
			if interrupted {
				terminal.WriteString(e.out, "^C\r\n")
				e.queued = nil
				return "", ErrLineInterrupted
			}
			if accepted {
				line := string(st.buf)
				terminal.WriteString(e.out, "\r\n")
				e.history.add(line)
				return line, nil
			}
			e.refresh(&st, e.historySearchSuffix(&search))
			continue
		}
		if menu.active {
			handled, accepted := e.handleCompletionMenu(&st, &menu, ev)
			if accepted {
				e.refresh(&st, "")
				continue
			}
			if handled {
				e.refresh(&st, e.completionMenuSuffix(&menu, &st))
				continue
			}
			menu = completionMenu{}
		}
		switch ev.key {
		case keyRune:
			st.insert(ev.r)
			hpos = len(e.history.items)
			draft = string(st.buf)
		case keyPaste:
			for _, r := range normalizePaste(ev.text) {
				st.insert(r)
			}
			hpos = len(e.history.items)
			draft = string(st.buf)
		case keyEnter:
			line := string(st.buf)
			terminal.WriteString(e.out, "\r\n")
			e.history.add(line)
			return line, nil
		case keyCtrlC:
			terminal.WriteString(e.out, "^C\r\n")
			e.queued = nil
			return "", ErrLineInterrupted
		case keyCtrlD:
			if len(st.buf) == 0 {
				terminal.WriteString(e.out, "\r\n")
				return "", io.EOF
			}
			st.deleteRight()
		case keyBackspace:
			st.deleteLeft()
			hpos = len(e.history.items)
			draft = string(st.buf)
		case keyDelete:
			st.deleteRight()
			hpos = len(e.history.items)
			draft = string(st.buf)
		case keyLeft:
			if st.cursor > 0 {
				st.cursor--
			}
		case keyRight:
			if st.cursor < len(st.buf) {
				st.cursor++
			}
		case keyHome:
			st.cursor = 0
		case keyEnd:
			st.cursor = len(st.buf)
		case keyUp:
			hpos, draft = e.history.move(&st, hpos, draft, -1)
		case keyDown:
			hpos, draft = e.history.move(&st, hpos, draft, 1)
		case keyTab:
			e.startCompletion(&st, &menu)
		case keyCtrlL:
			terminal.WriteString(e.out, "\x1b[H\x1b[2J")
		case keyCtrlR:
			search = e.startHistorySearch(&st)
		}
		e.refresh(&st, e.completionMenuSuffix(&menu, &st)+e.historySearchSuffix(&search))
	}
}

func (e *Editor) Wait(ctx context.Context, spec waitui.Spec, fn func(context.Context, *progress.Reporter) error) error {
	restoreOut := terminal.DiscardRestore()
	if e.outFile != nil {
		restoreOut = terminal.PrepareOutput(e.outFile)
	}
	defer restoreOut()
	if e.caps.Mode == terminal.ModeNonInteractive {
		return e.waitPlain(ctx, spec, fn)
	}
	if e.inFile == nil || e.outFile == nil {
		return e.waitPlain(ctx, spec, fn)
	}
	restore, err := terminal.MakeRaw(e.inFile)
	if err != nil {
		return err
	}
	defer restore()
	runCtx, cancel := waitui.Context(ctx, spec.Timeout)
	defer cancel()
	reporter := spec.Progress
	if reporter == nil {
		reporter = progress.NewReporter()
	}
	done := make(chan error, 1)
	go func() {
		done <- fn(runCtx, reporter)
	}()
	return e.waitInteractive(runCtx, cancel, spec, reporter, done)
}

func (e *Editor) waitPlain(ctx context.Context, spec waitui.Spec, fn func(context.Context, *progress.Reporter) error) error {
	runCtx, cancel := waitui.Context(ctx, spec.Timeout)
	defer cancel()
	reporter := spec.Progress
	if reporter == nil {
		reporter = progress.NewReporter()
	}
	done := make(chan error, 1)
	go func() {
		done <- fn(runCtx, reporter)
	}()
	timer := time.NewTimer(spec.FirstStatusAfter())
	defer timer.Stop()
	interval := spec.TickInterval()
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var shown bool
	var latest progress.Snapshot
	for {
		select {
		case err := <-done:
			if shown {
				e.finishStatus(spec, err, false)
				if err != nil {
					return waitui.Displayed(err)
				}
			}
			return err
		case s := <-reporter.C():
			latest = s
		case <-timer.C:
			shown = true
			fmt.Fprintf(e.out, "%s\n", waitMessage(spec))
		case <-ticker.C:
			if shown {
				if latest.Operation != "" {
					fmt.Fprintf(e.out, "%s\n", latest.Message(time.Now()))
				} else {
					fmt.Fprintf(e.out, "%s\n", waitMessage(spec))
				}
			}
		}
	}
}

func (e *Editor) waitInteractive(ctx context.Context, cancel context.CancelFunc, spec waitui.Spec, reporter *progress.Reporter, done <-chan error) error {
	spinner := []string{"-", "\\", "|", "/"}
	started := time.Now()
	showAt := time.NewTimer(spec.FirstStatusAfter())
	defer showAt.Stop()
	tick := time.NewTicker(spec.TickInterval())
	defer tick.Stop()
	inputTick := time.NewTicker(10 * time.Millisecond)
	defer inputTick.Stop()
	var shown bool
	var frame int
	var latest progress.Snapshot
	var status string
	for {
		select {
		case err := <-done:
			if shown {
				e.clearStatus()
				e.finishStatus(spec, err, false)
				if err != nil {
					return waitui.Displayed(err)
				}
			}
			return err
		case s := <-reporter.C():
			latest = s
		case <-showAt.C:
			shown = true
			status = waitMessage(spec)
			e.renderStatus(spinner[frame%len(spinner)], status)
		case <-inputTick.C:
			ev, ok, err := e.pollKey()
			if err != nil && !isAgain(err) {
				cancel()
				return err
			}
			if ok {
				if ev.key == keyCtrlC {
					cancel()
					if shown {
						e.renderStatus("!", "interrupting")
					}
					select {
					case err := <-done:
						if shown {
							e.clearStatus()
							e.finishStatus(spec, err, true)
							if err != nil {
								return waitui.Displayed(err)
							}
						}
						if err == nil {
							return waitui.ErrInterrupted
						}
						return err
					case <-time.After(50 * time.Millisecond):
						if shown {
							e.clearStatus()
							e.finishStatus(spec, waitui.ErrInterrupted, true)
						}
						return waitui.ErrInterrupted
					}
				}
				e.queueEvent(ev)
			}
		case <-tick.C:
			if shown {
				frame++
				status = waitMessage(spec)
				if time.Since(started) >= spec.DetailThreshold() && latest.Operation != "" {
					status = latest.Message(time.Now())
				}
				if e.caps.ReducedMotion {
					e.renderStatus("", status)
				} else {
					e.renderStatus(spinner[frame%len(spinner)], status)
				}
			}
		case <-ctx.Done():
			if shown {
				e.clearStatus()
				e.finishStatus(spec, ctx.Err(), true)
				return waitui.Displayed(ctx.Err())
			}
			return ctx.Err()
		}
	}
}

func (e *Editor) renderStatus(mark, msg string) {
	if msg == "" {
		msg = "working"
	}
	if mark != "" {
		fmt.Fprintf(e.out, "\r\x1b[2K%s %s", mark, msg)
		return
	}
	fmt.Fprintf(e.out, "\r\x1b[2K%s", msg)
}

func (e *Editor) clearStatus() {
	terminal.WriteString(e.out, "\r\x1b[2K")
}

func (e *Editor) finishStatus(spec waitui.Spec, err error, interrupted bool) {
	switch {
	case interrupted || errors.Is(err, waitui.ErrInterrupted) || errors.Is(err, context.Canceled):
		msg := spec.InterruptMessage
		if msg == "" {
			msg = "interrupted"
		}
		fmt.Fprintf(e.out, "%s\n", msg)
	case err != nil:
		prefix := spec.FailurePrefix
		if prefix == "" {
			prefix = "failed"
		}
		fmt.Fprintf(e.out, "%s: %v\n", prefix, err)
	case spec.CompletionMessage != "":
		fmt.Fprintf(e.out, "%s\n", spec.CompletionMessage)
	}
}

func (e *Editor) readPlainLine(ctx context.Context, prompt string) (string, error) {
	if prompt != "" && e.caps.Mode != terminal.ModeNonInteractive {
		terminal.WriteString(e.out, prompt)
	}
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := e.plain.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			ch <- result{line: line}
			return
		}
		ch <- result{err: err}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			e.history.add(r.line)
		}
		return r.line, r.err
	case <-ctx.Done():
		terminal.InterruptRead(e.inFile)
		return "", ctx.Err()
	}
}

type lineState struct {
	prompt       string
	buf          []rune
	cursor       int
	width        int
	height       int
	renderedRows int
}

func (s *lineState) heightOrDefault() int {
	if s.height <= 0 {
		return 24
	}
	return s.height
}

func (s *lineState) insert(r rune) {
	s.buf = append(s.buf, 0)
	copy(s.buf[s.cursor+1:], s.buf[s.cursor:])
	s.buf[s.cursor] = r
	s.cursor++
}

func (s *lineState) deleteLeft() {
	if s.cursor == 0 {
		return
	}
	copy(s.buf[s.cursor-1:], s.buf[s.cursor:])
	s.buf = s.buf[:len(s.buf)-1]
	s.cursor--
}

func (s *lineState) deleteRight() {
	if s.cursor >= len(s.buf) {
		return
	}
	copy(s.buf[s.cursor:], s.buf[s.cursor+1:])
	s.buf = s.buf[:len(s.buf)-1]
}

func (e *Editor) refresh(s *lineState, suffix string) {
	overlay, inline := splitOverlay(suffix)
	if s.renderedRows > 0 {
		fmt.Fprintf(e.out, "\r\x1b[%dA", s.renderedRows)
		for i := 0; i <= s.renderedRows; i++ {
			terminal.WriteString(e.out, "\r\x1b[2K")
			if i < s.renderedRows {
				terminal.WriteString(e.out, "\x1b[B")
			}
		}
	}
	if len(overlay) > 0 {
		fmt.Fprintf(e.out, "\r\x1b[%dA", len(overlay))
		for _, line := range overlay {
			fmt.Fprintf(e.out, "\r\x1b[2K%s\x1b[B", line)
		}
	}
	line := string(s.buf)
	before := string(s.buf[:s.cursor])
	rendered := fmt.Sprintf("%s%s%s", s.prompt, line, inline)
	fmt.Fprintf(e.out, "\r\x1b[2K%s", rendered)
	terminal.WriteString(e.out, "\r")
	right := visibleWidth(s.prompt) + visibleWidth(before)
	if right > 0 {
		fmt.Fprintf(e.out, "\x1b[%dC", right)
	}
	s.renderedRows = len(overlay)
}

func splitOverlay(suffix string) ([]string, string) {
	if !strings.HasPrefix(suffix, "\n") {
		return nil, suffix
	}
	lines := strings.Split(strings.TrimPrefix(suffix, "\n"), "\n")
	return lines, ""
}

func (e *Editor) startCompletion(s *lineState, menu *completionMenu) {
	if e.completer == nil {
		return
	}
	items, replaceLen, _ := e.completer.Complete(s.buf, s.cursor)
	if len(items) == 0 {
		terminal.WriteString(e.out, "\a")
		return
	}
	items = uniqueSorted(items)
	if len(items) == 1 {
		e.insertCompletion(s, items[0], replaceLen)
		return
	}
	if prefix := commonPrefix(items); prefix != "" {
		token := completionToken(s, replaceLen)
		if prefix != token {
			e.insertCompletion(s, prefix, replaceLen)
			return
		}
	}
	*menu = completionMenu{
		active:     true,
		items:      items,
		filtered:   fuzzyFilter(items, ""),
		replaceLen: replaceLen,
	}
}

func (e *Editor) insertCompletion(s *lineState, repl string, replaceLen int) {
	if replaceLen > s.cursor {
		replaceLen = s.cursor
	}
	start := s.cursor - replaceLen
	next := append([]rune(nil), s.buf[:start]...)
	next = append(next, []rune(repl)...)
	next = append(next, s.buf[s.cursor:]...)
	s.buf = next
	s.cursor = start + utf8.RuneCountInString(repl)
}

func (e *Editor) handleCompletionMenu(s *lineState, menu *completionMenu, ev keyEvent) (bool, bool) {
	switch ev.key {
	case keyEnter, keyTab:
		if len(menu.filtered) == 0 {
			menu.active = false
			return true, true
		}
		e.insertCompletion(s, menu.filtered[menu.selected], menu.replaceLen)
		menu.active = false
		return true, true
	case keyEscape:
		menu.active = false
		return true, false
	case keyRune:
		menu.query = append(menu.query, ev.r)
		e.refilterCompletion(menu)
		return true, false
	case keyBackspace, keyDelete:
		if len(menu.query) > 0 {
			menu.query = menu.query[:len(menu.query)-1]
			e.refilterCompletion(menu)
		}
		return true, false
	case keyLeft:
		e.moveCompletion(menu, s, -1)
		return true, false
	case keyRight:
		e.moveCompletion(menu, s, 1)
		return true, false
	case keyUp:
		e.moveCompletion(menu, s, -e.completionColumns(menu, s))
		return true, false
	case keyDown:
		e.moveCompletion(menu, s, e.completionColumns(menu, s))
		return true, false
	case keyPageUp:
		e.moveCompletion(menu, s, -e.completionPageSize(menu, s))
		return true, false
	case keyPageDown:
		e.moveCompletion(menu, s, e.completionPageSize(menu, s))
		return true, false
	default:
		return false, false
	}
}

func (e *Editor) moveCompletion(menu *completionMenu, st *lineState, delta int) {
	if len(menu.filtered) == 0 {
		menu.selected = 0
		return
	}
	menu.selected += delta
	for menu.selected < 0 {
		menu.selected += len(menu.filtered)
	}
	menu.selected %= len(menu.filtered)
}

func (e *Editor) refilterCompletion(menu *completionMenu) {
	menu.filtered = fuzzyFilter(menu.items, string(menu.query))
	if menu.selected >= len(menu.filtered) {
		menu.selected = len(menu.filtered) - 1
	}
	if menu.selected < 0 {
		menu.selected = 0
	}
}

func (e *Editor) completionMenuSuffix(menu *completionMenu, st *lineState) string {
	if menu == nil || !menu.active {
		return ""
	}
	query := string(menu.query)
	if len(menu.filtered) == 0 {
		return fmt.Sprintf("  [complete %q: no matches]", query)
	}
	limit := 5
	if len(menu.filtered) < limit {
		limit = len(menu.filtered)
	}
	start := completionWindowStart(menu.selected, limit, len(menu.filtered))
	end := start + limit
	prefix := fmt.Sprintf("  [complete %q %d/%d (%d/%d): ", query, len(menu.filtered), len(menu.items), menu.selected+1, len(menu.filtered))
	available := st.width - visibleWidth(st.prompt) - visibleWidth(string(st.buf)) - visibleWidth(prefix) - 2
	if available < limit*4 {
		available = limit * 4
	}
	itemWidth := available/limit - 1
	if itemWidth < 3 {
		itemWidth = 3
	}
	var b strings.Builder
	b.WriteString(prefix)
	for idx := start; idx < end; idx++ {
		item := truncateCells(menu.filtered[idx], itemWidth)
		if idx == menu.selected {
			b.WriteString(" \x1b[7m>")
			b.WriteString(truncateCells(item, itemWidth-1))
			b.WriteString("\x1b[0m")
			continue
		}
		b.WriteByte(' ')
		b.WriteString(item)
	}
	if end < len(menu.filtered) {
		b.WriteString(" ...")
	}
	b.WriteByte(']')
	return b.String()
}

func completionWindowStart(selected, limit, total int) int {
	if total <= limit {
		return 0
	}
	start := selected - 2
	if start < 0 {
		return 0
	}
	if start+limit > total {
		return total - limit
	}
	return start
}

func (e *Editor) completionColumns(menu *completionMenu, st *lineState) int {
	return 1
}

func (e *Editor) completionRows(menu *completionMenu, st *lineState) int {
	if len(menu.filtered) == 0 {
		return 0
	}
	if len(menu.filtered) > 5 {
		return 5
	}
	return len(menu.filtered)
}

func (e *Editor) completionPageSize(menu *completionMenu, st *lineState) int {
	size := e.completionColumns(menu, st) * e.completionRows(menu, st)
	if size < 1 {
		return 1
	}
	return size
}

func (e *Editor) completionItemWidth(menu *completionMenu, st *lineState) int {
	return st.width - 4
}

func completionToken(s *lineState, replaceLen int) string {
	if replaceLen > s.cursor {
		replaceLen = s.cursor
	}
	if replaceLen < 0 {
		replaceLen = 0
	}
	return string(s.buf[s.cursor-replaceLen : s.cursor])
}

func uniqueSorted(items []string) []string {
	items = append([]string(nil), items...)
	sort.Strings(items)
	out := items[:0]
	for _, item := range items {
		if item == "" {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != item {
			out = append(out, item)
		}
	}
	return out
}

type fuzzyMatch struct {
	item  string
	score int
}

func fuzzyFilter(items []string, query string) []string {
	if query == "" {
		return append([]string(nil), items...)
	}
	matches := make([]fuzzyMatch, 0, len(items))
	for _, item := range items {
		score, ok := fuzzyScore(item, query)
		if ok {
			matches = append(matches, fuzzyMatch{item: item, score: score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].item < matches[j].item
	})
	out := make([]string, len(matches))
	for i, match := range matches {
		out[i] = match.item
	}
	return out
}

func fuzzyScore(item, query string) (int, bool) {
	itemLower := strings.ToLower(item)
	queryLower := strings.ToLower(query)
	pos := 0
	score := 0
	last := -1
	for _, q := range queryLower {
		found := -1
		for i, r := range itemLower[pos:] {
			if r == q {
				found = pos + i
				break
			}
		}
		if found < 0 {
			return 0, false
		}
		if found == 0 {
			score += 30
		}
		if last >= 0 && found == last+1 {
			score += 20
		}
		if found > 0 && isBoundaryRune(rune(itemLower[found-1])) {
			score += 12
		}
		score -= found - pos
		last = found
		pos = found + 1
	}
	score -= len(itemLower) / 8
	if strings.HasPrefix(itemLower, queryLower) {
		score += 80
	}
	return score, true
}

func isBoundaryRune(r rune) bool {
	return r == '-' || r == '_' || r == '.' || r == '/' || r == ' '
}

func truncateCells(s string, max int) string {
	if max <= 0 || visibleWidth(s) <= max {
		return s
	}
	rs := []rune(s)
	if max == 1 {
		return string(rs[:1])
	}
	if len(rs) > max-1 {
		rs = rs[:max-1]
	}
	return string(rs) + "~"
}

func padRight(s string, width int) string {
	for visibleWidth(s) < width {
		s += " "
	}
	return s
}

func (e *Editor) startHistorySearch(st *lineState) historySearch {
	s := historySearch{
		active:         true,
		originalBuf:    append([]rune(nil), st.buf...),
		originalCursor: st.cursor,
		matchPos:       len(e.history.items),
	}
	e.updateHistorySearch(st, &s, false)
	return s
}

func (e *Editor) handleHistorySearch(st *lineState, s *historySearch, ev keyEvent) (bool, bool) {
	switch ev.key {
	case keyCtrlC:
		return false, true
	case keyEnter:
		s.active = false
		return true, false
	case keyEscape, keyCtrlG:
		st.buf = append([]rune(nil), s.originalBuf...)
		st.cursor = s.originalCursor
		s.active = false
	case keyCtrlR:
		e.updateHistorySearch(st, s, true)
	case keyRune:
		s.query = append(s.query, ev.r)
		e.updateHistorySearch(st, s, false)
	case keyBackspace, keyDelete:
		if len(s.query) > 0 {
			s.query = s.query[:len(s.query)-1]
			e.updateHistorySearch(st, s, false)
		}
	default:
		terminal.WriteString(e.out, "\a")
	}
	return false, false
}

func (e *Editor) updateHistorySearch(st *lineState, s *historySearch, cycle bool) {
	if len(e.history.items) == 0 {
		st.buf = append([]rune(nil), s.originalBuf...)
		st.cursor = s.originalCursor
		s.matchPos = -1
		return
	}
	start := len(e.history.items) - 1
	if cycle && s.matchPos >= 0 {
		start = s.matchPos - 1
		if start < 0 {
			start = len(e.history.items) - 1
		}
	}
	query := string(s.query)
	for checked := 0; checked < len(e.history.items); checked++ {
		idx := start - checked
		if idx < 0 {
			idx += len(e.history.items)
		}
		if query == "" || strings.Contains(e.history.items[idx], query) {
			s.matchPos = idx
			st.buf = []rune(e.history.items[idx])
			st.cursor = len(st.buf)
			return
		}
	}
	st.buf = append([]rune(nil), s.originalBuf...)
	st.cursor = s.originalCursor
	s.matchPos = -1
}

func (e *Editor) historySearchSuffix(s *historySearch) string {
	if s == nil || !s.active {
		return ""
	}
	query := string(s.query)
	if s.matchPos < 0 {
		return fmt.Sprintf("  (reverse-i-search)`%s': no match", query)
	}
	return fmt.Sprintf("  (reverse-i-search)`%s'", query)
}

func commonPrefix(items []string) string {
	if len(items) == 0 {
		return ""
	}
	p := items[0]
	for _, item := range items[1:] {
		for !strings.HasPrefix(item, p) && p != "" {
			p = p[:len(p)-1]
		}
	}
	return p
}

func visibleWidth(s string) int {
	w := 0
	escState := 0
	for _, r := range s {
		switch escState {
		case 1:
			if r == '[' {
				escState = 2
			} else {
				escState = 0
			}
			continue
		case 2:
			if r >= '@' && r <= '~' {
				escState = 0
			}
			continue
		}
		if r == '\x1b' {
			escState = 1
			continue
		}
		if r == '\t' {
			w += 4
		} else if r == '\n' || r == '\r' {
			continue
		} else {
			w++
		}
	}
	return w
}

func physicalRows(s string, width int) int {
	if width <= 0 {
		width = 80
	}
	row := 0
	col := 0
	escState := 0
	for _, r := range s {
		switch escState {
		case 1:
			if r == '[' {
				escState = 2
			} else {
				escState = 0
			}
			continue
		case 2:
			if r >= '@' && r <= '~' {
				escState = 0
			}
			continue
		}
		if r == '\x1b' {
			escState = 1
			continue
		}
		if r == '\r' {
			col = 0
			continue
		}
		if r == '\n' {
			row++
			col = 0
			continue
		}
		cell := 1
		if r == '\t' {
			cell = 4
		}
		col += cell
		for col > width {
			row++
			col -= width
		}
	}
	return row
}

func normalizePaste(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func waitMessage(spec waitui.Spec) string {
	return spec.MessageText()
}

func indexRune(rs []rune, needle rune) int {
	for i, r := range rs {
		if r == needle {
			return i
		}
	}
	return -1
}

type history struct {
	path  string
	limit int
	items []string
}

func loadHistory(path string, limit int) *history {
	h := &history{path: path, limit: limit}
	if path == "" {
		return h
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if shouldSaveHistory(line) {
			h.items = append(h.items, line)
		}
	}
	h.trim()
	return h
}

func shouldSaveHistory(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.ContainsAny(line, "\r\n")
}

func (h *history) add(line string) {
	if h == nil || !shouldSaveHistory(line) {
		return
	}
	h.items = append(h.items, line)
	h.trim()
	if h.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(h.path), 0o755)
	_ = os.WriteFile(h.path, []byte(strings.Join(h.items, "\n")+"\n"), 0o600)
}

func (h *history) trim() {
	if h == nil || h.limit <= 0 || len(h.items) <= h.limit {
		return
	}
	h.items = append([]string(nil), h.items[len(h.items)-h.limit:]...)
}

func (h *history) move(st *lineState, pos int, draft string, delta int) (int, string) {
	if h == nil || len(h.items) == 0 {
		return pos, draft
	}
	if pos == len(h.items) {
		draft = string(st.buf)
	}
	pos += delta
	if pos < 0 {
		pos = 0
	}
	if pos > len(h.items) {
		pos = len(h.items)
	}
	if pos == len(h.items) {
		st.buf = []rune(draft)
	} else {
		st.buf = []rune(h.items[pos])
	}
	st.cursor = len(st.buf)
	return pos, draft
}

func isAgain(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}
