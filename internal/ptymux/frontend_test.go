package ptymux

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/ptyterm"
)

func TestDefaultCommandIsInteractive(t *testing.T) {
	command := defaultCommand(nil)
	if runtime.GOOS == "windows" {
		if len(command) == 0 {
			t.Fatalf("default command is empty")
		}
		return
	}
	if len(command) < 2 || command[len(command)-1] != "-i" {
		t.Fatalf("default command = %#v, want interactive shell", command)
	}
	if strings.Contains(strings.Join(command, " "), "fish") {
		t.Fatalf("default command should avoid rich user shell prompt for now: %#v", command)
	}
}

func TestDefaultFrontendStartsVisibleShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY ownership is unsupported on Windows")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frontend, err := New(ctx, Options{Size: ptyterm.Size{Cols: 80, Rows: 8}})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	defer frontend.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := frontend.panes[frontend.active].Session.Snapshot()
		if snap.BytesRead > 0 && snapshotHasText(snap) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("default shell produced no visible text; render=%q", frontend.Render())
}

func TestFrontendPrefixCreateSwitchAndQuit(t *testing.T) {
	ctx := context.Background()
	var sessions []*fakeSession
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 60, Rows: 6},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			session := &fakeSession{snap: ptyterm.Snapshot{Lines: []string{"pane "}}}
			sessions = append(sessions, session)
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	if frontend.PaneCount() != 1 || frontend.ActivePane() != 0 {
		t.Fatalf("initial panes=%d active=%d", frontend.PaneCount(), frontend.ActivePane())
	}

	event, err := frontend.HandleInput(ctx, []byte{PrefixKey, 'c'})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if event.Action != ActionCreate || frontend.PaneCount() != 2 || frontend.ActivePane() != 1 {
		t.Fatalf("create event=%+v panes=%d active=%d", event, frontend.PaneCount(), frontend.ActivePane())
	}

	event, err = frontend.HandleInput(ctx, []byte{PrefixKey, '0'})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if event.Action != ActionSwitch || event.Pane != 0 || frontend.ActivePane() != 0 {
		t.Fatalf("switch event=%+v active=%d", event, frontend.ActivePane())
	}

	if _, err := frontend.HandleInput(ctx, []byte("abc")); err != nil {
		t.Fatalf("write active: %v", err)
	}
	if got := string(sessions[0].writes); got != "abc" {
		t.Fatalf("active writes = %q", got)
	}
	if got := string(sessions[1].writes); got != "" {
		t.Fatalf("inactive writes = %q", got)
	}

	event, err = frontend.HandleInput(ctx, []byte{PrefixKey, 'q'})
	if err != nil {
		t.Fatalf("quit: %v", err)
	}
	if event.Action != ActionQuit {
		t.Fatalf("quit event=%+v", event)
	}
}

func TestFrontendPrefixPrefixSendsLiteralCtrlG(t *testing.T) {
	ctx := context.Background()
	session := &fakeSession{}
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 40, Rows: 4},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	if _, err := frontend.HandleInput(ctx, []byte{PrefixKey, PrefixKey}); err != nil {
		t.Fatalf("prefix prefix: %v", err)
	}
	if got := session.writes; len(got) != 1 || got[0] != PrefixKey {
		t.Fatalf("literal prefix bytes = %v", got)
	}
}

func TestFrontendReapExitedLastPaneQuits(t *testing.T) {
	ctx := context.Background()
	session := &fakeSession{snap: ptyterm.Snapshot{Exited: true}}
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 40, Rows: 4},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	event := frontend.ReapExited()
	if event.Action != ActionQuit || frontend.PaneCount() != 0 || !session.closed {
		t.Fatalf("reap event=%+v panes=%d closed=%v", event, frontend.PaneCount(), session.closed)
	}
}

func TestFrontendReapExitedActivePaneSwitchesToSurvivor(t *testing.T) {
	ctx := context.Background()
	sessions := []*fakeSession{
		{snap: ptyterm.Snapshot{Lines: []string{"zero"}}},
		{snap: ptyterm.Snapshot{Exited: true}},
		{snap: ptyterm.Snapshot{Lines: []string{"two"}}},
	}
	next := 0
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 40, Rows: 4},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			session := sessions[next]
			next++
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	if err := frontend.CreatePane(ctx); err != nil {
		t.Fatalf("create pane 1: %v", err)
	}
	if err := frontend.CreatePane(ctx); err != nil {
		t.Fatalf("create pane 2: %v", err)
	}
	if frontend.ActivePane() != 2 {
		t.Fatalf("active = %d, want 2", frontend.ActivePane())
	}
	if _, err := frontend.HandleInput(ctx, []byte{PrefixKey, '1'}); err != nil {
		t.Fatalf("switch pane 1: %v", err)
	}

	event := frontend.ReapExited()
	if event.Action != ActionSwitch || frontend.PaneCount() != 2 || frontend.ActivePane() != 1 {
		t.Fatalf("reap event=%+v panes=%d active=%d", event, frontend.PaneCount(), frontend.ActivePane())
	}
	if !sessions[1].closed {
		t.Fatalf("exited active pane was not closed")
	}
	if !strings.Contains(frontend.Render(), "two") {
		t.Fatalf("render did not switch to surviving pane: %q", frontend.Render())
	}
}

func TestFrontendRenderStatusBar(t *testing.T) {
	ctx := context.Background()
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 72, Rows: 5},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			return &fakeSession{snap: ptyterm.Snapshot{Lines: []string{"hello", "world"}}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	rendered := frontend.Render()
	if !strings.Contains(rendered, "hello") || !strings.Contains(rendered, "world") {
		t.Fatalf("rendered pane = %q", rendered)
	}
	if !strings.Contains(rendered, "vmsh [0]") {
		t.Fatalf("rendered status missing pane = %q", rendered)
	}
	if !strings.Contains(rendered, "Ctrl-G q exit") || !strings.Contains(rendered, "Ctrl-G c create") {
		t.Fatalf("rendered status missing commands = %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[1;1Hhello") || !strings.Contains(rendered, "\x1b[2;1Hworld") {
		t.Fatalf("rendered pane rows should be absolutely positioned: %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[5;1H\x1b[7m") {
		t.Fatalf("rendered status should be absolutely positioned: %q", rendered)
	}
}

func TestFrontendRenderUsesCellColorAndCursor(t *testing.T) {
	ctx := context.Background()
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 20, Rows: 4},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			return &fakeSession{snap: ptyterm.Snapshot{
				Cursor:        ptyterm.Cursor{X: 2, Y: 1},
				CursorVisible: true,
				Cells: [][]ptyterm.Cell{
					{
						{R: 'R', Attr: ptyterm.Attr{Bold: true, FG: 1, BG: -1}},
						{R: 'G', Attr: ptyterm.Attr{FG: 2, BG: -1}},
						{R: 'P', Attr: ptyterm.Attr{FG: 196, BG: -1}},
						{R: 'T', Attr: ptyterm.Attr{FG: -1, BG: trueColorBase | 1<<16 | 2<<8 | 3}},
					},
					{
						{R: 'u', Attr: ptyterm.Attr{Underline: true, FG: -1, BG: 4}},
					},
				},
			}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	rendered := frontend.Render()
	if !strings.Contains(rendered, "\x1b[0;1;31mR") {
		t.Fatalf("rendered missing bold red cell: %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[0;32mG") {
		t.Fatalf("rendered missing green cell: %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[0;4;44mu") {
		t.Fatalf("rendered missing underline blue-background cell: %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[0;38;5;196mP") {
		t.Fatalf("rendered missing palette color cell: %q", rendered)
	}
	if !strings.Contains(rendered, "\x1b[0;48;2;1;2;3mT") {
		t.Fatalf("rendered missing truecolor background cell: %q", rendered)
	}
	if !strings.HasSuffix(rendered, "\x1b[0m\x1b[?25h\x1b[2;3H") {
		t.Fatalf("rendered missing cursor position suffix: %q", rendered)
	}
}

func TestFrontendRenderHidesCursorWhenPaneHidesCursor(t *testing.T) {
	ctx := context.Background()
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 20, Rows: 4},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			return &fakeSession{snap: ptyterm.Snapshot{
				Cursor:        ptyterm.Cursor{X: 2, Y: 1},
				CursorVisible: false,
				Lines:         []string{"hidden"},
			}}, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	rendered := frontend.Render()
	if !strings.HasSuffix(rendered, "\x1b[0m\x1b[?25l") {
		t.Fatalf("rendered missing hidden cursor suffix: %q", rendered)
	}
	if strings.Contains(rendered, "\x1b[2;3H") {
		t.Fatalf("rendered positioned hidden cursor: %q", rendered)
	}
}

func TestFrontendResizeResizesPanesMinusStatusRow(t *testing.T) {
	ctx := context.Background()
	session := &fakeSession{}
	frontend, err := New(ctx, Options{
		Command: []string{"sh"},
		Size:    ptyterm.Size{Cols: 20, Rows: 5},
		Starter: func(context.Context, ptyterm.Options) (Session, error) {
			return session, nil
		},
	})
	if err != nil {
		t.Fatalf("new frontend: %v", err)
	}
	frontend.Resize(ptyterm.Size{Cols: 90, Rows: 30})
	if session.resize != (ptyterm.Size{Cols: 90, Rows: 29}) {
		t.Fatalf("resize = %+v", session.resize)
	}
}

type fakeSession struct {
	writes []byte
	resize ptyterm.Size
	snap   ptyterm.Snapshot
	closed bool
}

func (s *fakeSession) Write(data []byte) (int, error) {
	s.writes = append(s.writes, data...)
	return len(data), nil
}

func (s *fakeSession) Resize(size ptyterm.Size) error {
	s.resize = size
	return nil
}

func (s *fakeSession) Snapshot() ptyterm.Snapshot {
	return s.snap
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

func snapshotHasText(snap ptyterm.Snapshot) bool {
	for _, line := range append(append([]string{}, snap.History...), snap.Lines...) {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}
