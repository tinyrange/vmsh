package desktopapp

import (
	"image/color"
	"math"
	"testing"
)

func TestInterfaceTextMeetsReadableContrast(t *testing.T) {
	tests := []struct {
		name       string
		foreground color.RGBA
		background color.RGBA
		minimum    float64
	}{
		{name: "primary text", foreground: uiText, background: uiCanvas, minimum: 7},
		{name: "secondary text", foreground: uiTextSecondary, background: uiSurface, minimum: 4.5},
		{name: "accent text", foreground: uiAccent, background: uiCanvas, minimum: 4.5},
		{name: "primary button", foreground: uiWhite, background: uiPrimary, minimum: 4.5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if ratio := contrastRatio(test.foreground, test.background); ratio < test.minimum {
				t.Fatalf("contrast ratio %.2f is below %.2f", ratio, test.minimum)
			}
		})
	}
}

func TestProductThemesRemainDistinct(t *testing.T) {
	t.Cleanup(func() { applyTheme(ThemeNeurodesk) })

	applyTheme(ThemeSquadVM)
	squadPrimary := uiPrimary
	squadBackground := backgroundFragmentShader()
	applyTheme(ThemeNeurodesk)
	neurodeskPrimary := uiPrimary
	neurodeskBackground := backgroundFragmentShader()

	if squadPrimary == neurodeskPrimary || squadBackground == neurodeskBackground {
		t.Fatal("SquadVM and Neurodesk resolved to the same interface theme")
	}
	if squadPrimary.B <= squadPrimary.G {
		t.Fatalf("SquadVM primary color lost its purple identity: %v", squadPrimary)
	}
	if neurodeskPrimary.G <= neurodeskPrimary.B {
		t.Fatalf("Neurodesk primary color lost its green identity: %v", neurodeskPrimary)
	}
}

func contrastRatio(left, right color.RGBA) float64 {
	leftLuminance := relativeLuminance(left)
	rightLuminance := relativeLuminance(right)
	lighter := math.Max(leftLuminance, rightLuminance)
	darker := math.Min(leftLuminance, rightLuminance)
	return (lighter + 0.05) / (darker + 0.05)
}

func relativeLuminance(value color.RGBA) float64 {
	channel := func(component uint8) float64 {
		normalized := float64(component) / 255
		if normalized <= 0.04045 {
			return normalized / 12.92
		}
		return math.Pow((normalized+0.055)/1.055, 2.4)
	}
	return 0.2126*channel(value.R) + 0.7152*channel(value.G) + 0.0722*channel(value.B)
}
