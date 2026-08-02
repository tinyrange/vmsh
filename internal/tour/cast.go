package tour

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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
			var label string
			if err := json.Unmarshal(event[2], &label); err != nil {
				return fmt.Errorf("event %d has invalid marker", eventIndex)
			}
		case "r":
			var size string
			if err := json.Unmarshal(event[2], &size); err != nil {
				return fmt.Errorf("event %d has invalid resize", eventIndex)
			}
			cols, rows, ok := strings.Cut(size, "x")
			parsedCols, colsErr := strconv.Atoi(cols)
			parsedRows, rowsErr := strconv.Atoi(rows)
			if !ok || colsErr != nil || rowsErr != nil || parsedCols <= 0 || parsedRows <= 0 {
				return fmt.Errorf("event %d has invalid resize", eventIndex)
			}
		case "vmsh":
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

type Redaction struct {
	Pattern     *regexp.Regexp
	Replacement string
}

func LiteralRedaction(value, replacement string) Redaction {
	return Redaction{Pattern: regexp.MustCompile(regexp.QuoteMeta(value)), Replacement: replacement}
}

func WordRedaction(value, replacement string) Redaction {
	return Redaction{
		Pattern:     regexp.MustCompile(`(^|[^[:alnum:]_])` + regexp.QuoteMeta(value) + `([^[:alnum:]_]|$)`),
		Replacement: `${1}` + replacement + `${2}`,
	}
}

func RedactCast(path string, redactions []Redaction) error {
	redactions = append([]Redaction(nil), redactions...)
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".vmsh-tour-redacted-*.cast")
	if err != nil {
		_ = input.Close()
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	cleanup := func() {
		_ = input.Close()
		_ = temporary.Close()
	}

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	encoder := json.NewEncoder(temporary)
	for scanner.Scan() {
		var value any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			cleanup()
			return fmt.Errorf("decode cast for redaction: %w", err)
		}
		if err := encoder.Encode(redactValue(value, redactions)); err != nil {
			cleanup()
			return fmt.Errorf("encode redacted cast: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		cleanup()
		return err
	}
	if err := input.Close(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func redactValue(value any, redactions []Redaction) any {
	switch value := value.(type) {
	case string:
		for _, redaction := range redactions {
			value = redaction.Pattern.ReplaceAllString(value, redaction.Replacement)
		}
		return value
	case []any:
		for index := range value {
			value[index] = redactValue(value[index], redactions)
		}
		return value
	case map[string]any:
		for key, item := range value {
			value[key] = redactValue(item, redactions)
		}
		return value
	default:
		return value
	}
}
