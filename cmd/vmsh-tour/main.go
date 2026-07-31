package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/ptyterm"
	"github.com/tinyrange/vmsh/internal/tour"
)

type valuesFlag map[string]string

func (v valuesFlag) String() string { return "name=value" }
func (v valuesFlag) Set(value string) error {
	name, item, ok := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return fmt.Errorf("tour value must use name=value")
	}
	v[name] = item
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vmsh-tour:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("vmsh-tour", flag.ContinueOnError)
	vmsh := fs.String("vmsh", "vmsh", "Path to the vmsh executable")
	ccvm := fs.String("ccvm", "", "Path to the ccvm executable")
	cacheDir := fs.String("cache-dir", "", "vmsh cache directory shared by tour runs")
	out := fs.String("out", "", "Output cast path for one tour")
	outDir := fs.String("out-dir", "", "Output directory for one or more tours")
	version := fs.String("version", "", "vmsh release version recorded in the cast")
	commit := fs.String("commit", "", "source commit recorded in the cast")
	timeout := fs.Duration("timeout", 5*time.Minute, "maximum duration of each tour")
	typeDelay := fs.Duration("type-delay", 45*time.Millisecond, "delay between typed runes")
	enterDelay := fs.Duration("enter-delay", 350*time.Millisecond, "pause after typing before Enter")
	sectionDelay := fs.Duration("section-delay", 650*time.Millisecond, "pause after revealing a guided section")
	cols := fs.Int("cols", 100, "terminal columns")
	rows := fs.Int("rows", 30, "terminal rows")
	values := valuesFlag{}
	fs.Var(values, "var", "Tour value as name=value; repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	scripts := fs.Args()
	if len(scripts) == 0 {
		return fmt.Errorf("usage: vmsh-tour [flags] tour.star [tour.star ...]")
	}
	if len(scripts) > 1 && strings.TrimSpace(*out) != "" {
		return fmt.Errorf("-out can only be used with one tour; use -out-dir for multiple tours")
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.MkdirTemp("", "vmsh-tour-home-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	resolvedCache := strings.TrimSpace(*cacheDir)
	if resolvedCache == "" {
		resolvedCache = filepath.Join(home, "cache")
	}
	command := []string{*vmsh, "-cache-dir", resolvedCache}
	if strings.TrimSpace(*ccvm) != "" {
		command = append(command, "-ccvm", *ccvm)
	}
	env := []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
		"NO_COLOR=1",
		"TERMUI_REDUCED_MOTION=1",
		"SHELL=/bin/sh",
	}

	for _, script := range scripts {
		output := strings.TrimSpace(*out)
		if output == "" {
			dir := strings.TrimSpace(*outDir)
			if dir == "" {
				dir = filepath.Dir(script)
			}
			name := strings.TrimSuffix(filepath.Base(script), filepath.Ext(script)) + ".cast"
			output = filepath.Join(dir, name)
		}
		result, err := tour.Run(context.Background(), tour.Options{
			ScriptPath:   script,
			OutputPath:   output,
			Command:      command,
			Dir:          workingDir,
			Env:          env,
			Size:         ptyterm.Size{Cols: *cols, Rows: *rows},
			Timeout:      *timeout,
			TypeDelay:    *typeDelay,
			EnterDelay:   *enterDelay,
			SectionDelay: *sectionDelay,
			Version:      *version,
			Commit:       *commit,
			Values:       values,
		})
		if err != nil {
			return err
		}
		fmt.Printf("generated %s (%d sections) at %s\n", result.Title, result.Sections, result.Output)
	}
	return nil
}
