package desktopapp

import (
	"fmt"
	"image/color"
)

// Theme identifies a product palette while the desktop behavior remains shared.
type Theme string

const (
	ThemeSquadVM   Theme = "squadvm"
	ThemeNeurodesk Theme = "neurodesk"
)

type interfacePalette struct {
	canvas         color.RGBA
	surface        color.RGBA
	surfaceRaised  color.RGBA
	surfaceHover   color.RGBA
	border         color.RGBA
	borderStrong   color.RGBA
	primary        color.RGBA
	primaryHover   color.RGBA
	accent         color.RGBA
	accentSoft     color.RGBA
	text           color.RGBA
	textSecondary  color.RGBA
	textMuted      color.RGBA
	disabled       color.RGBA
	success        color.RGBA
	successSurface color.RGBA
	warning        color.RGBA
	warningSurface color.RGBA
	error          color.RGBA
	errorStrong    color.RGBA
	errorSurface   color.RGBA
	errorBorder    color.RGBA
	white          color.RGBA
}

var squadVMPalette = interfacePalette{
	canvas:         color.RGBA{R: 23, G: 14, B: 31, A: 255},
	surface:        color.RGBA{R: 38, G: 27, B: 47, A: 255},
	surfaceRaised:  color.RGBA{R: 57, G: 43, B: 67, A: 255},
	surfaceHover:   color.RGBA{R: 68, G: 52, B: 80, A: 255},
	border:         color.RGBA{R: 68, G: 52, B: 79, A: 255},
	borderStrong:   color.RGBA{R: 101, G: 74, B: 121, A: 255},
	primary:        color.RGBA{R: 95, G: 23, B: 238, A: 255},
	primaryHover:   color.RGBA{R: 117, G: 49, B: 255, A: 255},
	accent:         color.RGBA{R: 189, G: 151, B: 255, A: 255},
	accentSoft:     color.RGBA{R: 221, G: 202, B: 255, A: 255},
	text:           color.RGBA{R: 243, G: 245, B: 239, A: 255},
	textSecondary:  color.RGBA{R: 211, G: 202, B: 218, A: 255},
	textMuted:      color.RGBA{R: 177, G: 163, B: 187, A: 255},
	disabled:       color.RGBA{R: 153, G: 142, B: 163, A: 255},
	success:        color.RGBA{R: 143, G: 226, B: 156, A: 255},
	successSurface: color.RGBA{R: 35, G: 69, B: 52, A: 255},
	warning:        color.RGBA{R: 255, G: 193, B: 111, A: 255},
	warningSurface: color.RGBA{R: 72, G: 52, B: 31, A: 255},
	error:          color.RGBA{R: 255, G: 151, B: 151, A: 255},
	errorStrong:    color.RGBA{R: 255, G: 137, B: 137, A: 255},
	errorSurface:   color.RGBA{R: 75, G: 36, B: 47, A: 255},
	errorBorder:    color.RGBA{R: 92, G: 43, B: 56, A: 255},
	white:          color.RGBA{R: 255, G: 255, B: 255, A: 255},
}

var neurodeskPalette = interfacePalette{
	canvas:         color.RGBA{R: 12, G: 14, B: 10, A: 255},
	surface:        color.RGBA{R: 24, G: 32, B: 21, A: 255},
	surfaceRaised:  color.RGBA{R: 32, G: 42, B: 28, A: 255},
	surfaceHover:   color.RGBA{R: 41, G: 56, B: 36, A: 255},
	border:         color.RGBA{R: 67, G: 84, B: 57, A: 255},
	borderStrong:   color.RGBA{R: 91, G: 111, B: 80, A: 255},
	primary:        color.RGBA{R: 79, G: 123, B: 56, A: 255},
	primaryHover:   color.RGBA{R: 107, G: 164, B: 66, A: 255},
	accent:         color.RGBA{R: 158, G: 198, B: 114, A: 255},
	accentSoft:     color.RGBA{R: 211, G: 231, B: 182, A: 255},
	text:           color.RGBA{R: 243, G: 245, B: 239, A: 255},
	textSecondary:  color.RGBA{R: 194, G: 207, B: 184, A: 255},
	textMuted:      color.RGBA{R: 159, G: 172, B: 150, A: 255},
	disabled:       color.RGBA{R: 116, G: 128, B: 109, A: 255},
	success:        color.RGBA{R: 143, G: 226, B: 156, A: 255},
	successSurface: color.RGBA{R: 35, G: 69, B: 52, A: 255},
	warning:        color.RGBA{R: 255, G: 193, B: 111, A: 255},
	warningSurface: color.RGBA{R: 72, G: 52, B: 31, A: 255},
	error:          color.RGBA{R: 255, G: 151, B: 151, A: 255},
	errorStrong:    color.RGBA{R: 255, G: 137, B: 137, A: 255},
	errorSurface:   color.RGBA{R: 75, G: 36, B: 47, A: 255},
	errorBorder:    color.RGBA{R: 92, G: 43, B: 56, A: 255},
	white:          color.RGBA{R: 255, G: 255, B: 255, A: 255},
}

var activePalette = neurodeskPalette

var (
	uiCanvas         = activePalette.canvas
	uiSurface        = activePalette.surface
	uiSurfaceRaised  = activePalette.surfaceRaised
	uiSurfaceHover   = activePalette.surfaceHover
	uiBorder         = activePalette.border
	uiBorderStrong   = activePalette.borderStrong
	uiPrimary        = activePalette.primary
	uiPrimaryHover   = activePalette.primaryHover
	uiAccent         = activePalette.accent
	uiAccentSoft     = activePalette.accentSoft
	uiText           = activePalette.text
	uiTextSecondary  = activePalette.textSecondary
	uiTextMuted      = activePalette.textMuted
	uiDisabled       = activePalette.disabled
	uiSuccess        = activePalette.success
	uiSuccessSurface = activePalette.successSurface
	uiWarning        = activePalette.warning
	uiWarningSurface = activePalette.warningSurface
	uiError          = activePalette.error
	uiErrorStrong    = activePalette.errorStrong
	uiErrorSurface   = activePalette.errorSurface
	uiErrorBorder    = activePalette.errorBorder
	uiWhite          = activePalette.white
)

func applyTheme(theme Theme) {
	activePalette = neurodeskPalette
	if theme == ThemeSquadVM {
		activePalette = squadVMPalette
	}
	uiCanvas = activePalette.canvas
	uiSurface = activePalette.surface
	uiSurfaceRaised = activePalette.surfaceRaised
	uiSurfaceHover = activePalette.surfaceHover
	uiBorder = activePalette.border
	uiBorderStrong = activePalette.borderStrong
	uiPrimary = activePalette.primary
	uiPrimaryHover = activePalette.primaryHover
	uiAccent = activePalette.accent
	uiAccentSoft = activePalette.accentSoft
	uiText = activePalette.text
	uiTextSecondary = activePalette.textSecondary
	uiTextMuted = activePalette.textMuted
	uiDisabled = activePalette.disabled
	uiSuccess = activePalette.success
	uiSuccessSurface = activePalette.successSurface
	uiWarning = activePalette.warning
	uiWarningSurface = activePalette.warningSurface
	uiError = activePalette.error
	uiErrorStrong = activePalette.errorStrong
	uiErrorSurface = activePalette.errorSurface
	uiErrorBorder = activePalette.errorBorder
	uiWhite = activePalette.white
}

func backgroundFragmentShader() string {
	base := activePalette.canvas
	accent := activePalette.primaryHover
	return fmt.Sprintf(backgroundFragmentShaderTemplate,
		base.R, base.G, base.B, accent.R, accent.G, accent.B)
}
