package desktopapp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func monitorDesktopWebApp(ctx context.Context, hostPort int, config DesktopWebAppConfig) error {
	baseURL := "http://127.0.0.1:" + strconv.Itoa(hostPort)
	return waitForDesktopWebApp(ctx, &http.Client{Timeout: 2 * time.Second}, baseURL+config.StatusPath, baseURL+config.URLPath, time.Second, openLoopbackWebApp)
}

func waitForDesktopWebApp(
	ctx context.Context,
	httpClient *http.Client,
	statusURL string,
	webAppURL string,
	pollInterval time.Duration,
	open func(string) error,
) error {
	if err := requireLoopbackHTTPURL(statusURL); err != nil {
		return fmt.Errorf("invalid desktop web app status URL: %w", err)
	}
	if err := requireLoopbackHTTPURL(webAppURL); err != nil {
		return fmt.Errorf("invalid desktop web app URL: %w", err)
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return err
		}
		response, err := httpClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				return open(webAppURL)
			}
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
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
