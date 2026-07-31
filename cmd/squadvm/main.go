package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"j5.nz/cc/client"
	ccdisplay "j5.nz/cc/display"
)

const defaultSquadVMImage = "ghcr.io/tinyrange/squadvm:edge"

func main() {
	if err := run(platformArguments(os.Args[1:])); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "SquadVM:", err)
		os.Exit(1)
	}
}

func run(args []string) (retErr error) {
	fs := flag.NewFlagSet("SquadVM", flag.ContinueOnError)
	name := fs.String("name", "squadvm", "VM name")
	home := fs.String("home", "", "Persistent home identity (defaults to the VM name)")
	ephemeralHome := fs.Bool("ephemeral-home", platformDefaultEphemeralHome(), "Discard home-directory changes when the VM stops")
	storage := fs.String("storage", "~/squadvm-shared", "Host directory shared at /shared")
	user := fs.String("user", "root", "Default command user")
	cacheDir := fs.String("cache-dir", "", "Image and runtime cache directory")
	refreshImage := fs.Bool("refresh-image", false, "Refresh and rebuild the cached image")
	vnc := fs.Bool("vnc", false, "Use a VNC client instead of the native graphics window")
	vncListen := fs.String("vnc-listen", "127.0.0.1:0", "VNC listen address (requires --vnc)")
	vncPassword := fs.String("vnc-password", "", "VNC password (generated when omitted; requires --vnc)")
	displaySize := fs.String("display", "1440x900", "Initial display size WIDTHxHEIGHT")
	initSystem := fs.String("init", "systemd", "Guest init system")
	memoryMB := fs.Uint64("memory-mb", 4096, "Guest memory in MiB")
	cpus := fs.Int("cpus", platformDefaultCPUs(), "Guest CPU count")
	network := fs.Bool("network", true, "Enable isolated networking with outbound internet access")
	bootTimeout := fs.Duration("boot-timeout", 10*time.Minute, "VM preparation and boot timeout")
	dmesg := fs.Bool("dmesg", false, "Forward the guest kernel log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: SquadVM [OPTIONS] IMAGE")
	}
	if strings.TrimSpace(*name) == "" {
		return fmt.Errorf("VM name cannot be empty")
	}
	if *memoryMB == 0 {
		return fmt.Errorf("memory must be greater than zero")
	}
	if *cpus <= 0 {
		return fmt.Errorf("CPU count must be greater than zero")
	}
	var vncOptionSet bool
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "vnc-listen" || item.Name == "vnc-password" {
			vncOptionSet = true
		}
	})
	if !*vnc && vncOptionSet {
		return fmt.Errorf("--vnc-listen and --vnc-password require --vnc")
	}
	if len(*vncPassword) > 8 {
		return fmt.Errorf("VNC passwords are limited to 8 bytes")
	}
	if *vnc && *vncPassword == "" {
		generated, err := generateVNCPassword()
		if err != nil {
			return err
		}
		*vncPassword = generated
	}
	persistentMounts, persistentHome, err := squadvmPersistentHome(*name, *home, *ephemeralHome)
	if err != nil {
		return err
	}
	storageShare, err := squadvmStorageShare(*storage)
	if err != nil {
		return err
	}
	width, height, err := parseDisplaySize(*displaySize)
	if err != nil {
		return err
	}
	displayReady := make(chan ccdisplay.Session, 1)
	var openGLShareContext atomic.Uintptr
	var openGLSharePixelFormat atomic.Uintptr
	openGLShareGroup := func() (context, pixelFormat uintptr) {
		return openGLShareContext.Load(), openGLSharePixelFormat.Load()
	}
	publishOpenGLShareGroup := func(context, pixelFormat uintptr) {
		openGLShareContext.Store(context)
		openGLSharePixelFormat.Store(pixelFormat)
	}
	var (
		settings       squadVMSettings
		settingsDir    string
		systemInstall  bool
		activeCacheDir string
	)
	if strings.TrimSpace(*cacheDir) != "" {
		activeCacheDir, err = resolveSquadVMCacheDir(*cacheDir, false)
		if err != nil {
			return err
		}
		settingsDir = filepath.Join(activeCacheDir, "frontend")
		settings, err = loadSquadVMSettingsFromDir(settingsDir)
		if err != nil {
			return err
		}
	} else {
		settings, settingsDir, err = loadSquadVMSettings()
		if err != nil {
			return err
		}
		systemInstall = settings.systemInstall()
		activeCacheDir, err = resolveSquadVMCacheDir("", systemInstall)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(*cacheDir) == "" && !systemInstall {
		regularCacheDir, regularErr := resolveSquadVMCacheDir("", true)
		if regularErr == nil && filepath.Clean(activeCacheDir) == filepath.Clean(regularCacheDir) {
			// A populated cache from an existing installation takes precedence
			// over the new portable default. Keep the checkbox honest about the
			// location that is actually in use.
			systemInstall = true
		}
	}
	backend, err := startEmbeddedSquadVMBackend(activeCacheDir, *name, displayReady, openGLShareGroup)
	if err != nil && strings.TrimSpace(*cacheDir) == "" && !systemInstall {
		// A portable directory may be unavailable beside an installed app.
		// Keep the first-run UI reachable by falling back to the user cache and
		// presenting the system-install checkbox as selected.
		systemInstall = true
		activeCacheDir, err = resolveSquadVMCacheDir("", true)
		if err == nil {
			backend, err = startEmbeddedSquadVMBackend(activeCacheDir, *name, displayReady, openGLShareGroup)
		}
	}
	if err != nil {
		return err
	}
	api := backend.api
	// Some launch environments forward Control-C from the focused native
	// graphics window to the process's controlling terminal as SIGINT. Do not
	// let a guest keyboard shortcut tear down the VM. Native sessions stop when
	// their window closes; SIGTERM remains available for process supervision.
	signals := []os.Signal{syscall.SIGTERM}
	if *vnc {
		signals = append(signals, os.Interrupt)
	} else {
		signal.Ignore(os.Interrupt)
		defer signal.Reset(os.Interrupt)
	}
	lifetimeContext, stopSignals := signal.NotifyContext(context.Background(), signals...)
	defer stopSignals()
	defer func() {
		if err := backend.stop(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	var networkConfig *client.NetworkConfig
	if *network {
		networkConfig = &client.NetworkConfig{
			Enabled:       true,
			AllowInternet: true,
		}
	}
	vncAddress := ""
	password := ""
	if *vnc {
		vncAddress = *vncListen
		password = *vncPassword
	}
	// The arm64 image carries qemu-x86_64 and its binfmt declaration. Ensure
	// the VM kernel also has binfmt_misc available so systemd can register the
	// interpreter during early boot.
	kernelModules := squadVMKernelModules(runtimeArchitecture())
	request := client.CreateInstanceRequest{
		DefaultUser: *user,
		InitSystem:  *initSystem,
		Network:     networkConfig,
		Display: &client.DisplayConfig{
			Width:       uint32(width),
			Height:      uint32(height),
			VNCListen:   vncAddress,
			VNCPassword: password,
		},
		Shares:           []client.ShareMount{storageShare},
		PersistentMounts: persistentMounts,
		KernelModules:    kernelModules,
		MemoryMB:         *memoryMB,
		CPUs:             *cpus,
		AMD64Emulation:   false,
		Dmesg:            *dmesg,
		TimeoutSeconds:   bootTimeout.Seconds(),
	}

	if !*vnc {
		displayContext, cancelDisplay := context.WithCancel(lifetimeContext)
		monitorDone := make(chan error, 1)
		preflight := func(ctx context.Context) (startupPreflight, error) {
			return runSquadVMPreflight(ctx, api, fs.Arg(0), activeCacheDir)
		}
		start := func(ctx context.Context, options startupOptions, publish func(startupProgress)) (started displayStarted, retErr error) {
			desiredCacheDir, err := resolveSquadVMCacheDir(*cacheDir, options.SystemInstall)
			if err != nil {
				return displayStarted{}, err
			}
			if desiredCacheDir != activeCacheDir {
				publish(desktopStartupProgress("Switching install location"))
				if err := backend.stop(); err != nil {
					return displayStarted{}, err
				}
				backend, err = startEmbeddedSquadVMBackend(desiredCacheDir, *name, displayReady, openGLShareGroup)
				if err != nil {
					return displayStarted{}, err
				}
				api = backend.api
				activeCacheDir = desiredCacheDir
				selectedPreflight, err := runSquadVMPreflight(ctx, api, fs.Arg(0), activeCacheDir)
				if err != nil {
					return displayStarted{}, err
				}
				if !selectedPreflight.canStart() {
					return displayStarted{}, fmt.Errorf("selected install location did not pass startup checks")
				}
			}
			stopped := make(chan struct{})
			var stopOnce sync.Once
			stopVM := func() {
				stopOnce.Do(func() {
					shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = api.ShutdownInstanceWithIDContext(shutdownContext, *name)
					cancel()
					close(stopped)
				})
			}
			defer func() {
				if retErr != nil || ctx.Err() != nil {
					stopVM()
				}
			}()
			settings.FirstRunComplete = true
			settings.SSHEnabled = options.SSHEnabled
			if options.SystemInstall {
				settings.InstallMode = squadVMInstallSystem
			} else {
				settings.InstallMode = squadVMInstallPortable
			}
			if err := saveSquadVMSettings(settingsDir, settings); err != nil {
				return displayStarted{}, err
			}
			hostHome, err := os.UserHomeDir()
			if err != nil {
				return displayStarted{}, fmt.Errorf("resolve home directory: %w", err)
			}
			var sshPort int
			var sshPublicKey []byte
			var sshIdentity string
			startRequest := request
			if request.Network != nil {
				networkCopy := *request.Network
				networkCopy.PortForwards = append([]client.PortForward(nil), request.Network.PortForwards...)
				startRequest.Network = &networkCopy
			}
			if options.SSHEnabled {
				sshPort, err = reserveSquadVMSSHPort()
				if err != nil {
					return displayStarted{}, err
				}
				sshIdentity, sshPublicKey, err = ensureSquadVMSSHIdentity(settingsDir)
				if err != nil {
					return displayStarted{}, err
				}
				if startRequest.Network == nil {
					return displayStarted{}, fmt.Errorf("SquadVM networking must be enabled to forward SSH")
				}
				startRequest.Network.PortForwards = append(startRequest.Network.PortForwards, client.PortForward{
					Protocol:  "tcp",
					HostAddr:  "127.0.0.1",
					HostPort:  sshPort,
					GuestPort: 22,
				})
			} else if err := removeSquadVMSSHHost(hostHome); err != nil {
				return displayStarted{}, err
			}

			var measuredDownloadRate float64
			imageName, err := prepareSquadVMImage(ctx, api, fs.Arg(0), options.RefreshImage || *refreshImage, func(progress startupProgress) {
				if progress.Rate > 0 {
					measuredDownloadRate = progress.Rate
				}
				publish(progress)
			})
			if err != nil {
				return displayStarted{}, fmt.Errorf("prepare image %q: %w", fs.Arg(0), err)
			}
			if measuredDownloadRate > 0 {
				settings.DownloadRate = measuredDownloadRate
				if err := saveSquadVMSettings(settingsDir, settings); err != nil {
					return displayStarted{}, err
				}
			}
			startRequest.Image = imageName
			state, err := api.CreateInstanceStreamWithIDContext(ctx, *name, startRequest, func(event client.BootEvent) error {
				if *dmesg && event.Kind == "serial" && event.Data != "" {
					_, _ = io.WriteString(os.Stderr, event.Data)
				}
				if event.Kind != "serial" {
					publish(bootStartupProgress(event))
				}
				return nil
			})
			if err != nil {
				return displayStarted{}, fmt.Errorf("boot %q: %w", imageName, err)
			}
			if state.Display == nil {
				return displayStarted{}, fmt.Errorf("VM started without a graphical display")
			}
			go func() {
				<-ctx.Done()
				stopVM()
			}()
			if options.SSHEnabled {
				publish(desktopStartupProgress("Configuring secure host access"))
				if err := configureSquadVMGuestSSH(ctx, api, *name, sshPublicKey); err != nil {
					return displayStarted{}, err
				}
				if err := configureSquadVMSSHHost(hostHome, sshIdentity, sshPort); err != nil {
					return displayStarted{}, err
				}
			}
			var session ccdisplay.Session
			select {
			case session = <-displayReady:
			case <-ctx.Done():
				return displayStarted{}, ctx.Err()
			}
			publish(desktopStartupProgress("Waiting for the SquadVM session"))
			if err := waitForSquadVMDisplayReady(ctx, api, *name, session); err != nil {
				return displayStarted{}, err
			}
			publish(desktopStartupProgress("Waiting for a complete desktop frame"))
			go func() {
				err := monitorDisplayVM(ctx, api, *name)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					publish(failedStartupProgress(err))
				}
				monitorDone <- err
			}()
			return displayStarted{Session: session, Stopped: stopped}, nil
		}
		windowErr := openDisplayWindow(
			displayContext,
			"SquadVM",
			width,
			height,
			startupOptions{
				SSHEnabled:    settings.SSHEnabled,
				SystemInstall: systemInstall,
				DownloadRate:  settings.DownloadRate,
			},
			settings.FirstRunComplete,
			preflight,
			start,
			publishOpenGLShareGroup,
		)
		cancelDisplay()
		if windowErr != nil {
			return windowErr
		}
		select {
		case monitorErr := <-monitorDone:
			if monitorErr != nil {
				return monitorErr
			}
		default:
		}
		return nil
	}

	var lastBootMessage string
	imageName, err := prepareSquadVMImage(lifetimeContext, api, fs.Arg(0), *refreshImage, func(progress startupProgress) {
		if progress.Detail != "" {
			fmt.Fprintln(os.Stderr, progress.Detail)
		}
	})
	if err != nil {
		return fmt.Errorf("prepare image %q: %w", fs.Arg(0), err)
	}
	request.Image = imageName
	state, err := api.CreateInstanceStreamWithIDContext(lifetimeContext, *name, request, func(event client.BootEvent) error {
		if *dmesg && event.Kind == "serial" && event.Data != "" {
			_, _ = io.WriteString(os.Stderr, event.Data)
		}
		message := strings.TrimSpace(event.Message)
		if message != "" && message != lastBootMessage {
			fmt.Fprintln(os.Stderr, message)
			lastBootMessage = message
		}
		return nil
	})
	if err != nil {
		if lifetimeContext.Err() != nil {
			fmt.Fprintln(os.Stderr, "Stopping SquadVM VM...")
			return nil
		}
		return fmt.Errorf("boot %q: %w", imageName, err)
	}
	if state.Display == nil {
		return fmt.Errorf("VM started without a graphical display")
	}
	if state.Display.VNCAddress == "" {
		return fmt.Errorf("VM started without a VNC endpoint")
	}
	fmt.Printf("VNC listening on %s\n", state.Display.VNCAddress)
	fmt.Printf("VNC password: %s\n", *vncPassword)
	fmt.Printf("VM %q is running with %d CPUs and %d MiB RAM", state.ID, state.CPUs, state.MemoryMB)
	if state.NetworkIPv4 != "" {
		fmt.Printf(" at %s", state.NetworkIPv4)
	}
	fmt.Println()
	if persistentHome != "" {
		fmt.Printf("Home directory persists as %q.\n", persistentHome)
	} else {
		fmt.Println("Home directory is ephemeral.")
	}
	fmt.Printf("Storage is shared from %s to /shared.\n", storageShare.Source)
	fmt.Println("Press Ctrl-C to stop the VM.")

	statusTicker := time.NewTicker(time.Second)
	defer statusTicker.Stop()
	for {
		select {
		case <-lifetimeContext.Done():
			fmt.Fprintln(os.Stderr, "Stopping SquadVM VM...")
			return nil
		case err := <-backend.done:
			backend.finished = true
			if err == nil {
				return fmt.Errorf("embedded VM backend stopped unexpectedly")
			}
			return fmt.Errorf("embedded VM backend stopped: %w", err)
		case <-statusTicker.C:
			current, err := api.InstanceStatusOfContext(lifetimeContext, *name)
			if err != nil {
				if lifetimeContext.Err() != nil {
					fmt.Fprintln(os.Stderr, "Stopping SquadVM VM...")
					return nil
				}
				return fmt.Errorf("check VM status: %w", err)
			}
			if current.Status != "running" {
				detail := current.Error
				if detail == "" {
					detail = current.ExitReason
				}
				if detail == "" {
					detail = "no failure detail was reported"
				}
				return fmt.Errorf("VM entered %q state: %s", current.Status, detail)
			}
		}
	}
}

func monitorDisplayVM(ctx context.Context, api *client.Client, name string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			current, err := api.InstanceStatusOfContext(ctx, name)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("check VM status: %w", err)
			}
			if current.Status == "running" {
				continue
			}
			detail := current.Error
			if detail == "" {
				detail = current.ExitReason
			}
			if detail == "" {
				detail = "no failure detail was reported"
			}
			return fmt.Errorf("VM entered %q state: %s", current.Status, detail)
		}
	}
}

func prepareSquadVMImage(ctx context.Context, api *client.Client, source string, refresh bool, publish func(startupProgress)) (string, error) {
	source = strings.TrimSpace(source)
	if !isRegistryImageReference(source) {
		if publish != nil {
			publish(startupProgress{
				Phase:  startupImage,
				Title:  "Image ready",
				Detail: "Using the selected local image",
			})
		}
		return source, nil
	}

	localName := squadvmPulledImageName(source, runtime.GOARCH)
	err := api.PullImageStreamContext(ctx, localName, client.PullImageRequest{
		Source:       source,
		Architecture: runtime.GOARCH,
		Refresh:      refresh,
	}, func(event client.ProgressEvent) error {
		if publish != nil {
			publish(pullStartupProgress(event))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return localName, nil
}

func isRegistryImageReference(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return true
	}
	first, _, hasPath := strings.Cut(value, "/")
	if !hasPath {
		return false
	}
	return first == "localhost" || strings.Contains(first, ".") || strings.Contains(first, ":")
}

func squadvmPulledImageName(source, architecture string) string {
	base := source
	if slash := strings.LastIndexByte(base, '/'); slash >= 0 {
		base = base[slash+1:]
	}
	if marker := strings.IndexAny(base, ":@"); marker >= 0 {
		base = base[:marker]
	}
	var clean strings.Builder
	for _, char := range strings.ToLower(base) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			clean.WriteRune(char)
		case clean.Len() > 0 && !strings.HasSuffix(clean.String(), "-"):
			clean.WriteByte('-')
		}
	}
	name := strings.Trim(clean.String(), "-")
	if name == "" {
		name = "image"
	}
	sum := sha256.Sum256([]byte(source + "\x00" + architecture))
	return fmt.Sprintf("squadvm/%s-%s-%x", name, architecture, sum[:6])
}

func waitForSquadVMDesktop(ctx context.Context, api *client.Client, name string) error {
	const readinessScript = `
attempt=0
while [ "$attempt" -lt 900 ]; do
    if [ -S /tmp/.X11-unix/X0 ] &&
       [ -f /run/user/1000/squadvm-desktop-ready ]; then
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
exit 1
`
	exitCode := -1
	err := api.RunStreamInContext(ctx, name, client.RunRequest{
		Command:        []string{"/bin/sh", "-c", readinessScript},
		User:           "root",
		TimeoutSeconds: 95,
	}, func(event client.ExecEvent) error {
		if event.Kind == "exit" {
			exitCode = event.ExitCode
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("wait for SquadVM session: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("SquadVM session did not become ready")
	}
	return nil
}

func waitForSquadVMDisplayReady(ctx context.Context, api *client.Client, name string, session ccdisplay.Session) error {
	native, ok := session.(ccdisplay.OpenGLFrameSession)
	if !ok {
		return waitForSquadVMDesktop(ctx, api, name)
	}

	var generation uint64
	for {
		frame, available, err := native.AcquireOpenGLFrame(generation)
		if err != nil {
			return fmt.Errorf("wait for accelerated SquadVM frame: %w", err)
		}
		if available {
			generation = frame.Generation
			valid := frame.Width > 0 && frame.Height > 0 && frame.Texture != 0
			frame.Release(0)
			if valid {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Changed():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func generateVNCPassword() (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate VNC password: %w", err)
	}
	for index := range raw {
		raw[index] = alphabet[int(raw[index])%len(alphabet)]
	}
	return string(raw), nil
}

func squadvmPersistentHome(vmName, homeName string, ephemeral bool) ([]client.PersistentMount, string, error) {
	homeName = strings.TrimSpace(homeName)
	if ephemeral {
		if homeName != "" {
			return nil, "", fmt.Errorf("--home and --ephemeral-home cannot be used together")
		}
		return nil, "", nil
	}
	if homeName == "" {
		homeName = strings.TrimSpace(vmName)
	}
	return []client.PersistentMount{{Name: homeName}}, homeName, nil
}

func squadvmStorageShare(value string) (client.ShareMount, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return client.ShareMount{}, fmt.Errorf("--storage cannot be empty")
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return client.ShareMount{}, fmt.Errorf("resolve host home directory: %w", err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	source, err := filepath.Abs(value)
	if err != nil {
		return client.ShareMount{}, fmt.Errorf("resolve storage directory %q: %w", value, err)
	}
	if err := os.MkdirAll(source, 0o755); err != nil {
		return client.ShareMount{}, fmt.Errorf("create storage directory %q: %w", source, err)
	}
	info, err := os.Stat(source)
	if err != nil {
		return client.ShareMount{}, fmt.Errorf("inspect storage directory %q: %w", source, err)
	}
	if !info.IsDir() {
		return client.ShareMount{}, fmt.Errorf("storage path %q is not a directory", source)
	}
	return client.ShareMount{
		Source:   source,
		Mount:    "/shared",
		Writable: true,
		MapOwner: true,
		OwnerUID: 1000,
		OwnerGID: 1000,
		Cache:    "strict",
	}, nil
}

func parseDisplaySize(value string) (int, int, error) {
	widthText, heightText, ok := strings.Cut(strings.ToLower(strings.TrimSpace(value)), "x")
	if !ok {
		return 0, 0, fmt.Errorf("display size %q must be WIDTHxHEIGHT", value)
	}
	width, err := strconv.Atoi(widthText)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid display width %q", widthText)
	}
	height, err := strconv.Atoi(heightText)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid display height %q", heightText)
	}
	if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
		return 0, 0, fmt.Errorf("invalid display dimensions %dx%d", width, height)
	}
	return width, height, nil
}
