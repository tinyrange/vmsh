package main

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
			ReleaseUpdate: &squadVMReleaseUpdate{
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
		ReleaseUpdate: &squadVMReleaseUpdate{
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
