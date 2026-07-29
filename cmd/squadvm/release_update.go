package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tinyrange/vmsh/internal/version"
)

const (
	squadVMLatestReleaseURL = "https://api.github.com/repos/tinyrange/vmsh/releases/latest"
	squadVMReleaseMaxBytes  = 1024 * 1024
)

var (
	squadVMReleaseHTTPClient = &http.Client{Timeout: 3 * time.Second}
	squadVMOpenReleaseURL    = openSquadVMReleaseURL
)

type squadVMReleaseUpdate struct {
	Version     string
	DownloadURL string
	Size        int64
}

type squadVMGitHubRelease struct {
	TagName string                      `json:"tag_name"`
	Assets  []squadVMGitHubReleaseAsset `json:"assets"`
}

type squadVMGitHubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func checkLatestSquadVMRelease(
	ctx context.Context,
	httpClient *http.Client,
	releaseURL, currentVersion, goos, goarch string,
) (*squadVMReleaseUpdate, error) {
	if _, ok := parseSquadVMReleaseVersion(currentVersion); !ok {
		// Development builds and custom versions must never be offered a
		// potentially older stable release.
		return nil, nil
	}
	if err := requireSquadVMHTTPSURL(releaseURL); err != nil {
		return nil, fmt.Errorf("check SquadVM release: %w", err)
	}
	if httpClient == nil {
		httpClient = squadVMReleaseHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "SquadVM/"+strings.TrimSpace(currentVersion))
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check SquadVM release: %w", err)
	}
	defer resp.Body.Close()
	if err := requireSquadVMHTTPSURL(resp.Request.URL.String()); err != nil {
		return nil, fmt.Errorf("check SquadVM release: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("check SquadVM release: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var release squadVMGitHubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, squadVMReleaseMaxBytes+1)).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode SquadVM release: %w", err)
	}
	if !newerSquadVMRelease(currentVersion, release.TagName) {
		return nil, nil
	}
	assetName := squadVMReleaseAssetName(release.TagName, goos, goarch)
	for _, asset := range release.Assets {
		if asset.Name != assetName || strings.TrimSpace(asset.BrowserDownloadURL) == "" {
			continue
		}
		if err := requireSquadVMHTTPSURL(asset.BrowserDownloadURL); err != nil {
			return nil, fmt.Errorf("check SquadVM release asset: %w", err)
		}
		return &squadVMReleaseUpdate{
			Version:     release.TagName,
			DownloadURL: asset.BrowserDownloadURL,
			Size:        asset.Size,
		}, nil
	}
	return nil, nil
}

func squadVMReleaseAssetName(tag, goos, goarch string) string {
	suffix := ""
	switch goos {
	case "darwin":
		suffix = ".zip"
	case "windows":
		suffix = ".exe"
	}
	return "SquadVM_" + tag + "_" + goos + "_" + goarch + suffix
}

type squadVMReleaseVersion struct {
	numbers    [3]uint64
	prerelease []string
}

func newerSquadVMRelease(current, candidate string) bool {
	currentVersion, currentOK := parseSquadVMReleaseVersion(current)
	candidateVersion, candidateOK := parseSquadVMReleaseVersion(candidate)
	if !currentOK || !candidateOK {
		return false
	}
	for index := range currentVersion.numbers {
		if candidateVersion.numbers[index] != currentVersion.numbers[index] {
			return candidateVersion.numbers[index] > currentVersion.numbers[index]
		}
	}
	return compareSquadVMPrerelease(candidateVersion.prerelease, currentVersion.prerelease) > 0
}

func parseSquadVMReleaseVersion(value string) (squadVMReleaseVersion, bool) {
	var parsed squadVMReleaseVersion
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "v") {
		return parsed, false
	}
	value = strings.TrimPrefix(value, "v")
	if build := strings.IndexByte(value, '+'); build >= 0 {
		value = value[:build]
	}
	if separator := strings.IndexByte(value, '-'); separator >= 0 {
		prerelease := value[separator+1:]
		value = value[:separator]
		if prerelease == "" {
			return parsed, false
		}
		parsed.prerelease = strings.Split(prerelease, ".")
		for _, identifier := range parsed.prerelease {
			if identifier == "" {
				return squadVMReleaseVersion{}, false
			}
			for _, char := range identifier {
				if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
					(char >= '0' && char <= '9') || char == '-' {
					continue
				}
				return squadVMReleaseVersion{}, false
			}
		}
	}
	core := strings.Split(value, ".")
	if len(core) != len(parsed.numbers) {
		return squadVMReleaseVersion{}, false
	}
	for index, component := range core {
		if component == "" || (len(component) > 1 && component[0] == '0') {
			return squadVMReleaseVersion{}, false
		}
		number, err := strconv.ParseUint(component, 10, 64)
		if err != nil {
			return squadVMReleaseVersion{}, false
		}
		parsed.numbers[index] = number
	}
	return parsed, true
}

func compareSquadVMPrerelease(left, right []string) int {
	if len(left) == 0 && len(right) == 0 {
		return 0
	}
	if len(left) == 0 {
		return 1
	}
	if len(right) == 0 {
		return -1
	}
	for index := 0; index < min(len(left), len(right)); index++ {
		leftNumber, leftNumeric := squadVMPrereleaseNumber(left[index])
		rightNumber, rightNumeric := squadVMPrereleaseNumber(right[index])
		switch {
		case leftNumeric && rightNumeric && leftNumber != rightNumber:
			if leftNumber > rightNumber {
				return 1
			}
			return -1
		case leftNumeric != rightNumeric:
			if leftNumeric {
				return -1
			}
			return 1
		case !leftNumeric && left[index] != right[index]:
			return strings.Compare(left[index], right[index])
		}
	}
	return len(left) - len(right)
}

func squadVMPrereleaseNumber(value string) (uint64, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	number, err := strconv.ParseUint(value, 10, 64)
	return number, err == nil
}

func openSquadVMReleaseURL(value string) error {
	if err := requireSquadVMHTTPSURL(value); err != nil {
		return err
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", value)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", value)
	default:
		command = exec.Command("xdg-open", value)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("open SquadVM download: %w", err)
	}
	go func() {
		_ = command.Wait()
	}()
	return nil
}

func requireSquadVMHTTPSURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return fmt.Errorf("release URL must use HTTPS")
	}
	return nil
}

func currentSquadVMVersion() string {
	return version.Current().Version
}
