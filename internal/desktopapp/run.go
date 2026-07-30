package desktopapp

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
	"syscall"
	"time"

	"j5.nz/cc/client"
	ccdisplay "j5.nz/cc/display"
)

// Run starts a configured desktop application.
func Run(config Config, args []string) (retErr error) {
	var err error
	appConfig, err = normalizeConfig(config)
	if err != nil {
		return err
	}
	applyTheme(appConfig.Theme)
	if len(args) == 0 {
		args = []string{appConfig.DefaultImage}
	}
	fs := flag.NewFlagSet(productName(), flag.ContinueOnError)
	name := fs.String("name", appConfig.DefaultVMName, "VM name")
	home := fs.String("home", "", "Persistent home identity (defaults to the VM name)")
	ephemeralHome := fs.Bool("ephemeral-home", appConfig.DefaultEphemeralHome, "Discard home-directory changes when the VM stops")
	storage := fs.String("storage", appConfig.DefaultStorage, "Host directory shared at "+appConfig.GuestStorageMount)
	user := fs.String("user", appConfig.DefaultUser, "Default command user")
	cacheDir := fs.String("cache-dir", "", "Image and runtime cache directory")
	refreshImage := fs.Bool("refresh-image", false, "Refresh and rebuild the cached image")
	vnc := fs.Bool("vnc", false, "Use a VNC client instead of the native graphics window")
	vncListen := fs.String("vnc-listen", "127.0.0.1:0", "VNC listen address (requires --vnc)")
	vncPassword := fs.String("vnc-password", "", "VNC password (generated when omitted; requires --vnc)")
	displaySize := fs.String("display", "1440x900", "Initial display size WIDTHxHEIGHT")
	initSystem := fs.String("init", "systemd", "Guest init system")
	memoryMB := fs.Uint64("memory-mb", appConfig.DefaultMemoryMB, "Guest memory in MiB")
	cpus := fs.Int("cpus", appConfig.DefaultCPUs, "Guest CPU count")
	network := fs.Bool("network", true, "Enable isolated networking with outbound internet access")
	bootTimeout := fs.Duration("boot-timeout", 10*time.Minute, "VM preparation and boot timeout")
	dmesg := fs.Bool("dmesg", false, "Forward the guest kernel log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := platformCompatibilityError(); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: %s [OPTIONS] IMAGE", productName())
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
	var storageOptionSet bool
	var memoryOptionSet bool
	var cpuOptionSet bool
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "vnc-listen" || item.Name == "vnc-password" {
			vncOptionSet = true
		}
		if item.Name == "storage" {
			storageOptionSet = true
		}
		if item.Name == "memory-mb" {
			memoryOptionSet = true
		}
		if item.Name == "cpus" {
			cpuOptionSet = true
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
	persistentMounts, persistentHome, err := persistentHomeMount(*name, *home, *ephemeralHome)
	if err != nil {
		return err
	}
	mapPersistentHomeOwner(persistentMounts, appConfig.PersistentHomeOwner)
	width, height, err := parseDisplaySize(*displaySize)
	if err != nil {
		return err
	}
	var (
		settings       appSettings
		settingsDir    string
		systemInstall  bool
		activeCacheDir string
	)
	if strings.TrimSpace(*cacheDir) != "" {
		activeCacheDir, err = resolveAppCacheDir(*cacheDir, false)
		if err != nil {
			return err
		}
		settingsDir = filepath.Join(activeCacheDir, "frontend")
		settings, err = loadAppSettingsFromDir(settingsDir)
		if err != nil {
			return err
		}
	} else {
		settings, settingsDir, err = loadAppSettings()
		if err != nil {
			return err
		}
		systemInstall = settings.systemInstall()
		activeCacheDir, err = resolveAppCacheDir("", systemInstall)
		if err != nil {
			return err
		}
	}
	storageValue := *storage
	if !storageOptionSet && strings.TrimSpace(settings.SharedFolder) != "" {
		storageValue = settings.SharedFolder
	}
	storageShare, err := createStorageShare(storageValue)
	if err != nil {
		return err
	}
	settings.SharedFolder = storageShare.Source
	selectedMemoryMB := *memoryMB
	if !memoryOptionSet && settings.MemoryMB != 0 {
		selectedMemoryMB = settings.MemoryMB
	}
	selectedCPUs := *cpus
	if !cpuOptionSet && settings.CPUs > 0 {
		selectedCPUs = settings.CPUs
	}
	maximumMemoryMB := hostMemoryMB()
	if maximumMemoryMB == 0 {
		maximumMemoryMB = selectedMemoryMB
	}
	resourceOptions := normalizeResourceOptions(startupOptions{
		MemoryMB:    selectedMemoryMB,
		CPUs:        selectedCPUs,
		MaxMemoryMB: maximumMemoryMB,
		MaxCPUs:     runtime.NumCPU(),
	})

	displayReady := make(chan ccdisplay.Session, 1)
	if strings.TrimSpace(*cacheDir) == "" && !systemInstall {
		regularCacheDir, regularErr := resolveAppCacheDir("", true)
		if regularErr == nil && filepath.Clean(activeCacheDir) == filepath.Clean(regularCacheDir) {
			// A populated cache from an existing installation takes precedence
			// over the new portable default. Keep the checkbox honest about the
			// location that is actually in use.
			systemInstall = true
		}
	}
	backend, err := startEmbeddedBackend(activeCacheDir, *name, displayReady)
	if err != nil && strings.TrimSpace(*cacheDir) == "" && !systemInstall {
		// A portable directory may be unavailable beside an installed app.
		// Keep the first-run UI reachable by falling back to the user cache and
		// presenting the system-install checkbox as selected.
		systemInstall = true
		activeCacheDir, err = resolveAppCacheDir("", true)
		if err == nil {
			backend, err = startEmbeddedBackend(activeCacheDir, *name, displayReady)
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
	kernelModules := appKernelModules(runtimeArchitecture())
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
		AMD64Emulation:   appConfig.AMD64Emulation,
		Dmesg:            *dmesg,
		TimeoutSeconds:   bootTimeout.Seconds(),
	}
	if appConfig.CVMFSHostMount != nil {
		request.Env = append(request.Env, "CVMFS_DISABLE=true", "NEURODESKTOP_CVMFS_STARTUP_MODE=external")
	}

	if !*vnc {
		displayContext, cancelDisplay := context.WithCancel(lifetimeContext)
		monitorDone := make(chan error, 1)
		var preflightMu sync.Mutex
		var latestPreflight startupPreflight
		var imageStage backgroundImageStage
		preflight := func(ctx context.Context) (startupPreflight, error) {
			result, err := runConfiguredAppPreflight(ctx, api, fs.Arg(0), activeCacheDir, appConfig.CVMFSHostMount)
			if err == nil {
				preflightMu.Lock()
				latestPreflight = result
				preflightMu.Unlock()
			}
			return result, err
		}
		start := func(ctx context.Context, options startupOptions, publish func(startupProgress)) (started displayStarted, retErr error) {
			options = normalizeResourceOptions(options)
			desiredCacheDir, err := resolveAppCacheDir(*cacheDir, options.SystemInstall)
			if err != nil {
				return displayStarted{}, err
			}
			if desiredCacheDir != activeCacheDir {
				publish(desktopStartupProgress("Switching install location"))
				if err := backend.stop(); err != nil {
					return displayStarted{}, err
				}
				backend, err = startEmbeddedBackend(desiredCacheDir, *name, displayReady)
				if err != nil {
					return displayStarted{}, err
				}
				api = backend.api
				activeCacheDir = desiredCacheDir
				selectedPreflight, err := runConfiguredAppPreflight(ctx, api, fs.Arg(0), activeCacheDir, appConfig.CVMFSHostMount)
				if err != nil {
					return displayStarted{}, err
				}
				if !selectedPreflight.canPrepareImage() {
					return displayStarted{}, fmt.Errorf("selected install location did not pass startup checks")
				}
				options.CVMFSAutoMirror = selectedPreflight.CVMFSMirror
				preflightMu.Lock()
				latestPreflight = selectedPreflight
				preflightMu.Unlock()
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
			selectedShare, err := createStorageShare(options.SharedFolder)
			if err != nil {
				return displayStarted{}, err
			}
			settings.FirstRunComplete = true
			settings.SSHEnabled = options.SSHEnabled
			settings.SharedFolder = selectedShare.Source
			settings.MemoryMB = options.MemoryMB
			settings.CPUs = options.CPUs
			settings.CVMFSMirror = options.CVMFSMirror
			if options.SystemInstall {
				settings.InstallMode = appInstallSystem
			} else {
				settings.InstallMode = appInstallPortable
			}
			if err := saveAppSettings(settingsDir, settings); err != nil {
				return displayStarted{}, err
			}
			hostHome, err := os.UserHomeDir()
			if err != nil {
				return displayStarted{}, fmt.Errorf("resolve home directory: %w", err)
			}
			var sshPort int
			var sshPublicKey []byte
			var sshIdentity string
			startRequest := applyResourceOptions(request, options)
			startRequest.Shares = []client.ShareMount{selectedShare}
			// The native startup view owns the window until the graphical session
			// is ready, so keep the VM serial stream enabled and show it there.
			startRequest.Dmesg = true
			if options.DisplayWidth > 0 && options.DisplayHeight > 0 {
				displayCopy := *request.Display
				displayCopy.Width = uint32(options.DisplayWidth)
				displayCopy.Height = uint32(options.DisplayHeight)
				startRequest.Display = &displayCopy
			}
			if request.Network != nil {
				networkCopy := *request.Network
				networkCopy.PortForwards = append([]client.PortForward(nil), request.Network.PortForwards...)
				startRequest.Network = &networkCopy
			}
			var webApp *desktopWebAppTrigger
			if appConfig.DesktopWebApp != nil && startRequest.Network != nil {
				webApp, err = prepareDesktopWebApp(startRequest.Network, *appConfig.DesktopWebApp)
				if err != nil {
					return displayStarted{}, err
				}
				defer func() {
					if retErr != nil || ctx.Err() != nil {
						webApp.Close()
					}
				}()
			}
			if options.SSHEnabled {
				sshPort, err = reserveSSHPort()
				if err != nil {
					return displayStarted{}, err
				}
				sshIdentity, sshPublicKey, err = ensureSSHIdentity(settingsDir)
				if err != nil {
					return displayStarted{}, err
				}
				if startRequest.Network == nil {
					return displayStarted{}, fmt.Errorf("%s networking must be enabled to forward SSH", productName())
				}
				startRequest.Network.PortForwards = append(startRequest.Network.PortForwards, client.PortForward{
					Protocol:  "tcp",
					HostAddr:  "127.0.0.1",
					HostPort:  sshPort,
					GuestPort: 22,
				})
			} else if err := removeSSHHost(hostHome); err != nil {
				return displayStarted{}, err
			}

			var measuredDownloadRate float64
			imageName := ""
			if options.RefreshImage && appConfig.ExperimentalBackgroundImageUpdates {
				publish(desktopStartupProgress("Finishing the image update"))
				stagedName, staged, stageErr := imageStage.take(ctx)
				if staged && stageErr == nil {
					imageName = pulledImageName(fs.Arg(0), runtime.GOARCH)
					if err := api.ActivateStagedImageContext(ctx, imageName, stagedName); err != nil {
						imageName = ""
					}
				}
			}
			if imageName == "" {
				imageName, err = prepareAppImage(ctx, api, fs.Arg(0), options.RefreshImage || *refreshImage, func(progress startupProgress) {
					if progress.Rate > 0 {
						measuredDownloadRate = progress.Rate
					}
					publish(progress)
				})
			}
			if err != nil {
				return displayStarted{}, fmt.Errorf("prepare image %q: %w", fs.Arg(0), err)
			}
			if measuredDownloadRate > 0 {
				settings.DownloadRate = measuredDownloadRate
				if err := saveAppSettings(settingsDir, settings); err != nil {
					return displayStarted{}, err
				}
			}
			startRequest.Image = imageName
			if config := appConfig.CVMFSHostMount; config != nil {
				selectedMirror := strings.TrimSpace(options.CVMFSMirror)
				if selectedMirror == "" {
					selectedMirror = strings.TrimSpace(options.CVMFSAutoMirror)
				}
				if selectedMirror == "" {
					publish(startupProgress{Phase: startupBoot, Title: "Connecting CVMFS", Detail: "Detecting the nearest mirror"})
					probe, probeErr := probeConfiguredCVMFS(ctx, api, config)
					if probeErr != nil {
						return displayStarted{}, fmt.Errorf("connect CVMFS before VM start: %w", probeErr)
					}
					selectedMirror = strings.TrimSpace(probe.SelectedMirror)
				}
				if selectedMirror == "" {
					return displayStarted{}, fmt.Errorf("connect CVMFS before VM start: no mirror is available")
				}
				publish(startupProgress{Phase: startupBoot, Title: "Connecting CVMFS", Detail: mirrorDisplayName(selectedMirror)})
				if err := api.SelectCVMFSMirrorContext(ctx, client.CVMFSMirrorSelectionRequest{Repo: config.Repo, Mirror: selectedMirror}); err != nil {
					return displayStarted{}, fmt.Errorf("connect CVMFS before VM start: %w", err)
				}
			}
			state, err := api.CreateInstanceStreamWithIDContext(ctx, *name, startRequest, func(event client.BootEvent) error {
				if *dmesg && event.Kind == "serial" && event.Data != "" {
					_, _ = io.WriteString(os.Stderr, event.Data)
				}
				if event.Kind == "serial" {
					publish(startupProgress{Serial: event.Data})
				} else {
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
			if webApp != nil {
				if err := webApp.configureGuest(ctx, api, *name); err != nil {
					return displayStarted{}, err
				}
				go func() {
					if err := webApp.monitor(ctx, openLoopbackWebApp); err != nil && ctx.Err() == nil {
						fmt.Fprintf(os.Stderr, "%s: %v\n", productName(), err)
					}
				}()
			}
			go func() {
				<-ctx.Done()
				stopVM()
			}()
			if options.SSHEnabled {
				publish(desktopStartupProgress("Configuring secure host access"))
				if err := configureGuestSSH(ctx, api, *name, sshPublicKey); err != nil {
					return displayStarted{}, err
				}
				if err := configureSSHHost(hostHome, sshIdentity, sshPort); err != nil {
					return displayStarted{}, err
				}
			}
			var session ccdisplay.Session
			select {
			case session = <-displayReady:
			case <-ctx.Done():
				return displayStarted{}, ctx.Err()
			}
			publish(desktopStartupProgress("Waiting for the " + productName() + " session"))
			if err := waitForDesktop(ctx, api, *name); err != nil {
				return displayStarted{}, err
			}
			publish(desktopStartupProgress("Waiting for a complete desktop frame"))
			if appConfig.ExperimentalBackgroundImageUpdates && isRegistryImageReference(fs.Arg(0)) {
				preflightMu.Lock()
				hasUpdate := latestPreflight.hasUpdate()
				preflightMu.Unlock()
				if hasUpdate {
					stageAPI := api
					stageName := pulledImageName(fs.Arg(0), runtime.GOARCH) + "-staged"
					imageStage.start(stageName, func() error {
						return stageAPI.PullImageStreamContext(lifetimeContext, stageName, client.PullImageRequest{
							Source:         fs.Arg(0),
							Architecture:   runtime.GOARCH,
							Refresh:        true,
							KeepCompressed: appConfig.ExperimentalCompressedOCI,
						}, nil)
					})
				}
			}
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
		var cvmfsStatus cvmfsStatusSource
		if appConfig.CVMFSHostMount != nil {
			cvmfsStatus = func(ctx context.Context) (client.CVMFSStatusResponse, error) {
				return api.CVMFSStatusContext(ctx)
			}
		}
		windowErr := openDisplayWindow(
			displayContext,
			productName(),
			width,
			height,
			startupOptions{
				SSHEnabled:    settings.SSHEnabled,
				SystemInstall: systemInstall,
				DownloadRate:  settings.DownloadRate,
				SharedFolder:  settings.SharedFolder,
				MemoryMB:      resourceOptions.MemoryMB,
				CPUs:          resourceOptions.CPUs,
				MaxMemoryMB:   resourceOptions.MaxMemoryMB,
				MaxCPUs:       resourceOptions.MaxCPUs,
				CVMFSRepo:     cvmfsConfigRepo(appConfig.CVMFSHostMount),
				CVMFSMirrors:  cvmfsConfigMirrors(appConfig.CVMFSHostMount),
				CVMFSMirror:   configuredCVMFSMirror(settings.CVMFSMirror, appConfig.CVMFSHostMount),
			},
			preflight,
			start,
			cvmfsStatus,
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

	if config := appConfig.CVMFSHostMount; config != nil {
		probe, err := probeConfiguredCVMFS(lifetimeContext, api, config)
		if err != nil {
			return fmt.Errorf("check CVMFS mirrors: %w", err)
		}
		selected := configuredCVMFSMirror(settings.CVMFSMirror, config)
		if selected == "" {
			selected = probe.SelectedMirror
		}
		if err := api.SelectCVMFSMirrorContext(lifetimeContext, client.CVMFSMirrorSelectionRequest{Repo: config.Repo, Mirror: selected}); err != nil {
			return fmt.Errorf("select CVMFS mirror: %w", err)
		}
	}

	var lastBootMessage string
	imageName, err := prepareAppImage(lifetimeContext, api, fs.Arg(0), *refreshImage, func(progress startupProgress) {
		if progress.Detail != "" {
			fmt.Fprintln(os.Stderr, progress.Detail)
		}
	})
	if err != nil {
		return fmt.Errorf("prepare image %q: %w", fs.Arg(0), err)
	}
	request.Image = imageName
	var webApp *desktopWebAppTrigger
	if appConfig.DesktopWebApp != nil && request.Network != nil {
		webApp, err = prepareDesktopWebApp(request.Network, *appConfig.DesktopWebApp)
		if err != nil {
			return err
		}
		defer webApp.Close()
	}
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
			fmt.Fprintf(os.Stderr, "Stopping %s VM...\n", productName())
			return nil
		}
		return fmt.Errorf("boot %q: %w", imageName, err)
	}
	if state.Display == nil {
		return fmt.Errorf("VM started without a graphical display")
	}
	if webApp != nil {
		if err := webApp.configureGuest(lifetimeContext, api, *name); err != nil {
			return err
		}
		go func() {
			if err := webApp.monitor(lifetimeContext, openLoopbackWebApp); err != nil && lifetimeContext.Err() == nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", productName(), err)
			}
		}()
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
	fmt.Printf("Storage is shared from %s to %s.\n", storageShare.Source, appConfig.GuestStorageMount)
	fmt.Println("Press Ctrl-C to stop the VM.")

	statusTicker := time.NewTicker(time.Second)
	defer statusTicker.Stop()
	for {
		select {
		case <-lifetimeContext.Done():
			fmt.Fprintf(os.Stderr, "Stopping %s VM...\n", productName())
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
					fmt.Fprintf(os.Stderr, "Stopping %s VM...\n", productName())
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

func prepareAppImage(ctx context.Context, api *client.Client, source string, refresh bool, publish func(startupProgress)) (string, error) {
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

	localName := pulledImageName(source, runtime.GOARCH)
	err := api.PullImageStreamContext(ctx, localName, client.PullImageRequest{
		Source:         source,
		Architecture:   runtime.GOARCH,
		Refresh:        refresh,
		KeepCompressed: appConfig.ExperimentalCompressedOCI,
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

func pulledImageName(source, architecture string) string {
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
	return fmt.Sprintf("%s/%s-%s-%x", appConfig.ImageNamespace, name, architecture, sum[:6])
}

func waitForDesktop(ctx context.Context, api *client.Client, name string) error {
	readinessScript := appConfig.DesktopReadiness
	if strings.TrimSpace(readinessScript) == "" {
		readinessScript = `
attempt=0
while [ "$attempt" -lt 900 ]; do
    if [ -S /tmp/.X11-unix/X0 ]; then
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
exit 1
`
	}
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
		return fmt.Errorf("wait for %s session: %w", productName(), err)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s session did not become ready", productName())
	}
	return nil
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

func persistentHomeMount(vmName, homeName string, ephemeral bool) ([]client.PersistentMount, string, error) {
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

func mapPersistentHomeOwner(mounts []client.PersistentMount, owner *GuestOwner) {
	if len(mounts) == 0 || owner == nil {
		return
	}
	mounts[0].MapOwner = true
	mounts[0].OwnerUID = owner.UID
	mounts[0].OwnerGID = owner.GID
}

func runConfiguredAppPreflight(ctx context.Context, api *client.Client, source, cacheDir string, cvmfs *CVMFSHostMountConfig) (startupPreflight, error) {
	result, err := runAppPreflight(ctx, api, source, cacheDir)
	if err != nil || cvmfs == nil {
		return result, err
	}
	result.CVMFSRequired = true
	probe, probeErr := probeConfiguredCVMFS(ctx, api, cvmfs)
	if probeErr != nil {
		result.CVMFSDetail = probeErr.Error()
		return result, nil
	}
	result.CVMFSMirror = probe.SelectedMirror
	result.CVMFSOK = strings.TrimSpace(probe.SelectedMirror) != ""
	measured := 0
	var selectedRate float64
	for _, candidate := range probe.Results {
		if candidate.RootCatalogBytes > 0 {
			measured++
		}
		if candidate.Mirror == probe.SelectedMirror {
			selectedRate = candidate.RootCatalogBytesPerSec
		}
	}
	result.CVMFSDetail = fmt.Sprintf("Measured %d root-catalog mirror", measured)
	if measured != 1 {
		result.CVMFSDetail += "s"
	}
	if selectedRate > 0 {
		result.CVMFSDetail += " · " + formatBytes(int64(selectedRate)) + "/s"
	}
	return result, nil
}

func probeConfiguredCVMFS(ctx context.Context, api *client.Client, cvmfs *CVMFSHostMountConfig) (client.CVMFSMirrorProbeResponse, error) {
	if cvmfs == nil {
		return client.CVMFSMirrorProbeResponse{}, fmt.Errorf("CVMFS host mount is not configured")
	}
	probeContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return api.ProbeCVMFSMirrorsContext(probeContext, client.CVMFSMirrorProbeRequest{Repo: cvmfs.Repo})
}

func cvmfsConfigRepo(config *CVMFSHostMountConfig) string {
	if config == nil {
		return ""
	}
	return config.Repo
}

func cvmfsConfigMirrors(config *CVMFSHostMountConfig) []string {
	if config == nil {
		return nil
	}
	seen := make(map[string]bool, len(config.Mirrors)+1)
	out := make([]string, 0, len(config.Mirrors)+1)
	for _, candidate := range append([]string{config.Mirror}, config.Mirrors...) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

func configuredCVMFSMirror(saved string, config *CVMFSHostMountConfig) string {
	saved = strings.TrimSpace(saved)
	if saved == "" || config == nil {
		return ""
	}
	for _, mirror := range cvmfsConfigMirrors(config) {
		if strings.TrimSpace(mirror) == saved {
			return saved
		}
	}
	return ""
}

func createStorageShare(value string) (client.ShareMount, error) {
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
		Mount:    appConfig.GuestStorageMount,
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
