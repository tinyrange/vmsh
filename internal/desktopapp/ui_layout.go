package desktopapp

import "image"

const (
	uiOuterMargin       = 20
	uiCompactMargin     = 16
	uiSettingsTopInset  = 12
	uiPanelMaxWidth     = 1080
	uiPanelWidthRatio   = 0.75
	uiSettingsHeight    = 468
	uiBrandSize         = 48
	uiCardGap           = 8
	uiStatusCardHeight  = 54
	uiOptionCardHeight  = 54
	uiPrimaryButtonSize = 220
	uiSkipButtonSize    = 156
	uiButtonHeight      = 50
)

type startupControl uint8

const (
	startupControlNone startupControl = iota
	startupControlSSH
	startupControlSystem
	startupControlAdvanced
	startupControlSharedFolder
	startupControlMemory
	startupControlCPUs
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
	advanced       image.Rectangle
	sharedFolder   image.Rectangle
	sharedBrowse   image.Rectangle
	advancedPanel  image.Rectangle
	memorySlider   image.Rectangle
	cpuSlider      image.Rectangle
	actionDivider  image.Rectangle
	skip           image.Rectangle
	button         image.Rectangle
}

func settingsControlLayout(width, height float32) startupControlLayout {
	return settingsControlLayoutForState(width, height, false)
}

func settingsControlLayoutForState(width, height float32, advancedExpanded bool) startupControlLayout {
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

	statusTop := top + 154
	statusWidth := (panelWidth - uiCardGap) / 2
	compactExpanded := advancedExpanded && height < top+uiSettingsHeight+148
	if !compactExpanded {
		for index := range layout.status {
			column := index % 2
			row := index / 2
			x := left + float32(column)*(statusWidth+uiCardGap)
			y := statusTop + float32(row)*(uiStatusCardHeight+uiCardGap)
			layout.status[index] = image.Rect(
				int(x), int(y), int(x+statusWidth), int(y+uiStatusCardHeight),
			)
		}
	}

	optionTop := statusTop
	if !compactExpanded {
		optionTop += 2*(uiStatusCardHeight+uiCardGap) + 2
	}
	optionWidth := (panelWidth - 2*uiCardGap) / 3
	layout.sshCheckbox = image.Rect(
		int(left), int(optionTop), int(left+optionWidth), int(optionTop+uiOptionCardHeight),
	)
	layout.systemCheckbox = image.Rect(
		int(left+optionWidth+uiCardGap), int(optionTop), int(left+2*optionWidth+uiCardGap), int(optionTop+uiOptionCardHeight),
	)
	layout.advanced = image.Rect(
		int(left+2*(optionWidth+uiCardGap)), int(optionTop), int(right), int(optionTop+uiOptionCardHeight),
	)
	sharedTop := optionTop + uiOptionCardHeight + 10
	if advancedExpanded {
		advancedHeight := float32(148)
		sliderTop := float32(70)
		if compactExpanded {
			advancedHeight = 138
			sliderTop = 62
		}
		layout.advancedPanel = image.Rect(int(left), int(sharedTop), int(right), int(sharedTop+advancedHeight))
		midpoint := left + panelWidth/2
		layout.memorySlider = image.Rect(int(left+22), int(sharedTop+sliderTop), int(midpoint-12), int(sharedTop+sliderTop+44))
		layout.cpuSlider = image.Rect(int(midpoint+12), int(sharedTop+sliderTop), int(right-22), int(sharedTop+sliderTop+44))
		sharedTop += advancedHeight + 10
	}
	layout.sharedFolder = image.Rect(int(left), int(sharedTop), int(right), int(sharedTop+uiOptionCardHeight))
	browseWidth := min(float32(148), panelWidth*0.34)
	layout.sharedBrowse = image.Rect(
		int(right-browseWidth-5), int(sharedTop+5), int(right-5), int(sharedTop+uiOptionCardHeight-5),
	)

	actionTop := sharedTop + uiOptionCardHeight + 20
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
	layout.panel.Max.Y = layout.button.Max.Y
	return layout
}

func startupControlAt(point image.Point, layout startupControlLayout, showSkip bool) startupControl {
	switch {
	case point.In(layout.sshCheckbox):
		return startupControlSSH
	case point.In(layout.systemCheckbox):
		return startupControlSystem
	case point.In(layout.advanced):
		return startupControlAdvanced
	case point.In(layout.sharedBrowse):
		return startupControlSharedFolder
	case showSkip && point.In(layout.skip):
		return startupControlSkip
	case point.In(layout.button):
		return startupControlPrimary
	default:
		return startupControlNone
	}
}

func advancedControlAt(point image.Point, layout startupControlLayout) startupControl {
	switch {
	case point.In(layout.memorySlider):
		return startupControlMemory
	case point.In(layout.cpuSlider):
		return startupControlCPUs
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
	contentHeight := float32(370)
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

func startupControlOrder(showSkip, advancedExpanded bool) []startupControl {
	controls := []startupControl{startupControlSSH, startupControlSystem, startupControlAdvanced, startupControlSharedFolder}
	if advancedExpanded {
		controls = []startupControl{startupControlSSH, startupControlSystem, startupControlAdvanced, startupControlMemory, startupControlCPUs, startupControlSharedFolder}
	}
	if showSkip {
		controls = append(controls, startupControlSkip)
	}
	return append(controls, startupControlPrimary)
}

func nextStartupControl(current startupControl, reverse, showSkip, advancedExpanded bool) startupControl {
	controls := startupControlOrder(showSkip, advancedExpanded)
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
