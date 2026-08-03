package desktopapp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
	"j5.nz/cc/display"
)

type embeddedBackend struct {
	api      *client.Client
	done     chan error
	finished bool
}

func startEmbeddedBackend(cacheDir, name string, displayReady chan<- display.Session) (*embeddedBackend, error) {
	ready := make(chan client.ServerHello, 1)
	done := make(chan error, 1)
	serverArgs := []string{"-addr", "127.0.0.1:0", "-cache-dir", cacheDir}
	go func() {
		_, err := ccvmd.RunServer(serverArgs, ccvmd.ServerOptions{
			Kind:          appConfig.Kind,
			StartupWriter: io.Discard,
			OnStartup: func(hello client.ServerHello) error {
				ready <- hello
				return nil
			},
			OnDisplay: func(id string, session display.Session) {
				if id != name {
					return
				}
				select {
				case displayReady <- session:
				default:
				}
			},
		})
		done <- err
	}()

	var hello client.ServerHello
	select {
	case hello = <-ready:
	case err := <-done:
		if err == nil {
			return nil, fmt.Errorf("embedded VM backend stopped before startup")
		}
		return nil, fmt.Errorf("start embedded VM backend: %w", err)
	}
	if strings.TrimSpace(hello.Addr) == "" {
		return nil, fmt.Errorf("embedded VM backend did not publish an address")
	}
	scheme := hello.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return &embeddedBackend{
		api:  client.NewClient(scheme+"://"+hello.Addr, nil),
		done: done,
	}, nil
}

func (b *embeddedBackend) stop() error {
	if b == nil || b.finished {
		return nil
	}
	select {
	case err := <-b.done:
		b.finished = true
		return err
	default:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	shutdownErr := b.api.ShutdownContext(ctx)
	cancel()
	if shutdownErr != nil {
		return fmt.Errorf("stop embedded VM backend: %w", shutdownErr)
	}
	select {
	case err := <-b.done:
		b.finished = true
		return err
	case <-time.After(15 * time.Second):
		return fmt.Errorf("embedded VM backend did not stop")
	}
}
