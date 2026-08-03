package main

import (
	"image"
	"strings"
	"testing"
)

func TestSettingsLayoutFitsSupportedWindows(t *testing.T) {
	for _, viewport := range []image.Point{
		image.Pt(1440, 900),
		image.Pt(1024, 640),
		image.Pt(760, 520),
	} {
		t.Run(viewport.String(), func(t *testing.T) {
			layout := settingsControlLayout(float32(viewport.X), float32(viewport.Y))
			windowBounds := image.Rect(0, 0, viewport.X, viewport.Y)
			if !layout.panel.In(windowBounds) {
				t.Fatalf("panel %v is outside viewport %v", layout.panel, windowBounds)
			}
			minimumTop := uiOuterMargin + uiSettingsTopInset
			if viewport.X < 800 {
				minimumTop = uiCompactMargin + uiSettingsTopInset
			}
			if layout.panel.Min.Y != minimumTop {
				t.Fatalf("panel top = %d, want %d", layout.panel.Min.Y, minimumTop)
			}

			controls := []image.Rectangle{
				layout.brand,
				layout.state,
				layout.sshCheckbox,
				layout.systemCheckbox,
				layout.sharedFolder,
				layout.sharedBrowse,
				layout.actionDivider,
				layout.skip,
				layout.button,
			}
			controls = append(controls, layout.status[:]...)
			for _, bounds := range controls {
				if !bounds.In(layout.panel) {
					t.Errorf("control %v is outside panel %v", bounds, layout.panel)
				}
			}
			for _, bounds := range []image.Rectangle{
				layout.sshCheckbox,
				layout.systemCheckbox,
				layout.sharedBrowse,
				layout.skip,
				layout.button,
			} {
				if bounds.Dy() < 44 {
					t.Errorf("interactive control %v is shorter than 44 logical pixels", bounds)
				}
			}
			if layout.sshCheckbox.Overlaps(layout.systemCheckbox) {
				t.Fatal("settings options overlap")
			}
			if layout.skip.Overlaps(layout.button) {
				t.Fatal("start actions overlap")
			}
			if layout.button.Max.X != layout.panel.Max.X || layout.button.Max.Y != layout.panel.Max.Y {
				t.Fatalf("primary action %v is not aligned to panel %v", layout.button, layout.panel)
			}
			if got := layout.actionDivider.Min.Y - layout.sharedFolder.Max.Y; got != 6 {
				t.Fatalf("footer divider gap = %d, want 6", got)
			}
			for index, first := range layout.status {
				for _, second := range layout.status[index+1:] {
					if first.Overlaps(second) {
						t.Fatalf("status cards overlap: %v and %v", first, second)
					}
				}
			}
		})
	}
}

func TestSettingsLayoutUsesThreeQuartersOfAvailableWidth(t *testing.T) {
	for _, width := range []int{760, 1024, 1440} {
		layout := settingsControlLayout(float32(width), 900)
		want := int(float32(width) * uiPanelWidthRatio)
		if layout.panel.Dx() != want {
			t.Fatalf("panel width at %d = %d, want %d", width, layout.panel.Dx(), want)
		}
	}
}

func TestLongStartupDetailWrapsWithinTwoReadableLines(t *testing.T) {
	lines := wrapStartupText(
		"Hypervisor.framework could not create the interrupt controller required by this machine",
		320,
		16,
		2,
	)
	if len(lines) != 2 {
		t.Fatalf("wrapped detail has %d lines, want 2", len(lines))
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatal("wrapped detail contains an empty line")
		}
		if len([]rune(line)) > 32 {
			t.Fatalf("wrapped line exceeds its measured width: %q", line)
		}
	}
}

func TestErrorWrappingPreservesCompleteMessage(t *testing.T) {
	message := "boot failed: Hypervisor.framework/could-not-create-a-required-interrupt-controller because the configured machine is unsupported"
	lines := wrapStartupTextAll(message, 180, 15)
	if len(lines) < 2 {
		t.Fatalf("long error wrapped to %d lines, want multiple lines", len(lines))
	}
	if got, want := strings.Join(strings.Fields(strings.Join(lines, "")), ""), strings.Join(strings.Fields(message), ""); got != want {
		t.Fatalf("wrapped error = %q, want complete message %q", got, want)
	}
}

func TestSerialTextIsSafeForStartupDisplay(t *testing.T) {
	viewer := displayViewer{}
	viewer.appendStartupSerial("\x1b[1;31mFAILED\x1b[0m normal \xff\nnext")
	snapshot := viewer.startupTerminal.Snapshot()
	if got := snapshot.Cells[0][0]; got.R != 'F' || !got.Attr.Bold || got.Attr.FG != 1 {
		t.Fatalf("highlighted serial cell = %+v", got)
	}
	if got := startupTerminalRune(snapshot.Cells[0][14].R); got != '?' {
		t.Fatalf("invalid serial rune = %q, want safe replacement", got)
	}
	if got := snapshot.Cells[1][0].R; got != 'n' {
		t.Fatalf("text after bare LF starts at column zero with %q, want n", got)
	}
}

func TestSerialCRLFAcrossChunksIsNotDoubled(t *testing.T) {
	viewer := displayViewer{}
	viewer.appendStartupSerial("first\r")
	viewer.appendStartupSerial("\nsecond")
	snapshot := viewer.startupTerminal.Snapshot()
	if got := snapshot.Cells[1][0].R; got != 's' {
		t.Fatalf("text after split CRLF starts at column zero with %q, want s", got)
	}
}

func TestStartupLayoutFitsSupportedWindows(t *testing.T) {
	for _, viewport := range []image.Point{image.Pt(1024, 640), image.Pt(760, 520)} {
		layout := calculateStartupScreenLayout(float32(viewport.X), float32(viewport.Y))
		windowBounds := image.Rect(0, 0, viewport.X, viewport.Y)
		if !layout.panel.In(windowBounds) {
			t.Fatalf("startup panel %v is outside viewport %v", layout.panel, windowBounds)
		}
		for _, bounds := range append([]image.Rectangle{layout.brand, layout.state, layout.bar}, layout.steps[:]...) {
			if !bounds.In(layout.panel) {
				t.Errorf("startup element %v is outside panel %v", bounds, layout.panel)
			}
		}
	}
}

func TestStartupKeyboardTraversalIncludesOnlyVisibleActions(t *testing.T) {
	control := startupControlNone
	want := []startupControl{
		startupControlSSH,
		startupControlSystem,
		startupControlSharedFolder,
		startupControlPrimary,
		startupControlSSH,
	}
	for index, expected := range want {
		control = nextStartupControl(control, false, false)
		if control != expected {
			t.Fatalf("forward control %d = %v, want %v", index, control, expected)
		}
	}

	control = nextStartupControl(startupControlNone, true, true)
	if control != startupControlPrimary {
		t.Fatalf("reverse traversal starts at %v, want primary action", control)
	}
	control = nextStartupControl(control, true, true)
	if control != startupControlSkip {
		t.Fatalf("reverse traversal reached %v, want visible skip action", control)
	}
}

func TestStartupPointerTargetsUseDrawnControlBounds(t *testing.T) {
	layout := settingsControlLayout(1024, 640)
	tests := []struct {
		point image.Point
		want  startupControl
	}{
		{point: layout.sshCheckbox.Min.Add(image.Pt(2, 2)), want: startupControlSSH},
		{point: layout.systemCheckbox.Min.Add(image.Pt(2, 2)), want: startupControlSystem},
		{point: layout.sharedBrowse.Min.Add(image.Pt(2, 2)), want: startupControlSharedFolder},
		{point: layout.skip.Min.Add(image.Pt(2, 2)), want: startupControlSkip},
		{point: layout.button.Min.Add(image.Pt(2, 2)), want: startupControlPrimary},
		{point: layout.panel.Min, want: startupControlNone},
	}
	for _, test := range tests {
		if got := startupControlAt(test.point, layout, true); got != test.want {
			t.Errorf("control at %v = %v, want %v", test.point, got, test.want)
		}
	}
	if got := startupControlAt(layout.skip.Min.Add(image.Pt(2, 2)), layout, false); got != startupControlNone {
		t.Fatalf("hidden skip target is still interactive: %v", got)
	}
}
