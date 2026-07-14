package vmshdprotocol

import (
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/version"
)

const (
	Name    = "vmshd.frontend"
	Current = 1
	Minimum = 1

	HeaderProtocol    = "X-VMSH-Frontend-Protocol"
	HeaderMinProtocol = "X-VMSH-Frontend-Min-Protocol"
	HeaderName        = "X-VMSH-Frontend-Name"
)

type Info struct {
	Kind          string        `json:"kind"`
	Protocol      Range         `json:"protocol"`
	Daemon        DaemonRuntime `json:"daemon"`
	Compatibility Compatibility `json:"compatibility"`
	Routes        []Route       `json:"routes,omitempty"`
}

type Range struct {
	Name          string `json:"name"`
	Current       int    `json:"current"`
	Minimum       int    `json:"minimum"`
	SchemaVersion string `json:"schema_version,omitempty"`
}

type DaemonRuntime struct {
	Version    string           `json:"version"`
	Commit     string           `json:"commit,omitempty"`
	Dirty      bool             `json:"dirty,omitempty"`
	BuildDate  string           `json:"build_date,omitempty"`
	GoVersion  string           `json:"go_version,omitempty"`
	StartedAt  time.Time        `json:"started_at,omitempty"`
	Platform   string           `json:"platform"`
	Executable DaemonExecutable `json:"executable"`
}

type DaemonExecutable struct {
	Mode  string `json:"mode"`
	Path  string `json:"path,omitempty"`
	Argv0 string `json:"argv0,omitempty"`
	Name  string `json:"name,omitempty"`
}

type Compatibility struct {
	Compatible bool   `json:"compatible"`
	Action     string `json:"action"`
	Warning    string `json:"warning,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status string `json:"status,omitempty"`
}

func NewInfo(kind string, startedAt time.Time, executable DaemonExecutable, frontendMin, frontendCurrent int) Info {
	build := version.Current()
	info := Info{
		Kind: kind,
		Protocol: Range{
			Name:          Name,
			Current:       Current,
			Minimum:       Minimum,
			SchemaVersion: "0.1.0",
		},
		Daemon: DaemonRuntime{
			Version:    build.Version,
			Commit:     build.Commit,
			Dirty:      build.Dirty,
			BuildDate:  build.BuildDate,
			GoVersion:  build.GoVersion,
			StartedAt:  startedAt,
			Platform:   build.GOOS + "/" + build.GOARCH,
			Executable: executable,
		},
		Routes: CurrentRoutes(),
	}
	info.Compatibility = CompatibilityFor(frontendMin, frontendCurrent)
	return info
}

func CompatibilityFor(frontendMin, frontendCurrent int) Compatibility {
	if frontendMin <= 0 {
		frontendMin = Current
	}
	if frontendCurrent <= 0 {
		frontendCurrent = frontendMin
	}
	if frontendCurrent < Minimum {
		return Compatibility{
			Compatible: false,
			Action:     "start-private-daemon",
			Reason:     "frontend-too-old",
			Warning:    "existing vmshd requires a newer frontend protocol; starting a private daemon",
		}
	}
	if frontendMin > Current {
		return Compatibility{
			Compatible: false,
			Action:     "start-private-daemon",
			Reason:     "frontend-too-new",
			Warning:    "existing vmshd is too old for this frontend; starting a private daemon",
		}
	}
	return Compatibility{Compatible: true, Action: "reuse", Reason: "compatible"}
}

func CurrentRoutes() []Route {
	return []Route{
		{Method: "GET", Path: "/vmsh/protocol", Status: "current"},
		{Method: "GET", Path: "/vmsh/status", Status: "current"},
		{Method: "GET", Path: "/vmsh/frontends", Status: "current"},
		{Method: "POST", Path: "/vmsh/frontends", Status: "current"},
		{Method: "DELETE", Path: "/vmsh/frontends/{frontend_id}", Status: "current"},
		{Method: "GET", Path: "/vmsh/sessions", Status: "current"},
		{Method: "POST", Path: "/vmsh/sessions", Status: "current"},
		{Method: "GET", Path: "/vmsh/sessions/{session_id}", Status: "current"},
		{Method: "PATCH", Path: "/vmsh/sessions/{session_id}", Status: "current"},
		{Method: "DELETE", Path: "/vmsh/sessions/{session_id}", Status: "current"},
		{Method: "POST", Path: "/vmsh/sessions/{session_id}/attach", Status: "current"},
		{Method: "POST", Path: "/vmsh/sessions/{session_id}/detach", Status: "current"},
		{Method: "POST", Path: "/vmsh/sessions/{session_id}/persist", Status: "current"},
		{Method: "POST", Path: "/vmsh/sessions/{session_id}/attachments/{attachment_id}/terminal", Status: "current"},
		{Method: "GET", Path: "/vmsh/sessions/{session_id}/attachments/{attachment_id}/stream", Status: "current"},
		{Method: "GET", Path: "/vmsh/jobs", Status: "current"},
		{Method: "POST", Path: "/vmsh/sessions/{session_id}/jobs", Status: "current"},
		{Method: "DELETE", Path: "/vmsh/sessions/{session_id}/jobs/{job_id}", Status: "current"},
		{Method: "GET", Path: "/vmsh/events", Status: "current"},
	}
}

func ExecutableMode(executablePath, argv0 string, internalEnv bool) DaemonExecutable {
	name := strings.TrimSpace(filepath.Base(firstNonEmpty(argv0, executablePath)))
	mode := "unknown"
	if isDaemonExecutableName(name) {
		mode = "stable-daemon-copy"
	} else if internalEnv {
		mode = "embedded-frontend"
	}
	return DaemonExecutable{
		Mode:  mode,
		Path:  strings.TrimSpace(executablePath),
		Argv0: strings.TrimSpace(argv0),
		Name:  name,
	}
}

func IsDaemonExecutableName(path string) bool {
	return isDaemonExecutableName(filepath.Base(path))
}

func isDaemonExecutableName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(name, ".exe")
	}
	return name == "vmshd"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
