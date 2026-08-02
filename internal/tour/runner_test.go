package tour

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tinyrange/vmsh/internal/ptyterm"
	"github.com/tinyrange/vmsh/internal/termui/asciicast"
)

func TestRunProducesGuidedCastFromBehavioralSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tour test helper currently requires Unix terminal line discipline")
	}
	dir := t.TempDir()
	t.Setenv("VMSH_TOUR_PARENT_SECRET", "must-not-reach-tour")
	script := filepath.Join(dir, "example.star")
	if err := os.WriteFile(script, []byte(`
tour_id = "example-tour"
tour_title = "Example tour"
tour_description = "Exercises the PTY-backed tour runner."

def main(ctx):
    ctx.section("Start", "Type a command and verify its **result**.")
    ctx.wait_prompt()
    ctx.type("hello")
    ctx.enter()
    ctx.expect_line("RESULT=hello")
    ctx.wait_prompt()
    ctx.type("environment")
    ctx.enter()
    ctx.expect_line("PARENT_SECRET=absent")
    ctx.wait_prompt()
    if ctx.value("expected", "missing") != "configured":
        fail("tour value was not provided")

    ctx.section("Finish", "Exit the tested terminal cleanly.")
    ctx.type("exit")
    ctx.enter()
    ctx.wait_exit()
`), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "example.cast")
	result, err := Run(context.Background(), Options{
		ScriptPath: script,
		OutputPath: output,
		Command:    []string{os.Args[0], "-test.run=TestTourHelperProcess"},
		Env:        []string{"VMSH_TOUR_HELPER=1"},
		Size:       ptyterm.Size{Cols: 80, Rows: 24},
		Timeout:    10 * time.Second,
		Version:    "v1.2.3",
		Commit:     "abc123",
		Values:     map[string]string{"expected": "configured"},
	})
	if err != nil {
		t.Fatalf("run tour: %v", err)
	}
	if result.ID != "example-tour" || result.Sections != 2 || result.Output != output {
		t.Fatalf("result = %+v", result)
	}

	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var header asciicast.Header
	if err := decoder.Decode(&header); err != nil {
		t.Fatal(err)
	}
	if header.VMSHTour == nil || header.VMSHTour.ID != "example-tour" || header.VMSHTour.VMSHVersion != "v1.2.3" || header.VMSHTour.Commit != "abc123" {
		t.Fatalf("tour header = %+v", header.VMSHTour)
	}
	sections := 0
	haveInput := false
	haveOutput := false
	for {
		var event []any
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(event) != 3 {
			continue
		}
		switch event[1] {
		case "i":
			haveInput = true
		case "o":
			haveOutput = true
		case "vmsh":
			payload, _ := event[2].(map[string]any)
			if payload["name"] == "vmsh.tour.section" {
				sections++
			}
		}
	}
	if sections != 2 || !haveInput || !haveOutput {
		t.Fatalf("cast events: sections=%d input=%v output=%v", sections, haveInput, haveOutput)
	}
}

func TestRedactCastRemovesPrivateHostIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.cast")
	data := "{\"version\":2,\"width\":80,\"height\":24,\"vmsh_tour\":{\"schema\":1,\"id\":\"private-tour\",\"title\":\"Private tour\"}}\n" +
		"[0.1,\"o\",\"alice@workstation:/Users/alice/project $ pwd\\r\\n/Users/alice/project\\r\\n\"]\n" +
		"[0.2,\"m\",\"Finish\"]\n" +
		"[0.2,\"vmsh\",{\"name\":\"vmsh.tour.section\",\"fields\":{\"title\":\"Finish\",\"markdown\":\"Done\"}}]\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	redactions := []Redaction{
		LiteralRedaction("/Users/alice/project", "/workspace"),
		LiteralRedaction("/Users/alice", "/home/user"),
		WordRedaction("alice", "user"),
		WordRedaction("workstation", "host"),
	}
	if err := RedactCast(path, redactions); err != nil {
		t.Fatal(err)
	}
	redacted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(redacted)
	for _, private := range []string{"alice", "workstation", "/Users/alice", "/Users/alice/project"} {
		if strings.Contains(text, private) {
			t.Fatalf("redacted cast still contains %q: %s", private, text)
		}
	}
	for _, replacement := range []string{"user@host", "/workspace"} {
		if !strings.Contains(text, replacement) {
			t.Fatalf("redacted cast does not contain %q: %s", replacement, text)
		}
	}
}

func TestTourHelperProcess(t *testing.T) {
	if os.Getenv("VMSH_TOUR_HELPER") != "1" {
		return
	}
	fmt.Print("\x1b]0;vmsh host:fixture\x07\x1b[?2004h")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "exit":
			os.Exit(0)
		case "hello":
			fmt.Print("\x1b[?2004l\r\nRESULT=hello\r\n\x1b[?2004h")
		case "environment":
			secret := "absent"
			if os.Getenv("VMSH_TOUR_PARENT_SECRET") != "" {
				secret = "inherited"
			}
			fmt.Printf("\x1b[?2004l\r\nPARENT_SECRET=%s\r\n\x1b[?2004h", secret)
		default:
			fmt.Printf("\x1b[?2004l\r\nRESULT=unexpected:%s\r\n\x1b[?2004h", scanner.Text())
		}
	}
	os.Exit(0)
}
