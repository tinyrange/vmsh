package desktopapp

import (
	"image"
	"image/color"
	"time"

	"github.com/tinyrange/gowin/gl"
	"github.com/tinyrange/gowin/window"
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
			title:  productName() + " " + update.Version,
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
	cardWidth := max(float32(1), min(float32(420), width-32))
	const (
		top        = float32(16)
		cardHeight = float32(128)
		gap        = float32(10)
		buttonGap  = float32(8)
	)
	left := max(float32(16), width-16-cardWidth)
	buttonWidth := (cardWidth - 32 - buttonGap) / 2
	layouts := make([]updateNotificationLayout, 0, len(notifications))
	for index, notification := range notifications {
		y := top + float32(index)*(cardHeight+gap)
		layouts = append(layouts, updateNotificationLayout{
			notification: notification,
			bounds:       image.Rect(int(left), int(y), int(left+cardWidth), int(y+cardHeight)),
			dismiss: image.Rect(
				int(left+14),
				int(y+72),
				int(left+14+buttonWidth),
				int(y+116),
			),
			apply: image.Rect(
				int(left+22+buttonWidth),
				int(y+72),
				int(left+22+buttonWidth*2),
				int(y+116),
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
	for index, layout := range updateNotificationLayouts(width, notifications) {
		bounds := layout.bounds
		v.drawPanel(backingWidth, backingHeight, scale, bounds, 10,
			uiBorderStrong, uiSurface)
		v.drawRect(
			backingWidth, backingHeight, scale,
			float32(bounds.Min.X), float32(bounds.Min.Y), 4, float32(bounds.Dy()),
			uiPrimary,
		)
		v.text.BeginDraw()
		v.drawTextBold(
			fitStartupText(layout.notification.title, float32(bounds.Dx()-32), 16),
			float32(bounds.Min.X+16), float32(bounds.Min.Y+27), 16,
			uiText,
		)
		detailColor := uiAccentSoft
		if layout.notification.kind == releaseUpdateNotification && v.releaseUpdateErr != nil {
			detailColor = uiErrorStrong
		}
		v.drawText(
			fitStartupText(layout.notification.detail, float32(bounds.Dx()-152), 14),
			float32(bounds.Min.X+16), float32(bounds.Min.Y+53), 14,
			detailColor,
		)
		focusHint := "F6: CONTROLS"
		focusHintWidth := float32(v.text.GetAdvance(v.font, 14, focusHint))
		v.drawTextBold(
			focusHint,
			float32(bounds.Max.X-16)-focusHintWidth, float32(bounds.Min.Y+53), 14,
			uiAccent,
		)
		v.text.EndDraw()

		dismissBorder := uiBorderStrong
		dismissFill := uiSurface
		applyFill := uiPrimary
		if v.updateHoverActive {
			switch v.updateHover {
			case index * 2:
				dismissBorder = uiBorderStrong
				dismissFill = uiSurfaceHover
			case index*2 + 1:
				applyFill = uiPrimaryHover
			}
		}
		v.drawPanel(backingWidth, backingHeight, scale, layout.dismiss, 8, dismissBorder, dismissFill)
		v.drawRoundedRect(backingWidth, backingHeight, scale, layout.apply, 8, applyFill)
		if v.updateFocusActive {
			focusColor := uiAccent
			switch v.updateFocus {
			case index * 2:
				v.drawOutline(backingWidth, backingHeight, scale, layout.dismiss, focusColor)
			case index*2 + 1:
				v.drawOutline(backingWidth, backingHeight, scale, layout.apply, focusColor)
			}
		}
		v.text.BeginDraw()
		v.drawNotificationButtonLabel(layout.dismiss, "DISMISS", uiText)
		v.drawNotificationButtonLabel(layout.apply, "APPLY", uiWhite)
		v.text.EndDraw()
	}
}

func (v *displayViewer) drawNotificationButtonLabel(bounds image.Rectangle, label string, textColor color.RGBA) {
	v.drawCenteredTextBold(label, bounds, 14, textColor)
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
			v.updateFocusActive = false
			v.dismissUpdateNotification(layout.notification.kind)
		case point.In(layout.apply):
			v.updateFocusActive = false
			v.applyUpdateNotification(layout.notification.kind)
		}
		return true
	}
	return false
}

func (v *displayViewer) updateUpdateNotificationHover(x, y float32, now time.Time) {
	scale := normalizedDisplayScale(v.window.Scale())
	backingWidth, _ := v.window.BackingSize()
	width := float32(backingWidth) / scale
	point := image.Pt(int(x/scale), int(y/scale))
	v.updateHoverActive = false
	for index, layout := range updateNotificationLayouts(width, v.activeUpdateNotifications(now)) {
		switch {
		case point.In(layout.dismiss):
			v.updateHover = index * 2
			v.updateHoverActive = true
			return
		case point.In(layout.apply):
			v.updateHover = index*2 + 1
			v.updateHoverActive = true
			return
		}
	}
}

func (v *displayViewer) handleUpdateNotificationKey(event window.InputEvent, now time.Time) bool {
	if event.Type == window.InputEventKeyUp && v.updateConsumedKeys[event.Key] {
		delete(v.updateConsumedKeys, event.Key)
		return true
	}
	if event.Type == window.InputEventKeyDown && v.updateConsumedKeys[event.Key] {
		return true
	}
	if event.Type != window.InputEventKeyDown || event.Repeat {
		return false
	}
	consume := func() bool {
		v.updateConsumedKeys[event.Key] = true
		return true
	}
	notifications := v.activeUpdateNotifications(now)
	if event.Key == window.KeyF6 {
		if len(notifications) == 0 {
			return false
		}
		v.updateFocusActive = true
		v.updateFocus = 0
		return consume()
	}
	if !v.updateFocusActive {
		return false
	}
	if len(notifications) == 0 {
		v.updateFocusActive = false
		return false
	}
	targetCount := len(notifications) * 2
	if v.updateFocus >= targetCount {
		v.updateFocus = targetCount - 1
	}
	switch event.Key {
	case window.KeyEscape:
		v.updateFocusActive = false
		return consume()
	case window.KeyTab:
		if event.Mods&window.ModShift != 0 {
			v.updateFocus = (v.updateFocus - 1 + targetCount) % targetCount
		} else {
			v.updateFocus = (v.updateFocus + 1) % targetCount
		}
		return consume()
	case window.KeyEnter, window.KeySpace:
		notification := notifications[v.updateFocus/2]
		if v.updateFocus%2 == 0 {
			v.dismissUpdateNotification(notification.kind)
		} else {
			v.applyUpdateNotification(notification.kind)
		}
		v.updateFocusActive = false
		return consume()
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
			if err := openReleaseURL(update.DownloadURL); err != nil {
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
	v.session = nil
	v.presentation = desktopPresentationGate{}
	v.desktopVisible = false
	v.generation = 0
	v.textureWidth = 0
	v.textureHeight = 0
	v.lastResize = image.Point{}
	v.pendingResize = image.Point{}
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
