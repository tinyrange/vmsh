package main

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"

	"j5.nz/cc/client"
	"j5.nz/cc/display"
)

type startupPhase int

const (
	startupPull startupPhase = iota
	startupPrepare
	startupBoot
	startupDesktop
)

type startupProgress struct {
	Phase       startupPhase
	Title       string
	Detail      string
	Progress    float64
	Determinate bool
	Bytes       int64
	TotalBytes  int64
	Files       int64
	TotalFiles  int64
	Rate        float64
	ETA         time.Duration
	Failed      bool
}

type displayStart func(context.Context, func(startupProgress)) (display.Session, error)

func initialStartupProgress() startupProgress {
	return startupProgress{
		Phase:  startupPull,
		Title:  "Preparing Neurodesktop",
		Detail: "Checking the image and local cache",
	}
}

func pullStartupProgress(event client.ProgressEvent) startupProgress {
	progress := startupProgress{
		Phase:      startupPull,
		Title:      "Preparing Neurodesktop",
		Detail:     "Checking the image and local cache",
		Bytes:      event.BytesDownloaded,
		TotalBytes: event.BytesTotal,
		Files:      event.FilesDownloaded,
		TotalFiles: event.FilesTotal,
		Rate:       event.RateBytesPerSecond,
	}
	if event.ETASeconds > 0 {
		progress.ETA = time.Duration(event.ETASeconds * float64(time.Second))
	}
	if event.BytesTotal > 0 {
		progress.Determinate = true
		progress.Progress = float64(event.BytesDownloaded) / float64(event.BytesTotal)
	} else if event.Progress > 0 || event.FilesTotal > 0 {
		progress.Determinate = true
		progress.Progress = event.Progress
	}
	switch event.Status {
	case "available":
		progress.Title = "Using the cached image"
		progress.Detail = "The Neurodesktop image is already available"
	case "restored":
		progress.Title = "Restoring the cached image"
		progress.Detail = "Reusing verified image content"
	case "resolving":
		progress.Title = "Finding the Neurodesktop image"
		progress.Detail = "Resolving the image manifest for this computer"
	case "downloading":
		progress.Title = "Downloading Neurodesktop"
		progress.Detail = "Downloading and verifying image content"
	case "indexing":
		progress.Phase = startupPrepare
		progress.Title = "Preparing the image"
		progress.Detail = "Indexing the Neurodesktop filesystem"
	case "downloaded":
		progress.Phase = startupPrepare
		progress.Title = "Image ready"
		progress.Detail = "Preparing your workspace"
	case "error":
		progress.Failed = true
		progress.Title = "Could not prepare Neurodesktop"
		progress.Detail = strings.TrimSpace(event.Error)
	}
	return progress
}

func bootStartupProgress(event client.BootEvent) startupProgress {
	progress := startupProgress{
		Phase:  startupBoot,
		Title:  "Starting Neurodesktop",
		Detail: strings.TrimSpace(event.Message),
	}
	message := strings.ToLower(progress.Detail)
	switch {
	case strings.Contains(message, "validat"):
		progress.Phase = startupPrepare
		progress.Title = "Preparing your workspace"
	case strings.Contains(message, "kernel"), strings.Contains(message, "emulator"):
		progress.Phase = startupPrepare
		progress.Title = "Preparing the virtual machine"
	case strings.Contains(message, "network"):
		progress.Title = "Connecting the virtual machine"
	case event.Kind == "error":
		progress.Failed = true
		progress.Title = "Neurodesktop could not start"
		progress.Detail = strings.TrimSpace(event.Error)
	}
	if progress.Detail == "" {
		progress.Detail = "Waiting for the guest environment"
	}
	return progress
}

func desktopStartupProgress(detail string) startupProgress {
	return startupProgress{
		Phase:  startupDesktop,
		Title:  "Getting your desktop ready",
		Detail: detail,
	}
}

func failedStartupProgress(err error) startupProgress {
	detail := "An unexpected error occurred"
	if err != nil {
		detail = err.Error()
	}
	return startupProgress{
		Phase:  startupDesktop,
		Title:  "Neurodesktop could not start",
		Detail: detail,
		Failed: true,
	}
}

func formatStartupTransfer(progress startupProgress) string {
	if progress.TotalFiles > 0 {
		return fmt.Sprintf("%d of %d layers prepared", min(progress.Files, progress.TotalFiles), progress.TotalFiles)
	}
	if progress.Bytes <= 0 || progress.TotalBytes <= 0 {
		return ""
	}
	text := fmt.Sprintf("%s of %s", formatBytes(progress.Bytes), formatBytes(progress.TotalBytes))
	if progress.Rate > 0 {
		text += fmt.Sprintf("  ·  %s/s", formatBytes(int64(progress.Rate)))
	}
	return text
}

func formatStartupETA(eta time.Duration) string {
	if eta <= 0 {
		return ""
	}
	if eta < time.Minute {
		return fmt.Sprintf("about %d sec", max(1, int(eta.Round(time.Second)/time.Second)))
	}
	return fmt.Sprintf("about %d min", max(1, int(eta.Round(time.Minute)/time.Minute)))
}

func formatBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor := unit
	suffix := "KB"
	for _, next := range []string{"MB", "GB", "TB"} {
		if value < divisor*unit {
			break
		}
		divisor *= unit
		suffix = next
	}
	return fmt.Sprintf("%.1f %s", float64(value)/float64(divisor), suffix)
}

const desktopFrameSettleDelay = 250 * time.Millisecond

// desktopPresentationGate keeps every guest frame off-screen until the
// Neurodesktop session is known to be running and a complete framebuffer has
// remained available long enough for the compositor to paint.
type desktopPresentationGate struct {
	guestReady  bool
	fullFrameAt time.Time
	fullSize    image.Point
}

func (g *desktopPresentationGate) markGuestReady() {
	g.guestReady = true
}

func (g *desktopPresentationGate) observe(update display.FramebufferUpdate, now time.Time) {
	if !g.guestReady || update.Width <= 0 || update.Height <= 0 {
		return
	}
	full := image.Rect(0, 0, update.Width, update.Height)
	if update.Rect != full || len(update.Pixels) < update.Width*update.Height*4 {
		return
	}
	size := image.Pt(update.Width, update.Height)
	if size != g.fullSize {
		g.fullSize = size
		g.fullFrameAt = now
		return
	}
	if g.fullFrameAt.IsZero() {
		g.fullFrameAt = now
	}
}

func (g desktopPresentationGate) ready(now time.Time) bool {
	return g.guestReady && !g.fullFrameAt.IsZero() && now.Sub(g.fullFrameAt) >= desktopFrameSettleDelay
}
