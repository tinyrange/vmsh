package main

import (
	"image"
	"image/color"
	"time"

	"github.com/tinyrange/gowin/gl"
)

const updateNotificationLifetime = 30 * time.Second

type updateNotificationKind uint8

const (
	releaseUpdateNotification updateNotificationKind = iota
	imageUpdateNotification
)

type updateNotification struct {
	kind   updateNotificationKind
	title  string
	detail string
}

type updateNotificationLayout struct {
	notification updateNotification
	bounds       image.Rectangle
	apply        image.Rectangle
	dismiss      image.Rectangle
}

func (v *displayViewer) activeUpdateNotifications(now time.Time) []updateNotification {
	if v.updateShownAt.IsZero() {
		v.updateShownAt = now
	}
	if now.Sub(v.updateShownAt) >= updateNotificationLifetime {
		v.releaseDismissed = true
		v.imageDismissed = true
		return nil
	}
	var notifications []updateNotification
	if update := v.preflight.ReleaseUpdate; update != nil && !v.releaseDismissed {
		detail := "VMM update"
		if update.Size > 0 {
			detail = formatBytes(update.Size) + " VMM update"
		}
		if v.releaseUpdateErr != nil {
			detail = v.releaseUpdateErr.Error()
		}
		notifications = append(notifications, updateNotification{
			kind:   releaseUpdateNotification,
			title:  "SquadVM " + update.Version,
			detail: detail,
		})
	}
	if v.preflight.hasUpdate() && !v.imageDismissed {
		notifications = append(notifications, updateNotification{
			kind:   imageUpdateNotification,
			title:  "Image update available",
			detail: preflightImageDetail(true, v.preflight, v.settings.DownloadRate),
		})
	}
	return notifications
}

func updateNotificationLayouts(width float32, notifications []updateNotification) []updateNotificationLayout {
	cardWidth := max(float32(1), min(float32(390), width-36))
	const (
		top        = float32(18)
		cardHeight = float32(112)
		gap        = float32(10)
		buttonGap  = float32(8)
	)
	left := max(float32(18), width-18-cardWidth)
	buttonWidth := (cardWidth - 36 - buttonGap) / 2
	layouts := make([]updateNotificationLayout, 0, len(notifications))
	for index, notification := range notifications {
		y := top + float32(index)*(cardHeight+gap)
		layouts = append(layouts, updateNotificationLayout{
			notification: notification,
			bounds:       image.Rect(int(left), int(y), int(left+cardWidth), int(y+cardHeight)),
			dismiss: image.Rect(
				int(left+14),
				int(y+70),
				int(left+14+buttonWidth),
				int(y+98),
			),
			apply: image.Rect(
				int(left+22+buttonWidth),
				int(y+70),
				int(left+22+buttonWidth*2),
				int(y+98),
			),
		})
	}
	return layouts
}

func (v *displayViewer) drawUpdateNotifications(backingWidth, backingHeight int, now time.Time) {
	notifications := v.activeUpdateNotifications(now)
	if len(notifications) == 0 {
		return
	}
	v.gl.Enable(gl.Blend)
	scale := normalizedDisplayScale(v.window.Scale())
	width := float32(backingWidth) / scale
	height := float32(backingHeight) / scale
	v.text.SetViewport(int32(width), int32(height))
	v.text.SetScale(scale)
	for _, layout := range updateNotificationLayouts(width, notifications) {
		bounds := layout.bounds
		v.drawRect(
			backingWidth, backingHeight, scale,
			float32(bounds.Min.X), float32(bounds.Min.Y),
			float32(bounds.Dx()), float32(bounds.Dy()),
			color.RGBA{R: 31, G: 22, B: 40, A: 255},
		)
		v.text.BeginDraw()
		v.drawTextBold(
			fitStartupText(layout.notification.title, float32(bounds.Dx()-28), 15),
			float32(bounds.Min.X+14), float32(bounds.Min.Y+25), 15,
			color.RGBA{R: 243, G: 245, B: 239, A: 255},
		)
		detailColor := color.RGBA{R: 195, G: 183, B: 204, A: 255}
		if layout.notification.kind == releaseUpdateNotification && v.releaseUpdateErr != nil {
			detailColor = color.RGBA{R: 255, G: 137, B: 137, A: 255}
		}
		v.drawText(
			fitStartupText(layout.notification.detail, float32(bounds.Dx()-28), 12),
			float32(bounds.Min.X+14), float32(bounds.Min.Y+48), 12,
			detailColor,
		)
		v.text.EndDraw()

		v.drawRect(
			backingWidth, backingHeight, scale,
			float32(layout.dismiss.Min.X), float32(layout.dismiss.Min.Y),
			float32(layout.dismiss.Dx()), float32(layout.dismiss.Dy()),
			color.RGBA{R: 54, G: 41, B: 65, A: 255},
		)
		v.drawRect(
			backingWidth, backingHeight, scale,
			float32(layout.apply.Min.X), float32(layout.apply.Min.Y),
			float32(layout.apply.Dx()), float32(layout.apply.Dy()),
			color.RGBA{R: 95, G: 23, B: 238, A: 255},
		)
		v.text.BeginDraw()
		v.drawNotificationButtonLabel(layout.dismiss, "DISMISS", color.RGBA{R: 235, G: 229, B: 240, A: 255})
		v.drawNotificationButtonLabel(layout.apply, "APPLY", color.RGBA{R: 255, G: 255, B: 255, A: 255})
		v.text.EndDraw()
	}
}

func (v *displayViewer) drawNotificationButtonLabel(bounds image.Rectangle, label string, textColor color.RGBA) {
	labelWidth := float32(v.text.GetAdvance(v.font, 11, label))
	v.drawTextBold(
		label,
		float32(bounds.Min.X)+(float32(bounds.Dx())-labelWidth)/2,
		float32(bounds.Min.Y+19),
		11,
		textColor,
	)
}

func (v *displayViewer) handleUpdateNotificationClick(x, y float32, now time.Time) bool {
	scale := normalizedDisplayScale(v.window.Scale())
	backingWidth, _ := v.window.BackingSize()
	width := float32(backingWidth) / scale
	point := image.Pt(int(x/scale), int(y/scale))
	for _, layout := range updateNotificationLayouts(width, v.activeUpdateNotifications(now)) {
		if !point.In(layout.bounds) {
			continue
		}
		switch {
		case point.In(layout.dismiss):
			v.dismissUpdateNotification(layout.notification.kind)
		case point.In(layout.apply):
			v.applyUpdateNotification(layout.notification.kind)
		}
		return true
	}
	return false
}

func (v *displayViewer) dismissUpdateNotification(kind updateNotificationKind) {
	switch kind {
	case releaseUpdateNotification:
		v.releaseDismissed = true
		v.releaseUpdateErr = nil
	case imageUpdateNotification:
		v.imageDismissed = true
	}
}

func (v *displayViewer) applyUpdateNotification(kind updateNotificationKind) {
	switch kind {
	case releaseUpdateNotification:
		if update := v.preflight.ReleaseUpdate; update != nil {
			if err := squadVMOpenReleaseURL(update.DownloadURL); err != nil {
				v.releaseUpdateErr = err
				return
			}
		}
		v.releaseDismissed = true
	case imageUpdateNotification:
		v.beginImageUpdate()
	}
}

func (v *displayViewer) beginImageUpdate() {
	if !v.preflight.hasUpdate() || v.starting {
		return
	}
	v.imageDismissed = true
	if v.startCancel == nil || v.attemptStopped == nil {
		v.beginStart(true)
		return
	}
	stopped := v.attemptStopped
	v.starting = true
	v.releaseNativeFrame()
	v.session = nil
	v.presentation = desktopPresentationGate{}
	v.desktopVisible = false
	v.generation = 0
	v.textureWidth = 0
	v.textureHeight = 0
	v.lastResize = image.Point{}
	v.pendingResize = image.Point{}
	v.resizeInitialized = false
	v.buttons = 0
	v.sentButtons = 0
	clear(v.keysDown)
	v.startup = desktopStartupProgress("Restarting with the image update")
	v.startCancel()
	go func() {
		<-stopped
		v.imageRestartReady <- struct{}{}
	}()
}
