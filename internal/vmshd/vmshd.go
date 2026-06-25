package vmshd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tinyrange/vmsh/internal/backend"
	"j5.nz/cc/ccvmd"
	"j5.nz/cc/client"
)

const Kind = "vmshd"

type Status struct {
	Kind      string                 `json:"kind"`
	Status    string                 `json:"status"`
	Sessions  []SessionSummary       `json:"sessions"`
	VMs       []client.InstanceState `json:"vms"`
	StartedAt time.Time              `json:"started_at"`
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SessionSummary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateSessionRequest struct {
	Name string `json:"name,omitempty"`
}

type sessionRegistry struct {
	mu       sync.Mutex
	next     int
	sessions map[string]Session
}

type Server struct {
	token    string
	registry *sessionRegistry

	startedAt time.Time
}

func Main(args []string) {
	started, err := Run(args)
	if err == nil {
		return
	}
	if !started {
		_ = json.NewEncoder(os.Stdout).Encode(client.ServerHello{
			Kind:   "error",
			Error:  "vmshd failed to start",
			Detail: err.Error(),
		})
	}
	fmt.Fprintf(os.Stderr, "vmshd startup failed: %v\n", err)
	os.Exit(1)
}

func Run(args []string) (bool, error) {
	cacheDir, err := resolveCacheDir(scanCacheDir(args))
	if err != nil {
		return false, err
	}
	tokenPath := filepath.Join(cacheDir, "vmshd.token")
	token, err := writeTokenFile(tokenPath)
	if err != nil {
		return false, err
	}

	srv := NewServer(token)
	return ccvmd.RunServer(args, ccvmd.ServerOptions{
		Kind:      Kind,
		TokenPath: tokenPath,
		RegisterHandlers: func(mux *http.ServeMux, runtime ccvmd.RuntimeView) {
			srv.RegisterHandlers(mux, runtime)
		},
		WrapHandler: srv.Authenticate,
	})
}

func NewServer(token string) *Server {
	return &Server{
		token:     token,
		registry:  newSessionRegistry(),
		startedAt: time.Now(),
	}
}

func (s *Server) RegisterHandlers(mux *http.ServeMux, runtime ccvmd.RuntimeView) {
	mux.HandleFunc("GET /vmsh/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Status{
			Kind:      Kind,
			Status:    "ok",
			Sessions:  s.registry.List(),
			VMs:       runtimeInstanceStatuses(runtime),
			StartedAt: s.startedAt,
		})
	})
	mux.HandleFunc("GET /vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.registry.List())
	})
	mux.HandleFunc("POST /vmsh/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req CreateSessionRequest
		if err := decodeOptionalJSON(r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, client.ErrorResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.registry.Create(req.Name))
	})
	mux.HandleFunc("GET /vmsh/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.registry.Get(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, client.ErrorResponse{Error: "session not found"})
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
	mux.HandleFunc("DELETE /vmsh/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.registry.Delete(r.PathValue("id"))
		if !ok {
			writeJSON(w, http.StatusNotFound, client.ErrorResponse{Error: "session not found"})
			return
		}
		writeJSON(w, http.StatusOK, session)
	})
}

func runtimeInstanceStatuses(runtime ccvmd.RuntimeView) []client.InstanceState {
	if runtime == nil {
		return []client.InstanceState{}
	}
	statuses := runtime.InstanceStatuses()
	if statuses == nil {
		return []client.InstanceState{}
	}
	return statuses
}

func (s *Server) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearerToken(r.Header.Get("Authorization"), s.token) {
			writeJSON(w, http.StatusUnauthorized, client.ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validBearerToken(header, want string) bool {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func scanCacheDir(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-cache-dir" || arg == "--cache-dir" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		for _, prefix := range []string{"-cache-dir=", "--cache-dir="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
	}
	return ""
}

func resolveCacheDir(arg string) (string, error) {
	if strings.TrimSpace(arg) != "" {
		if err := os.MkdirAll(arg, 0o700); err != nil {
			return "", err
		}
		return arg, nil
	}
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	dir := filepath.Join(userCacheDir, "vmshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func writeTokenFile(path string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	return token, nil
}

func newToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: map[string]Session{}}
}

func (r *sessionRegistry) Create(name string) Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := fmt.Sprintf("sess_%08x", r.next)
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	now := time.Now()
	session := Session{
		ID:        id,
		Name:      name,
		State:     "detached",
		CreatedAt: now,
		UpdatedAt: now,
	}
	r.sessions[id] = session
	return session
}

func (r *sessionRegistry) Get(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, ok := r.sessions[strings.TrimSpace(id)]
	return session, ok
}

func (r *sessionRegistry) Delete(id string) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = strings.TrimSpace(id)
	session, ok := r.sessions[id]
	if !ok {
		return Session{}, false
	}
	session.State = "closing"
	session.UpdatedAt = time.Now()
	delete(r.sessions, id)
	return session, true
}

func (r *sessionRegistry) List() []SessionSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]SessionSummary, 0, len(ids))
	for _, id := range ids {
		session := r.sessions[id]
		out = append(out, SessionSummary{
			ID:        session.ID,
			Name:      session.Name,
			State:     session.State,
			CreatedAt: session.CreatedAt,
			UpdatedAt: session.UpdatedAt,
		})
	}
	return out
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func NewClient(state backend.DaemonState) (*client.Client, error) {
	api := backend.NewClient(state.Addr)
	if err := backend.ApplyDaemonStateAuth(api, state); err != nil {
		return nil, err
	}
	return api, nil
}
