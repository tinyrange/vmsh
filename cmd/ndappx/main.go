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
	"syscall"
	"time"

	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
	ccdisplay "j5.nz/cc/display"
)

const defaultNeurodesktopImage = "ghcr.io/tinyrange/neurodesktop-glass:20260727"

func main() {
	if err := run(platformArguments(os.Args[1:])); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "ndappx:", err)
		os.Exit(1)
	}
}

func run(args []string) (retErr error) {
	fs := flag.NewFlagSet("ndappx", flag.ContinueOnError)
	name := fs.String("name", "ndappx", "VM name")
	home := fs.String("home", "", "Persistent home identity (defaults to the VM name)")
	ephemeralHome := fs.Bool("ephemeral-home", false, "Discard home-directory changes when the VM stops")
	storage := fs.String("storage", "~/neurodesktop-storage", "Host directory shared at /neurodesktop-storage")
	user := fs.String("user", "jovyan", "Default desktop and command user")
	cacheDir := fs.String("cache-dir", "", "Image and runtime cache directory")
	vnc := fs.Bool("vnc", false, "Use a VNC client instead of the native graphics window")
	vncListen := fs.String("vnc-listen", "127.0.0.1:0", "VNC listen address (requires --vnc)")
	vncPassword := fs.String("vnc-password", "", "VNC password (generated when omitted; requires --vnc)")
	displaySize := fs.String("display", "1440x900", "Initial display size WIDTHxHEIGHT")
	initSystem := fs.String("init", "systemd", "Guest init system")
	memoryMB := fs.Uint64("memory-mb", 8192, "Guest memory in MiB")
	cpus := fs.Int("cpus", 4, "Guest CPU count")
	network := fs.Bool("network", true, "Enable isolated networking with outbound internet access")
	bootTimeout := fs.Duration("boot-timeout", 10*time.Minute, "VM preparation and boot timeout")
	dmesg := fs.Bool("dmesg", false, "Forward the guest kernel log")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: ndappx [OPTIONS] IMAGE")
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
	persistentMounts, persistentHome, err := ndappxPersistentHome(*name, *home, *ephemeralHome)
	if err != nil {
		return err
	}
	storageShare, err := ndappxStorageShare(*storage)
	if err != nil {
		return err
	}
	width, height, err := parseDisplaySize(*displaySize)
	if err != nil {
		return err
	}

	ready := make(chan client.ServerHello, 1)
	displayReady := make(chan ccdisplay.Session, 1)
	serverDone := make(chan error, 1)
	serverArgs := []string{"-addr", "127.0.0.1:0"}
	if strings.TrimSpace(*cacheDir) != "" {
		serverArgs = append(serverArgs, "-cache-dir", *cacheDir)
	}
	go func() {
		_, err := ccvmd.RunServer(serverArgs, ccvmd.ServerOptions{
			Kind:          "ndappx",
			StartupWriter: io.Discard,
			OnStartup: func(hello client.ServerHello) error {
				ready <- hello
				return nil
			},
			OnDisplay: func(id string, session ccdisplay.Session) {
				if id != *name {
					return
				}
				select {
				case displayReady <- session:
				default:
				}
			},
		})
		serverDone <- err
	}()

	var hello client.ServerHello
	select {
	case hello = <-ready:
	case err := <-serverDone:
		if err == nil {
			return fmt.Errorf("embedded VM backend stopped before startup")
		}
		return fmt.Errorf("start embedded VM backend: %w", err)
	}
	if hello.Addr == "" {
		return fmt.Errorf("embedded VM backend did not publish an address")
	}
	scheme := hello.Scheme
	if scheme == "" {
		scheme = "http"
	}
	api := client.NewClient(scheme+"://"+hello.Addr, nil)
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
	serverFinished := false
	defer func() {
		if serverFinished {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		shutdownErr := api.ShutdownContext(ctx)
		cancel()
		if shutdownErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("stop embedded VM backend: %w", shutdownErr))
			return
		}
		select {
		case err := <-serverDone:
			serverFinished = true
			if err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("embedded VM backend shutdown: %w", err))
			}
		case <-time.After(15 * time.Second):
			retErr = errors.Join(retErr, fmt.Errorf("embedded VM backend did not stop"))
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
		MemoryMB:         *memoryMB,
		CPUs:             *cpus,
		AMD64Emulation:   true,
		Dmesg:            *dmesg,
		TimeoutSeconds:   bootTimeout.Seconds(),
	}

	if !*vnc {
		displayContext, cancelDisplay := context.WithCancel(lifetimeContext)
		monitorDone := make(chan error, 1)
		start := func(ctx context.Context, publish func(startupProgress)) (ccdisplay.Session, error) {
			imageName, err := prepareNDAppXImage(ctx, api, fs.Arg(0), publish)
			if err != nil {
				return nil, fmt.Errorf("prepare image %q: %w", fs.Arg(0), err)
			}
			request.Image = imageName
			state, err := api.CreateInstanceStreamWithIDContext(ctx, *name, request, func(event client.BootEvent) error {
				if *dmesg && event.Kind == "serial" && event.Data != "" {
					_, _ = io.WriteString(os.Stderr, event.Data)
				}
				if event.Kind != "serial" {
					publish(bootStartupProgress(event))
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("boot %q: %w", imageName, err)
			}
			if state.Display == nil {
				return nil, fmt.Errorf("VM started without a graphical display")
			}
			var session ccdisplay.Session
			select {
			case session = <-displayReady:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			publish(desktopStartupProgress("Waiting for the Neurodesktop session"))
			if err := waitForNeurodesktopDesktop(ctx, api, *name); err != nil {
				return nil, err
			}
			publish(desktopStartupProgress("Waiting for a complete desktop frame"))
			go func() {
				err := monitorDisplayVM(displayContext, api, *name)
				if err != nil {
					publish(failedStartupProgress(err))
				}
				monitorDone <- err
			}()
			return session, nil
		}
		windowErr := openDisplayWindow(displayContext, "Neurodesktop", width, height, start)
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
	imageName, err := prepareNDAppXImage(lifetimeContext, api, fs.Arg(0), func(progress startupProgress) {
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
			fmt.Fprintln(os.Stderr, "Stopping ndappx VM...")
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
	fmt.Printf("Storage is shared from %s to /neurodesktop-storage.\n", storageShare.Source)
	fmt.Println("Press Ctrl-C to stop the VM.")

	statusTicker := time.NewTicker(time.Second)
	defer statusTicker.Stop()
	for {
		select {
		case <-lifetimeContext.Done():
			fmt.Fprintln(os.Stderr, "Stopping ndappx VM...")
			return nil
		case err := <-serverDone:
			serverFinished = true
			if err == nil {
				return fmt.Errorf("embedded VM backend stopped unexpectedly")
			}
			return fmt.Errorf("embedded VM backend stopped: %w", err)
		case <-statusTicker.C:
			current, err := api.InstanceStatusOfContext(lifetimeContext, *name)
			if err != nil {
				if lifetimeContext.Err() != nil {
					fmt.Fprintln(os.Stderr, "Stopping ndappx VM...")
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

func prepareNDAppXImage(ctx context.Context, api *client.Client, source string, publish func(startupProgress)) (string, error) {
	source = strings.TrimSpace(source)
	if !isRegistryImageReference(source) {
		if publish != nil {
			publish(startupProgress{
				Phase:  startupPrepare,
				Title:  "Image ready",
				Detail: "Using the selected local image",
			})
		}
		return source, nil
	}

	localName := ndappxPulledImageName(source, runtime.GOARCH)
	err := api.PullImageStreamContext(ctx, localName, client.PullImageRequest{
		Source:       source,
		Architecture: runtime.GOARCH,
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

func ndappxPulledImageName(source, architecture string) string {
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
	return fmt.Sprintf("ndappx/%s-%s-%x", name, architecture, sum[:6])
}

func waitForNeurodesktopDesktop(ctx context.Context, api *client.Client, name string) error {
	const readinessScript = `
attempt=0
while [ "$attempt" -lt 900 ]; do
    if [ -S /tmp/.X11-unix/X0 ]; then
        for comm in /proc/[0-9]*/comm; do
            [ -r "$comm" ] || continue
            IFS= read -r process < "$comm" || continue
            if [ "$process" = lxsession ]; then
                exit 0
            fi
        done
    fi
    attempt=$((attempt + 1))
    sleep 0.1
done
exit 1
`
	response, err := api.RunInContext(ctx, name, client.RunRequest{
		Command:        []string{"/bin/sh", "-c", readinessScript},
		User:           "root",
		TimeoutSeconds: 95,
	})
	if err != nil {
		return fmt.Errorf("wait for Neurodesktop session: %w", err)
	}
	if response.ExitCode != 0 {
		return fmt.Errorf("Neurodesktop session did not become ready")
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

func ndappxPersistentHome(vmName, homeName string, ephemeral bool) ([]client.PersistentMount, string, error) {
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

func ndappxStorageShare(value string) (client.ShareMount, error) {
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
		Mount:    "/vmsh-neurodesktop-storage",
		Writable: true,
		MapOwner: true,
		OwnerUID: 1000,
		OwnerGID: 100,
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
