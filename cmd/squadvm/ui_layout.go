package main

import "image"

const (
	uiOuterMargin       = 20
	uiCompactMargin     = 16
	uiSettingsTopInset  = 12
	uiPanelMaxWidth     = 1080
	uiPanelWidthRatio   = 0.75
	uiSettingsHeight    = 452
	uiBrandSize         = 48
	uiCardGap           = 10
	uiStatusCardHeight  = 64
	uiOptionCardHeight  = 60
	uiPrimaryButtonSize = 220
	uiSkipButtonSize    = 156
	uiButtonHeight      = 50
)

type startupControl uint8

const (
	startupControlNone startupControl = iota
	startupControlSSH
	startupControlSystem
	startupControlSkip
	startupControlPrimary
)

type startupControlLayout struct {
	panel          image.Rectangle
	brand          image.Rectangle
	state          image.Rectangle
	status         [4]image.Rectangle
	sshCheckbox    image.Rectangle
	systemCheckbox image.Rectangle
	actionDivider  image.Rectangle
	skip           image.Rectangle
	button         image.Rectangle
}

func settingsControlLayout(width, height float32) startupControlLayout {
	margin := float32(uiOuterMargin)
	if width < 800 {
		margin = uiCompactMargin
	}
	panelWidth := max(float32(1), min(float32(uiPanelMaxWidth), min(width*uiPanelWidthRatio, width-margin*2)))
	left := (width - panelWidth) / 2
	top := margin + uiSettingsTopInset
	right := left + panelWidth

	layout := startupControlLayout{
		panel: image.Rect(int(left), int(top), int(right), int(top+uiSettingsHeight)),
		brand: image.Rect(int(left), int(top), int(left+uiBrandSize), int(top+uiBrandSize)),
		state: image.Rect(int(right-144), int(top+5), int(right), int(top+39)),
	}

	statusTop := top + 164
	statusWidth := (panelWidth - uiCardGap) / 2
	for index := range layout.status {
		column := index % 2
		row := index / 2
		x := left + float32(column)*(statusWidth+uiCardGap)
		y := statusTop + float32(row)*(uiStatusCardHeight+uiCardGap)
		layout.status[index] = image.Rect(
			int(x), int(y), int(x+statusWidth), int(y+uiStatusCardHeight),
		)
	}

	optionTop := statusTop + 2*(uiStatusCardHeight+uiCardGap) + 2
	optionWidth := statusWidth
	layout.sshCheckbox = image.Rect(
		int(left), int(optionTop), int(left+optionWidth), int(optionTop+uiOptionCardHeight),
	)
	layout.systemCheckbox = image.Rect(
		int(left+optionWidth+uiCardGap), int(optionTop), int(right), int(optionTop+uiOptionCardHeight),
	)

	actionTop := optionTop + uiOptionCardHeight + 28
	layout.actionDivider = image.Rect(int(left), int(actionTop-14), int(right), int(actionTop-13))
	primaryWidth := min(float32(uiPrimaryButtonSize), panelWidth)
	skipWidth := min(float32(uiSkipButtonSize), max(float32(1), panelWidth-primaryWidth-uiCardGap))
	layout.button = image.Rect(
		int(right-primaryWidth), int(actionTop), int(right), int(actionTop+uiButtonHeight),
	)
	layout.skip = image.Rect(
		int(right-primaryWidth-uiCardGap-skipWidth), int(actionTop),
		int(right-primaryWidth-uiCardGap), int(actionTop+uiButtonHeight),
	)
	return layout
}

func startupControlAt(point image.Point, layout startupControlLayout, showSkip bool) startupControl {
	switch {
	case point.In(layout.sshCheckbox):
		return startupControlSSH
	case point.In(layout.systemCheckbox):
		return startupControlSystem
	case showSkip && point.In(layout.skip):
		return startupControlSkip
	case point.In(layout.button):
		return startupControlPrimary
	default:
		return startupControlNone
	}
}

type startupScreenLayout struct {
	panel image.Rectangle
	brand image.Rectangle
	state image.Rectangle
	bar   image.Rectangle
	steps [3]image.Rectangle
}

func calculateStartupScreenLayout(width, height float32) startupScreenLayout {
	margin := float32(uiOuterMargin)
	if width < 800 {
		margin = uiCompactMargin
	}
	panelWidth := max(float32(1), min(float32(960), min(width*uiPanelWidthRatio, width-margin*2)))
	left := (width - panelWidth) / 2
	contentHeight := float32(348)
	top := max(margin, float32(46))
	right := left + panelWidth

	layout := startupScreenLayout{
		panel: image.Rect(int(left), int(top), int(right), int(top+contentHeight)),
		brand: image.Rect(int(left), int(top), int(left+uiBrandSize), int(top+uiBrandSize)),
		state: image.Rect(int(right-144), int(top+5), int(right), int(top+39)),
		bar:   image.Rect(int(left), int(top+184), int(right), int(top+194)),
	}
	stepTop := top + 250
	stepWidth := (panelWidth - 2*uiCardGap) / 3
	for index := range layout.steps {
		x := left + float32(index)*(stepWidth+uiCardGap)
		layout.steps[index] = image.Rect(int(x), int(stepTop), int(x+stepWidth), int(stepTop+42))
	}
	return layout
}

func startupControlOrder(showSkip bool) []startupControl {
	controls := []startupControl{startupControlSSH, startupControlSystem}
	if showSkip {
		controls = append(controls, startupControlSkip)
	}
	return append(controls, startupControlPrimary)
}

func nextStartupControl(current startupControl, reverse, showSkip bool) startupControl {
	controls := startupControlOrder(showSkip)
	if len(controls) == 0 {
		return startupControlNone
	}
	index := -1
	for candidateIndex, candidate := range controls {
		if candidate == current {
			index = candidateIndex
			break
		}
	}
	if reverse {
		if index <= 0 {
			return controls[len(controls)-1]
		}
		return controls[index-1]
	}
	if index < 0 || index == len(controls)-1 {
		return controls[0]
	}
	return controls[index+1]
}
