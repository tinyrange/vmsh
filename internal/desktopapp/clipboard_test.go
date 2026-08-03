package desktopapp

import "testing"

func TestClipboardReconciliationPreservesNewestSide(t *testing.T) {
	tests := []struct {
		name      string
		cached    string
		cachedGen uint64
		host      string
		guest     string
		guestGen  uint64
		want      clipboardDecision
	}{
		{
			name:   "host pasteboard update",
			cached: "old", cachedGen: 2, host: "copied on host", guest: "old", guestGen: 2,
			want: clipboardDecision{text: "copied on host", guestGeneration: 2, sendToGuest: true},
		},
		{
			name:   "guest clipboard update",
			cached: "old", cachedGen: 2, host: "old", guest: "copied in guest", guestGen: 3,
			want: clipboardDecision{text: "copied in guest", guestGeneration: 3, writeToHost: true},
		},
		{
			name:   "simultaneous updates keep pasteboard",
			cached: "old", cachedGen: 2, host: "new pasteboard", guest: "stale guest", guestGen: 3,
			want: clipboardDecision{text: "new pasteboard", guestGeneration: 3, sendToGuest: true},
		},
		{
			name:   "unchanged",
			cached: "same", cachedGen: 4, host: "same", guest: "same", guestGen: 4,
			want: clipboardDecision{text: "same", guestGeneration: 4},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := reconcileClipboard(test.cached, test.cachedGen, test.host, test.guest, test.guestGen)
			if got != test.want {
				t.Fatalf("clipboard decision = %+v, want %+v", got, test.want)
			}
		})
	}
}
