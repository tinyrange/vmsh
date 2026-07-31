package tour

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/tinyrange/vmsh/internal/termui/asciicast"
)

func ValidateCast(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var header asciicast.Header
	if err := decoder.Decode(&header); err != nil {
		return fmt.Errorf("decode header: %w", err)
	}
	if header.Version != 2 || header.Width <= 0 || header.Height <= 0 {
		return fmt.Errorf("invalid asciinema v2 header dimensions %dx%d", header.Width, header.Height)
	}
	if header.VMSHTour == nil || header.VMSHTour.Schema != SchemaVersion || !validTourID.MatchString(header.VMSHTour.ID) || header.VMSHTour.Title == "" {
		return fmt.Errorf("invalid vmsh tour header")
	}

	lastTime := -1.0
	sections := 0
	outputEvents := 0
	for eventIndex := 0; ; eventIndex++ {
		var event []json.RawMessage
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decode event %d: %w", eventIndex, err)
		}
		if len(event) != 3 {
			return fmt.Errorf("event %d must contain time, kind, and data", eventIndex)
		}
		var at float64
		var kind string
		if err := json.Unmarshal(event[0], &at); err != nil || at < lastTime {
			return fmt.Errorf("event %d has invalid or decreasing time", eventIndex)
		}
		lastTime = at
		if err := json.Unmarshal(event[1], &kind); err != nil {
			return fmt.Errorf("event %d has invalid kind", eventIndex)
		}
		switch kind {
		case "o":
			outputEvents++
		case "i":
		case "m":
			var metadata struct {
				Name   string `json:"name"`
				Fields struct {
					Title    string `json:"title"`
					Markdown string `json:"markdown"`
				} `json:"fields"`
			}
			if err := json.Unmarshal(event[2], &metadata); err != nil {
				return fmt.Errorf("event %d has invalid metadata", eventIndex)
			}
			if metadata.Name == "vmsh.tour.section" {
				if metadata.Fields.Title == "" || metadata.Fields.Markdown == "" {
					return fmt.Errorf("event %d has an empty tour section", eventIndex)
				}
				sections++
			}
		default:
			return fmt.Errorf("event %d has unsupported kind %q", eventIndex, kind)
		}
	}
	if sections == 0 || outputEvents == 0 {
		return fmt.Errorf("tour cast requires guided sections and terminal output")
	}
	return nil
}
