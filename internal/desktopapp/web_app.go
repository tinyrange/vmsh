package desktopapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"j5.nz/cc/client"
)

const desktopWebAppReadyEnvironment = "VMSH_DESKTOP_WEBAPP_READY_URL"

// Guest init may spend up to 30 seconds waiting for systemd before it runs a
// systemctl command. Leave enough time for that readiness gate and the command
// itself; a shorter exec deadline aborts an otherwise healthy VM during boot.
const desktopWebAppGuestConfigurationTimeout = 45 * time.Second

// desktopWebAppTrigger is a guest-to-host readiness callback. The random URL
// is installed in systemd's manager environment after boot, and the guest
// desktop launcher POSTs to it only after its web app is ready.
type desktopWebAppTrigger struct {
	listener      net.Listener
	server        *http.Server
	notifications chan struct{}
	serveErr      chan error
	notifyPath    string
	notifyPort    int
	webAppURL     string
}

func prepareDesktopWebApp(network *client.NetworkConfig, config DesktopWebAppConfig) (*desktopWebAppTrigger, error) {
	if network == nil {
		return nil, nil
	}
	hostPort, err := reserveLoopbackPort("web app")
	if err != nil {
		return nil, err
	}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(hostPort)
	trigger, err := newDesktopWebAppTrigger(baseURL + config.URLPath)
	if err != nil {
		return nil, err
	}
	network.PortForwards = append(network.PortForwards, client.PortForward{
		Protocol:  "tcp",
		HostAddr:  "127.0.0.1",
		HostPort:  hostPort,
		GuestPort: config.GuestPort,
	})
	network.AllowedServiceProxyPorts = appendUniquePort(network.AllowedServiceProxyPorts, trigger.notifyPort)
	return trigger, nil
}

func newDesktopWebAppTrigger(webAppURL string) (*desktopWebAppTrigger, error) {
	if err := requireLoopbackHTTPURL(webAppURL); err != nil {
		return nil, fmt.Errorf("invalid desktop web app URL: %w", err)
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate desktop web app callback token: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for desktop web app readiness: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port <= 0 {
		_ = listener.Close()
		return nil, fmt.Errorf("resolve desktop web app callback port")
	}
	trigger := &desktopWebAppTrigger{
		listener:      listener,
		notifications: make(chan struct{}, 1),
		serveErr:      make(chan error, 1),
		notifyPath:    "/ready/" + hex.EncodeToString(token),
		notifyPort:    address.Port,
		webAppURL:     webAppURL,
	}
	trigger.server = &http.Server{
		Handler:           http.HandlerFunc(trigger.handleReady),
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       2 * time.Second,
	}
	go func() {
		trigger.serveErr <- trigger.server.Serve(listener)
	}()
	return trigger, nil
}

func (t *desktopWebAppTrigger) guestNotificationURL() string {
	return "http://service.internal:" + strconv.Itoa(t.notifyPort) + t.notifyPath
}

func (t *desktopWebAppTrigger) handleReady(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path != t.notifyPath {
		http.NotFound(response, request)
		return
	}
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	select {
	case t.notifications <- struct{}{}:
	default:
	}
	response.WriteHeader(http.StatusNoContent)
}

func (t *desktopWebAppTrigger) configureGuest(ctx context.Context, api *client.Client, name string) error {
	assignment := desktopWebAppReadyEnvironment + "=" + t.guestNotificationURL()
	result, err := api.RunInContext(ctx, name, client.RunRequest{
		Command:        []string{"/usr/bin/systemctl", "set-environment", assignment},
		User:           "root",
		TimeoutSeconds: desktopWebAppGuestConfigurationTimeout.Seconds(),
	})
	if err != nil {
		return fmt.Errorf("configure desktop web app callback: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("configure desktop web app callback: systemctl exited %d: %s", result.ExitCode, strings.TrimSpace(result.Output))
	}
	return nil
}

func (t *desktopWebAppTrigger) monitor(ctx context.Context, open func(string) error) error {
	defer t.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-t.serveErr:
			if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("serve desktop web app callback: %w", err)
		case <-t.notifications:
			if err := open(t.webAppURL); err != nil {
				return err
			}
		}
	}
}

func (t *desktopWebAppTrigger) Close() {
	_ = t.server.Close()
	_ = t.listener.Close()
}

func appendUniquePort(ports []int, port int) []int {
	for _, existing := range ports {
		if existing == port {
			return ports
		}
	}
	return append(ports, port)
}

func openLoopbackWebApp(value string) error {
	if err := requireLoopbackHTTPURL(value); err != nil {
		return err
	}
	if err := openExternalURL(value); err != nil {
		return fmt.Errorf("open %s web app: %w", productName(), err)
	}
	return nil
}

func requireLoopbackHTTPURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("web app URL must use HTTP on a loopback address")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("web app URL must use HTTP on a loopback address")
	}
	return nil
}
