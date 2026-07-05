package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	Release   string
	Commit    string
	BuildDate string
	Dirty     string
)

type Info struct {
	Version   string
	Commit    string
	Dirty     bool
	BuildDate string
	GoVersion string
	GOOS      string
	GOARCH    string
}

func Current() Info {
	info := Info{
		Version:   firstNonEmpty(strings.TrimSpace(Release), "devel"),
		Commit:    strings.TrimSpace(Commit),
		BuildDate: strings.TrimSpace(BuildDate),
		GoVersion: runtime.Version(),
		GOOS:      runtime.GOOS,
		GOARCH:    runtime.GOARCH,
	}
	if value, ok := parseBool(strings.TrimSpace(Dirty)); ok {
		info.Dirty = value
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		info = fillFromBuildInfo(info, build)
	}
	return info
}

func fillFromBuildInfo(info Info, build *debug.BuildInfo) Info {
	if info.Version == "devel" && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	if info.GoVersion == "" {
		info.GoVersion = build.GoVersion
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = setting.Value
			}
		case "vcs.modified":
			if _, ok := parseBool(strings.TrimSpace(Dirty)); !ok {
				if value, parsed := parseBool(setting.Value); parsed {
					info.Dirty = value
				}
			}
		case "vcs.time":
			if info.BuildDate == "" {
				info.BuildDate = setting.Value
			}
		}
	}
	return info
}

func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "vmsh version %s\n", emptyText(i.Version, "devel"))
	writeField(&b, "commit", i.Commit)
	fmt.Fprintf(&b, "dirty %t\n", i.Dirty)
	writeField(&b, "built", i.BuildDate)
	writeField(&b, "go", i.GoVersion)
	fmt.Fprintf(&b, "platform %s/%s\n", emptyText(i.GOOS, runtime.GOOS), emptyText(i.GOARCH, runtime.GOARCH))
	return b.String()
}

func writeField(b *strings.Builder, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	fmt.Fprintf(b, "%s %s\n", key, value)
}

func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func emptyText(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}
