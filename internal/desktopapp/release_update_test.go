package desktopapp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLatestSquadVMReleaseSelectsCompatibleNewerAsset(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		fmt.Fprintf(w, `{
			"tag_name":"v0.7.0",
			"assets":[
				{"name":"SquadVM_v0.7.0_linux_amd64","browser_download_url":%q,"size":1234},
				{"name":"SquadVM_v0.7.0_windows_amd64.exe","browser_download_url":%q,"size":5678}
			]
		}`, server.URL+"/linux", server.URL+"/windows")
	}))
	defer server.Close()

	update, err := checkLatestRelease(
		context.Background(),
		server.Client(),
		server.URL,
		"v0.6.1",
		"linux",
		"amd64",
	)
	if err != nil {
		t.Fatal(err)
	}
	if update == nil {
		t.Fatal("newer compatible release was not reported")
	}
	if update.Version != "v0.7.0" || update.DownloadURL != server.URL+"/linux" || update.Size != 1234 {
		t.Fatalf("update = %+v", update)
	}
}

func TestLatestSquadVMReleaseDoesNotOfferDowngradeOrWrongPlatform(t *testing.T) {
	tests := []struct {
		name      string
		current   string
		tag       string
		asset     string
		wantCalls int
	}{
		{
			name:      "newer current version",
			current:   "v0.8.0",
			tag:       "v0.7.0",
			asset:     "SquadVM_v0.7.0_linux_amd64",
			wantCalls: 1,
		},
		{
			name:      "development build",
			current:   "devel",
			tag:       "v0.7.0",
			asset:     "SquadVM_v0.7.0_linux_amd64",
			wantCalls: 0,
		},
		{
			name:      "missing platform asset",
			current:   "v0.6.1",
			tag:       "v0.7.0",
			asset:     "SquadVM_v0.7.0_windows_amd64.exe",
			wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"size":1}]}`,
					test.tag, test.asset, server.URL+"/asset")
			}))
			defer server.Close()

			update, err := checkLatestRelease(
				context.Background(),
				server.Client(),
				server.URL,
				test.current,
				"linux",
				"amd64",
			)
			if err != nil {
				t.Fatal(err)
			}
			if update != nil {
				t.Fatalf("unexpected update = %+v", update)
			}
			if calls != test.wantCalls {
				t.Fatalf("release requests = %d, want %d", calls, test.wantCalls)
			}
		})
	}
}

func TestNewerSquadVMReleaseUsesSemanticVersionOrder(t *testing.T) {
	tests := []struct {
		current   string
		candidate string
		newer     bool
	}{
		{current: "v0.6.1", candidate: "v0.7.0", newer: true},
		{current: "v0.10.0", candidate: "v0.9.9", newer: false},
		{current: "v1.0.0-rc.1", candidate: "v1.0.0", newer: true},
		{current: "v1.0.0", candidate: "v1.0.1-rc.1", newer: true},
		{current: "v1.0.0", candidate: "v1.0.0-rc.2", newer: false},
		{current: "devel", candidate: "v99.0.0", newer: false},
	}
	for _, test := range tests {
		if got := newerRelease(test.current, test.candidate); got != test.newer {
			t.Errorf("newerRelease(%q, %q) = %t, want %t",
				test.current, test.candidate, got, test.newer)
		}
	}
}

func TestReleaseUpdateApplyOpensSelectedAsset(t *testing.T) {
	const downloadURL = "https://github.com/tinyrange/vmsh/releases/download/v0.7.0/SquadVM_v0.7.0_linux_amd64"
	viewer := &displayViewer{
		preflight: startupPreflight{
			ReleaseUpdate: &releaseUpdate{
				Version:     "v0.7.0",
				DownloadURL: downloadURL,
			},
		},
	}
	previousOpen := openReleaseURL
	defer func() {
		openReleaseURL = previousOpen
	}()
	var opened string
	openReleaseURL = func(value string) error {
		opened = value
		return nil
	}

	viewer.applyUpdateNotification(releaseUpdateNotification)

	if opened != downloadURL {
		t.Fatalf("opened release URL = %q, want %q", opened, downloadURL)
	}
	if !viewer.releaseDismissed {
		t.Fatal("release notification remained visible after applying")
	}
}
