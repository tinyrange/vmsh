package editor

import "testing"

func TestLineEditorTabKeepsOpenCompletionSelectionStable(t *testing.T) {
	e := &LineEditor{}
	e.openCompletionMenu([]string{"alpha", "beta"}, 1, "a")
	e.menu.selected = 1

	e.handleTab()

	if e.menu.selected != 1 {
		t.Fatalf("selected completion = %d, want 1", e.menu.selected)
	}
	if !e.menu.active {
		t.Fatalf("completion menu was closed")
	}
}
