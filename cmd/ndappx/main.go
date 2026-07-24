package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
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
	user := fs.String("user", "jovyan", "Default desktop and command user")
	cacheDir := fs.String("cache-dir", "", "Image and runtime cache directory")
	vncListen := fs.String("vnc-listen", "127.0.0.1:0", "VNC listen address")
	display := fs.String("display", "1440x900", "Initial display size WIDTHxHEIGHT")
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
	persistentMounts, persistentHome, err := ndappxPersistentHome(*name, *home, *ephemeralHome)
	if err != nil {
		return err
	}
	width, height, err := parseDisplaySize(*display)
	if err != nil {
		return err
	}

	ready := make(chan client.ServerHello, 1)
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
	lifetimeContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	var lastBootMessage string
	state, err := api.CreateInstanceStreamWithIDContext(lifetimeContext, *name, client.CreateInstanceRequest{
		Image:       fs.Arg(0),
		DefaultUser: *user,
		InitSystem:  *initSystem,
		Network:     networkConfig,
		Display: &client.DisplayConfig{
			Width:     uint32(width),
			Height:    uint32(height),
			VNCListen: *vncListen,
		},
		PersistentMounts: persistentMounts,
		MemoryMB:         *memoryMB,
		CPUs:             *cpus,
		Dmesg:            *dmesg,
		TimeoutSeconds:   bootTimeout.Seconds(),
	}, func(event client.BootEvent) error {
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
		return fmt.Errorf("boot %q: %w", fs.Arg(0), err)
	}
	if state.Display == nil || state.Display.VNCAddress == "" {
		return fmt.Errorf("VM started without a VNC endpoint")
	}

	fmt.Printf("VNC listening on %s\n", state.Display.VNCAddress)
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
