package desktopapp

import (
	"fmt"
	"strings"
)

// Config contains the small set of product decisions that differ between the
// desktop applications. Runtime, setup, display, input, update, and lifecycle
// behavior lives in this package so fixes are shared by every branded app.
type Config struct {
	ProductName                        string
	Subtitle                           string
	Kind                               string
	Theme                              Theme
	DefaultVMName                      string
	DefaultImage                       string
	DefaultStorage                     string
	GuestStorageMount                  string
	DefaultUser                        string
	DefaultMemoryMB                    uint64
	DefaultCPUs                        int
	DefaultEphemeralHome               bool
	AMD64Emulation                     bool
	BrandPNG                           []byte
	ConfigDirName                      string
	DataDirName                        string
	ImageNamespace                     string
	CacheImageDir                      string
	DesktopReadiness                   string
	SSHHost                            string
	SSHUser                            string
	SSHHome                            string
	ReleaseAssetPrefix                 string
	ExperimentalCompressedOCI          bool
	ExperimentalBackgroundImageUpdates bool
	CVMFSHostMount                     *CVMFSHostMountConfig
	DesktopWebApp                      *DesktopWebAppConfig
}

// DesktopWebAppConfig describes a guest web application which is started by
// the user from the guest desktop and opened by the host once it is ready.
type DesktopWebAppConfig struct {
	GuestPort  int
	StatusPath string
	URLPath    string
}

type CVMFSHostMountConfig struct {
	Mount           string
	Mirror          string
	Mirrors         []string
	Repo            string
	Path            string
	CacheLimitBytes int64
}

var appConfig Config

func normalizeConfig(config Config) (Config, error) {
	if strings.TrimSpace(config.ProductName) == "" {
		return Config{}, fmt.Errorf("desktop app product name cannot be empty")
	}
	if config.Kind == "" {
		config.Kind = strings.ToLower(config.ProductName)
	}
	if config.Theme == "" {
		if config.Kind == "ndappx" {
			config.Theme = ThemeNeurodesk
		} else {
			config.Theme = ThemeSquadVM
		}
	}
	if config.DefaultVMName == "" {
		config.DefaultVMName = config.Kind
	}
	if config.DefaultStorage == "" {
		config.DefaultStorage = "~/" + config.DefaultVMName + "-shared"
	}
	if config.GuestStorageMount == "" {
		config.GuestStorageMount = "/shared"
	}
	if config.DefaultUser == "" {
		config.DefaultUser = "root"
	}
	if config.DefaultMemoryMB == 0 {
		config.DefaultMemoryMB = 4096
	}
	if config.DefaultCPUs <= 0 {
		config.DefaultCPUs = 4
	}
	if config.ConfigDirName == "" {
		config.ConfigDirName = config.ProductName
	}
	if config.DataDirName == "" {
		config.DataDirName = config.ProductName + "-data"
	}
	if config.ImageNamespace == "" {
		config.ImageNamespace = config.Kind
	}
	if config.CacheImageDir == "" {
		config.CacheImageDir = config.ImageNamespace
	}
	if config.SSHHost == "" {
		config.SSHHost = config.DefaultVMName
	}
	if config.SSHUser == "" {
		config.SSHUser = config.DefaultUser
	}
	if config.SSHHome == "" {
		config.SSHHome = "/root"
	}
	if config.ReleaseAssetPrefix == "" {
		config.ReleaseAssetPrefix = config.ProductName
	}
	if config.DesktopWebApp != nil {
		if config.DesktopWebApp.GuestPort <= 0 || config.DesktopWebApp.GuestPort > 65535 {
			return Config{}, fmt.Errorf("desktop web app guest port must be between 1 and 65535")
		}
		if config.DesktopWebApp.StatusPath == "" {
			config.DesktopWebApp.StatusPath = "/api/status"
		}
		if config.DesktopWebApp.URLPath == "" {
			config.DesktopWebApp.URLPath = "/"
		}
		if !strings.HasPrefix(config.DesktopWebApp.StatusPath, "/") || !strings.HasPrefix(config.DesktopWebApp.URLPath, "/") {
			return Config{}, fmt.Errorf("desktop web app paths must begin with /")
		}
	}
	return config, nil
}

func productName() string { return appConfig.ProductName }

func productSubtitle() string { return appConfig.Subtitle }
