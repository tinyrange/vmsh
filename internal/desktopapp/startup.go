package desktopapp

import (
	"context"
	"fmt"
	"image"
	"math"
	"strings"
	"time"

	"j5.nz/cc/client"
	"j5.nz/cc/display"
)

type startupPhase int

const (
	startupImage startupPhase = iota
	startupBoot
	startupDesktop
)

type startupProgress struct {
	Phase            startupPhase
	Title            string
	Detail           string
	Progress         float64
	DownloadProgress float64
	IndexProgress    float64
	ImagePipeline    bool
	Determinate      bool
	Bytes            int64
	TotalBytes       int64
	Files            int64
	TotalFiles       int64
	Rate             float64
	ETA              time.Duration
	Failed           bool
	Serial           string
}

type startupChecklistItem struct {
	Title  string
	Detail string
	Failed bool
}

func updateStartupChecklist(items []startupChecklistItem, progress startupProgress) ([]startupChecklistItem, bool) {
	title := strings.TrimSpace(progress.Title)
	if title == "" {
		title = "Starting " + productName()
	}
	item := startupChecklistItem{
		Title:  title,
		Detail: strings.TrimSpace(progress.Detail),
		Failed: progress.Failed,
	}
	if len(items) != 0 && items[len(items)-1].Title == item.Title {
		items[len(items)-1] = item
		return items, false
	}
	items = append(items, item)
	const historyLimit = 24
	if len(items) > historyLimit {
		items = append([]startupChecklistItem(nil), items[len(items)-historyLimit:]...)
	}
	return items, true
}

type displayPreflight func(context.Context) (startupPreflight, error)

type displayStarted struct {
	Session display.Session
	Stopped <-chan struct{}
}

type displayStart func(context.Context, startupOptions, func(startupProgress)) (displayStarted, error)

func initialStartupProgress() startupProgress {
	return startupProgress{
		Phase:  startupImage,
		Title:  "Preparing " + productName(),
		Detail: "Checking the image and local cache",
	}
}

func pullStartupProgress(event client.ProgressEvent) startupProgress {
	progress := startupProgress{
		Phase:            startupImage,
		Title:            "Preparing " + productName(),
		Detail:           "Checking the image and local cache",
		Bytes:            event.BytesDownloaded,
		TotalBytes:       event.BytesTotal,
		Files:            event.FilesDownloaded,
		TotalFiles:       event.FilesTotal,
		Rate:             event.RateBytesPerSecond,
		DownloadProgress: event.DownloadProgress,
		IndexProgress:    event.IndexProgress,
		ImagePipeline:    event.Status == "downloading" || event.Status == "processing",
	}
	if event.ETASeconds > 0 {
		progress.ETA = time.Duration(event.ETASeconds * float64(time.Second))
	}
	if event.Progress > 0 {
		progress.Determinate = true
		progress.Progress = event.Progress
	} else if event.BytesTotal > 0 {
		progress.Determinate = true
		progress.Progress = float64(event.BytesDownloaded) / float64(event.BytesTotal)
	} else if event.FilesTotal > 0 {
		progress.Determinate = true
		progress.Progress = event.Progress
	}
	switch event.Status {
	case "available":
		progress.Title = "Using the cached image"
		progress.Detail = "The " + productName() + " image is already available"
	case "restored":
		progress.Title = "Restoring the cached image"
		progress.Detail = "Reusing verified image content"
	case "resolving":
		progress.Title = "Finding the " + productName() + " image"
		progress.Detail = "Resolving the image manifest for this computer"
	case "downloading":
		progress.Title = "Downloading " + productName()
		progress.Detail = "Downloading and verifying image content"
	case "processing":
		progress.Title = "Preparing the image"
		progress.Detail = "Downloading and indexing image layers"
	case "indexing":
		progress.Title = "Preparing the image"
		progress.Detail = "Indexing the " + productName() + " filesystem"
	case "downloaded":
		progress.Title = "Image ready"
		progress.Detail = "Preparing your workspace"
	case "error":
		progress.Failed = true
		progress.Title = "Could not prepare " + productName()
		progress.Detail = strings.TrimSpace(event.Error)
	}
	return progress
}

func bootStartupProgress(event client.BootEvent) startupProgress {
	progress := startupProgress{
		Phase:  startupBoot,
		Title:  "Starting " + productName(),
		Detail: strings.TrimSpace(event.Message),
	}
	message := strings.ToLower(progress.Detail)
	switch {
	case strings.Contains(message, "validat"):
		progress.Phase = startupImage
		progress.Title = "Preparing your workspace"
	case strings.Contains(message, "kernel"), strings.Contains(message, "emulator"):
		progress.Phase = startupImage
		progress.Title = "Preparing the virtual machine"
	case strings.Contains(message, "network"):
		progress.Title = "Connecting the virtual machine"
	case event.Kind == "error":
		progress.Failed = true
		progress.Title = productName() + " could not start"
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
		Title:  productName() + " could not start",
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

func formatStartupDownload(progress startupProgress) string {
	if progress.Bytes <= 0 || progress.TotalBytes <= 0 {
		return ""
	}
	text := fmt.Sprintf("%s of %s", formatBytes(progress.Bytes), formatBytes(progress.TotalBytes))
	if progress.Rate > 0 {
		text += fmt.Sprintf(" · %s/s", formatBytes(int64(progress.Rate)))
	}
	return text
}

func formatStartupIndex(progress startupProgress) string {
	if progress.TotalFiles > 0 {
		return fmt.Sprintf("%d of %d layers", min(progress.Files, progress.TotalFiles), progress.TotalFiles)
	}
	return fmt.Sprintf("%d%%", int(math.Round(min(1, max(0, progress.IndexProgress))*100)))
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
// configured desktop session is running and a complete framebuffer has
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

func (g *desktopPresentationGate) observeOpenGLFrame(frame display.OpenGLFrame, now time.Time) {
	if !g.guestReady || frame.Width <= 0 || frame.Height <= 0 || frame.Texture == 0 {
		return
	}
	size := image.Pt(frame.Width, frame.Height)
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
