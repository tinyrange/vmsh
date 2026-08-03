package desktopapp

import (
	"testing"
	"time"

	"github.com/tinyrange/gowin/window"
	"j5.nz/cc/client"
)

func TestUpdateNotificationsIncludeImageAndVMMThenExpire(t *testing.T) {
	shownAt := time.Unix(100, 0)
	viewer := &displayViewer{
		updateShownAt: shownAt,
		preflight: startupPreflight{
			Image: client.ImagePullPlan{
				Installed:       true,
				Available:       false,
				BytesToDownload: 1024,
			},
			ReleaseUpdate: &releaseUpdate{
				Version: "v0.7.0",
				Size:    2048,
			},
		},
	}
	notifications := viewer.activeUpdateNotifications(shownAt.Add(29 * time.Second))
	if len(notifications) != 2 {
		t.Fatalf("active notifications = %d, want image and VMM", len(notifications))
	}
	if notifications = viewer.activeUpdateNotifications(shownAt.Add(30 * time.Second)); len(notifications) != 0 {
		t.Fatalf("expired notifications = %d, want none", len(notifications))
	}
	if !viewer.releaseDismissed || !viewer.imageDismissed {
		t.Fatal("expired notifications were not dismissed")
	}
}

func TestPreflightRetainsVMMUpdateDetailAfterNotificationDismissal(t *testing.T) {
	preflight := startupPreflight{
		ReleaseChecked: true,
		ReleaseUpdate: &releaseUpdate{
			Version: "v0.7.0",
			Size:    2048,
		},
	}
	viewer := &displayViewer{preflight: preflight}
	viewer.dismissUpdateNotification(releaseUpdateNotification)
	if preflightReleaseDetail(true, viewer.preflight) != "v0.7.0 · 2.0 KB" {
		t.Fatalf("preflight VMM detail = %q", preflightReleaseDetail(true, viewer.preflight))
	}
}

func TestUpdateNotificationKeyboardControlsDoNotLeakToGuest(t *testing.T) {
	now := time.Unix(100, 0)
	viewer := &displayViewer{
		updateShownAt:      now,
		updateConsumedKeys: make(map[window.Key]bool),
		preflight: startupPreflight{
			ReleaseUpdate: &releaseUpdate{Version: "v0.7.0"},
		},
	}

	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyDown,
		Key:  window.KeyF6,
	}, now) || !viewer.updateFocusActive || viewer.updateFocus != 0 {
		t.Fatal("F6 did not focus the first update action")
	}
	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyUp,
		Key:  window.KeyF6,
	}, now) {
		t.Fatal("captured F6 release would leak to the guest")
	}
	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyDown,
		Key:  window.KeyTab,
	}, now) || viewer.updateFocus != 1 {
		t.Fatal("Tab did not move to the apply action")
	}
	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyUp,
		Key:  window.KeyTab,
	}, now) {
		t.Fatal("captured Tab release would leak to the guest")
	}
	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyDown,
		Key:  window.KeyTab,
		Mods: window.ModShift,
	}, now) || viewer.updateFocus != 0 {
		t.Fatal("Shift-Tab did not return to the dismiss action")
	}
	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyUp,
		Key:  window.KeyTab,
	}, now) {
		t.Fatal("captured Shift-Tab release would leak to the guest")
	}
	if !viewer.handleUpdateNotificationKey(window.InputEvent{
		Type: window.InputEventKeyDown,
		Key:  window.KeyEnter,
	}, now) || !viewer.releaseDismissed || viewer.updateFocusActive {
		t.Fatal("Enter did not activate the focused dismiss action")
	}
}

func TestApplyingImageUpdateStopsActiveSessionBeforeRestart(t *testing.T) {
	stopped := make(chan struct{})
	close(stopped)
	cancelled := false
	viewer := &displayViewer{
		preflight: startupPreflight{
			Image: client.ImagePullPlan{
				Installed: true,
				Available: false,
			},
		},
		startCancel: func() {
			cancelled = true
		},
		attemptStopped:    stopped,
		imageRestartReady: make(chan struct{}, 1),
		keysDown: map[window.Key]bool{
			window.KeyA: true,
		},
	}

	viewer.beginImageUpdate()

	select {
	case <-viewer.imageRestartReady:
	case <-time.After(time.Second):
		t.Fatal("image update did not wait for the active session to stop")
	}
	if !cancelled || !viewer.starting || !viewer.imageDismissed {
		t.Fatalf("image update state = cancelled %t, starting %t, dismissed %t",
			cancelled, viewer.starting, viewer.imageDismissed)
	}
	if len(viewer.keysDown) != 0 {
		t.Fatal("image restart retained guest key state")
	}
}
