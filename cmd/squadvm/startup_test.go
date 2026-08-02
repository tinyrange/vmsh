package main

import "testing"

func TestStartupChecklistKeepsCompletedWorkVisible(t *testing.T) {
	var items []startupChecklistItem
	progress := []startupProgress{
		{Title: "Finding image", Detail: "Resolving manifest"},
		{Title: "Downloading image", Detail: "1 of 3 layers"},
		{Title: "Downloading image", Detail: "2 of 3 layers"},
		{Title: "Starting VM", Detail: "Connecting network"},
	}
	appended := 0
	for _, update := range progress {
		var added bool
		items, added = updateStartupChecklist(items, update)
		if added {
			appended++
		}
	}

	if appended != 3 || len(items) != 3 {
		t.Fatalf("checklist added %d items and retained %d, want 3", appended, len(items))
	}
	if items[1].Detail != "2 of 3 layers" {
		t.Fatalf("active checklist detail = %q, want latest progress", items[1].Detail)
	}
	if items[2].Title != "Starting VM" {
		t.Fatalf("current checklist item = %q, want Starting VM", items[2].Title)
	}
}
