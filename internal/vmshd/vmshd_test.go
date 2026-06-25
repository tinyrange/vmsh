package vmshd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTokenFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmshd.token")
	token, err := writeTokenFile(path)
	if err != nil {
		t.Fatalf("write token: %v", err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("token mode = %o, want 600", got)
		}
	}
}

func TestAuthenticateRequiresBearerToken(t *testing.T) {
	srv := &Server{token: "secret"}
	handler := srv.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, tc := range []struct {
		name   string
		header string
		status int
	}{
		{name: "missing", status: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic secret", status: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer nope", status: http.StatusUnauthorized},
		{name: "valid", header: "Bearer secret", status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d", rr.Code, tc.status)
			}
		})
	}
}

func TestStatusRoute(t *testing.T) {
	srv := &Server{token: "secret"}
	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/vmsh/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rr.Code, rr.Body.String())
	}
	var status Status
	if err := json.NewDecoder(rr.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Kind != Kind || status.Status != "ok" || len(status.Sessions) != 0 {
		t.Fatalf("status = %+v", status)
	}
}
