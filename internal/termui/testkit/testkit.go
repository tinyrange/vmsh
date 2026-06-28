package testkit

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/tinyrange/vmsh/internal/termui/editor"
	"github.com/tinyrange/vmsh/internal/termui/shell"
	"github.com/tinyrange/vmsh/internal/termui/terminal"
)

type Transcript struct {
	Stdout string
	Stderr string
}

type Harness struct {
	Input  *bytes.Buffer
	Output *bytes.Buffer
	Err    *bytes.Buffer
	Editor *editor.Editor
	Shell  *shell.Shell
}

func NewHarness(script string) *Harness {
	in := bytes.NewBufferString(script)
	out := &bytes.Buffer{}
	err := &bytes.Buffer{}
	caps := terminal.Capabilities{Mode: terminal.ModeNonInteractive, Width: 80, Height: 24}
	ed := editor.New(editor.Options{
		Reader:       in,
		Writer:       err,
		Capabilities: &caps,
		Completer:    shell.CommandCompleter{},
	})
	sh := shell.New(ed, in, out, err)
	return &Harness{
		Input:  in,
		Output: out,
		Err:    err,
		Editor: ed,
		Shell:  sh,
	}
}

func RunShell(t testing.TB, script string) Transcript {
	t.Helper()
	h := NewHarness(script)
	h.Shell.External = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
		return nil
	}
	if err := h.Shell.Loop(context.Background()); err != nil {
		t.Fatalf("shell loop: %v", err)
	}
	return Transcript{Stdout: h.Output.String(), Stderr: h.Err.String()}
}
