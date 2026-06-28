package shell

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	codexAgentProxyTokenHeader = "X-VMSH-Agent-Token"
	codexAgentProxyTokenEnv    = "VMSH_CODEX_AGENT_TOKEN"

	codexAgentProxyOpenAIBase  = "https://api.openai.com/v1"
	codexAgentProxyChatGPTBase = "https://chatgpt.com/backend-api/codex"
	codexAgentProxyRefreshURL  = "https://auth.openai.com/oauth/token"
	codexAgentProxyClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var (
	codexAgentProxyHTTPClient      = &http.Client{Transport: codexAgentProxyRoundTripper()}
	codexAgentProxyOpenAIUpstream  = codexAgentProxyOpenAIBase
	codexAgentProxyChatGPTUpstream = codexAgentProxyChatGPTBase
	codexAgentProxyDebugRequests   atomic.Uint64
)

type codexAgentProxy struct {
	server *http.Server
	ln     net.Listener
	token  string
	auth   *codexAgentProxyAuthStore
}

type codexAgentProxyAuthStore struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
}

type codexAgentProxyAuthFile struct {
	AuthMode            string                    `json:"auth_mode,omitempty"`
	OpenAIAPIKey        string                    `json:"OPENAI_API_KEY,omitempty"`
	Tokens              *codexAgentProxyTokenData `json:"tokens,omitempty"`
	LastRefresh         string                    `json:"last_refresh,omitempty"`
	AgentIdentity       string                    `json:"agent_identity,omitempty"`
	PersonalAccessToken string                    `json:"personal_access_token,omitempty"`
}

type codexAgentProxyTokenData struct {
	IDToken      json.RawMessage `json:"id_token,omitempty"`
	AccessToken  string          `json:"access_token,omitempty"`
	RefreshToken string          `json:"refresh_token,omitempty"`
	AccountID    string          `json:"account_id,omitempty"`
}

type codexAgentProxyAuth struct {
	bearer       string
	accountID    string
	fedRamp      bool
	upstreamBase string
	refreshable  bool
}

type codexAgentProxyRefreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type codexAgentProxyRefreshResponse struct {
	IDToken      *string `json:"id_token,omitempty"`
	AccessToken  *string `json:"access_token,omitempty"`
	RefreshToken *string `json:"refresh_token,omitempty"`
}

func startCodexAgentProxy(hostCodexHome string) (*codexAgentProxy, error) {
	auth := &codexAgentProxyAuthStore{
		path: filepath.Join(hostCodexHome, "auth.json"),
		now:  time.Now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := auth.auth(ctx, true); err != nil {
		return nil, err
	}
	token, err := randomCodexAgentProxyToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start Codex auth proxy: %w", err)
	}
	proxy := &codexAgentProxy{ln: ln, token: token, auth: auth}
	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
	}
	proxy.server = server
	go func() {
		if err := server.Serve(ln); err != nil &&
			!errors.Is(err, http.ErrServerClosed) &&
			!errors.Is(err, net.ErrClosed) {
			// There is no logger on shellState; stderr may already belong to the guest TTY.
			// Keep unexpected proxy shutdown silent here and let the guest request fail.
		}
	}()
	return proxy, nil
}

func (p *codexAgentProxy) Port() int {
	if p == nil || p.ln == nil {
		return 0
	}
	if addr, ok := p.ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

func (p *codexAgentProxy) Token() string {
	if p == nil {
		return ""
	}
	return p.token
}

func (p *codexAgentProxy) Close() {
	if p == nil || p.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = p.server.Shutdown(ctx)
}

func (p *codexAgentProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := codexAgentProxyDebugRequests.Add(1)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(codexAgentProxyTokenHeader)), []byte(p.token)) != 1 {
		debugCodexAgentProxyf(requestID, "reject forbidden method=%s path=%s", r.Method, r.URL.Path)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !codexAgentProxyPathAllowed(r.Method, r.URL.Path) {
		debugCodexAgentProxyf(requestID, "reject not-found method=%s path=%s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		debugCodexAgentProxyf(requestID, "read request body failed method=%s path=%s err=%v", r.Method, r.URL.Path, err)
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	debugCodexAgentProxyf(requestID, "request method=%s path=%s body_bytes=%d", r.Method, r.URL.RequestURI(), len(body))
	auth, err := p.auth.auth(r.Context(), true)
	if err != nil {
		debugCodexAgentProxyf(requestID, "load auth failed err=%v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := p.forward(r, body, auth)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && auth.refreshable {
		debugCodexAgentProxyf(requestID, "upstream unauthorized; refreshing auth")
		_ = resp.Body.Close()
		if refreshErr := p.auth.refresh(r.Context()); refreshErr == nil {
			if refreshed, authErr := p.auth.auth(r.Context(), false); authErr == nil {
				resp, err = p.forward(r, body, refreshed)
			} else {
				err = authErr
			}
		} else {
			debugCodexAgentProxyf(requestID, "refresh auth failed err=%v", refreshErr)
		}
	}
	if err != nil {
		debugCodexAgentProxyf(requestID, "forward failed err=%v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	debugCodexAgentProxyf(requestID, "upstream response status=%s content_type=%q content_length=%d transfer=%v", resp.Status, resp.Header.Get("Content-Type"), resp.ContentLength, resp.TransferEncoding)
	copyHTTPHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	var written int64
	if flusher, ok := w.(http.Flusher); ok {
		written, err = io.Copy(codexAgentProxyFlushWriter{w: w, flusher: flusher}, resp.Body)
		debugCodexAgentProxyf(requestID, "stream copy finished bytes=%d err=%v", written, err)
		return
	}
	written, err = io.Copy(w, resp.Body)
	debugCodexAgentProxyf(requestID, "copy finished bytes=%d err=%v", written, err)
}

func (p *codexAgentProxy) forward(r *http.Request, body []byte, auth codexAgentProxyAuth) (*http.Response, error) {
	targetURL := codexAgentProxyUpstreamURL(auth.upstreamBase, r.URL.Path, r.URL.RawQuery)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyProxyRequestHeaders(req.Header, r.Header)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Authorization", "Bearer "+auth.bearer)
	if auth.accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", auth.accountID)
	}
	if auth.fedRamp {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	}
	return codexAgentProxyHTTPClient.Do(req)
}

func codexAgentProxyPathAllowed(method, path string) bool {
	switch path {
	case "/v1/responses", "/v1/responses/compact":
		return method == http.MethodPost
	case "/v1/models":
		return method == http.MethodGet
	default:
		return false
	}
}

func codexAgentProxyUpstreamURL(base, guestPath, rawQuery string) string {
	pathSuffix := strings.TrimPrefix(guestPath, "/v1")
	if !strings.HasPrefix(pathSuffix, "/") {
		pathSuffix = "/" + pathSuffix
	}
	target := strings.TrimRight(base, "/") + pathSuffix
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return target
}

func (s *codexAgentProxyAuthStore) auth(ctx context.Context, refreshIfNeeded bool) (codexAgentProxyAuth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	authFile, err := s.load()
	if err != nil {
		return codexAgentProxyAuth{}, err
	}
	if refreshIfNeeded && authFile.shouldRefresh(s.now()) {
		if err := s.refreshLocked(ctx, &authFile); err == nil {
			authFile, err = s.load()
			if err != nil {
				return codexAgentProxyAuth{}, err
			}
		}
	}
	return authFile.proxyAuth()
}

func (s *codexAgentProxyAuthStore) refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	authFile, err := s.load()
	if err != nil {
		return err
	}
	return s.refreshLocked(ctx, &authFile)
}

func (s *codexAgentProxyAuthStore) load() (codexAgentProxyAuthFile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return codexAgentProxyAuthFile{}, fmt.Errorf("@agent --proxy codex requires %s; run `codex login` on the host first", s.path)
		}
		return codexAgentProxyAuthFile{}, err
	}
	var authFile codexAgentProxyAuthFile
	if err := json.Unmarshal(data, &authFile); err != nil {
		return codexAgentProxyAuthFile{}, fmt.Errorf("read Codex auth: %w", err)
	}
	return authFile, nil
}

func (s *codexAgentProxyAuthStore) save(authFile codexAgentProxyAuthFile) error {
	data, err := json.MarshalIndent(authFile, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(s.path, data, 0o600)
}

func (s *codexAgentProxyAuthStore) refreshLocked(ctx context.Context, authFile *codexAgentProxyAuthFile) error {
	if authFile == nil || authFile.Tokens == nil || strings.TrimSpace(authFile.Tokens.RefreshToken) == "" {
		return fmt.Errorf("Codex ChatGPT auth is missing a refresh token")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body := codexAgentProxyRefreshRequest{
		ClientID:     codexAgentProxyClientID,
		GrantType:    "refresh_token",
		RefreshToken: authFile.Tokens.RefreshToken,
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexAgentProxyRefreshEndpoint(), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := codexAgentProxyHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("refresh Codex ChatGPT token: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var refresh codexAgentProxyRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&refresh); err != nil {
		return err
	}
	if refresh.IDToken != nil {
		raw, err := json.Marshal(*refresh.IDToken)
		if err != nil {
			return err
		}
		authFile.Tokens.IDToken = raw
		if authFile.Tokens.AccountID == "" {
			authFile.Tokens.AccountID = codexAgentJWTAccountID(*refresh.IDToken)
		}
	}
	if refresh.AccessToken != nil {
		authFile.Tokens.AccessToken = *refresh.AccessToken
	}
	if refresh.RefreshToken != nil {
		authFile.Tokens.RefreshToken = *refresh.RefreshToken
	}
	authFile.LastRefresh = s.now().UTC().Format(time.RFC3339Nano)
	return s.save(*authFile)
}

func (a codexAgentProxyAuthFile) proxyAuth() (codexAgentProxyAuth, error) {
	mode := strings.ToLower(strings.ReplaceAll(a.AuthMode, "_", "-"))
	apiKey := strings.TrimSpace(a.OpenAIAPIKey)
	tokenBacked := a.Tokens != nil && strings.TrimSpace(a.Tokens.AccessToken) != ""
	if mode == "api-key" || mode == "apikey" || (mode == "" && apiKey != "" && !tokenBacked) {
		if apiKey == "" {
			return codexAgentProxyAuth{}, fmt.Errorf("Codex API key auth is selected but OPENAI_API_KEY is empty")
		}
		return codexAgentProxyAuth{
			bearer:       apiKey,
			upstreamBase: codexAgentProxyOpenAIUpstream,
		}, nil
	}
	if tokenBacked {
		idToken := a.Tokens.idTokenString()
		accountID := strings.TrimSpace(a.Tokens.AccountID)
		if accountID == "" && idToken != "" {
			accountID = codexAgentJWTAccountID(idToken)
		}
		return codexAgentProxyAuth{
			bearer:       strings.TrimSpace(a.Tokens.AccessToken),
			accountID:    accountID,
			fedRamp:      codexAgentJWTFedRAMP(idToken),
			upstreamBase: codexAgentProxyChatGPTUpstream,
			refreshable:  strings.TrimSpace(a.Tokens.RefreshToken) != "",
		}, nil
	}
	return codexAgentProxyAuth{}, fmt.Errorf("@agent --proxy codex requires file-backed Codex ChatGPT or API key auth in auth.json")
}

func (a codexAgentProxyAuthFile) shouldRefresh(now time.Time) bool {
	if a.Tokens == nil || strings.TrimSpace(a.Tokens.RefreshToken) == "" {
		return false
	}
	if exp, ok := codexAgentJWTExpiration(a.Tokens.AccessToken); ok {
		return !exp.After(now.Add(5 * time.Minute))
	}
	if a.LastRefresh == "" {
		return false
	}
	lastRefresh, err := time.Parse(time.RFC3339Nano, a.LastRefresh)
	if err != nil {
		return false
	}
	return lastRefresh.Before(now.Add(-8 * 24 * time.Hour))
}

func (t codexAgentProxyTokenData) idTokenString() string {
	var token string
	if len(t.IDToken) == 0 {
		return ""
	}
	if err := json.Unmarshal(t.IDToken, &token); err == nil {
		return token
	}
	var parsed struct {
		RawJWT string `json:"raw_jwt"`
	}
	if err := json.Unmarshal(t.IDToken, &parsed); err == nil {
		return parsed.RawJWT
	}
	return ""
}

func codexAgentProxyRefreshEndpoint() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE")); value != "" {
		return value
	}
	return codexAgentProxyRefreshURL
}

func randomCodexAgentProxyToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func codexAgentJWTExpiration(jwt string) (time.Time, bool) {
	payload, ok := codexAgentJWTPayload(jwt)
	if !ok {
		return time.Time{}, false
	}
	switch exp := payload["exp"].(type) {
	case float64:
		return time.Unix(int64(exp), 0), true
	case json.Number:
		value, err := exp.Int64()
		if err == nil {
			return time.Unix(value, 0), true
		}
	}
	return time.Time{}, false
}

func codexAgentJWTAccountID(jwt string) string {
	authClaims := codexAgentJWTAuthClaims(jwt)
	if value, ok := authClaims["chatgpt_account_id"].(string); ok {
		return value
	}
	if value, ok := authClaims["account_id"].(string); ok {
		return value
	}
	return ""
}

func codexAgentJWTFedRAMP(jwt string) bool {
	authClaims := codexAgentJWTAuthClaims(jwt)
	value, _ := authClaims["chatgpt_account_is_fedramp"].(bool)
	return value
}

func codexAgentJWTAuthClaims(jwt string) map[string]any {
	payload, ok := codexAgentJWTPayload(jwt)
	if !ok {
		return nil
	}
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		return auth
	}
	return payload
}

func codexAgentJWTPayload(jwt string) (map[string]any, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 || parts[1] == "" {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false
	}
	return payload, true
}

func copyProxyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if codexAgentProxyHeaderBlocked(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyHTTPHeader(dst, src http.Header) {
	for key, values := range src {
		if codexAgentProxyHeaderBlocked(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func codexAgentProxyHeaderBlocked(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "connection", "content-length", "host", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
		strings.ToLower(codexAgentProxyTokenHeader):
		return true
	default:
		return false
	}
}

type codexAgentProxyFlushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func (w codexAgentProxyFlushWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.flusher.Flush()
	return n, err
}

func codexAgentProxyRoundTripper() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	clone := transport.Clone()
	clone.DisableCompression = true
	return clone
}

func debugCodexAgentProxyf(requestID uint64, format string, args ...any) {
	if strings.TrimSpace(os.Getenv("VMSH_CODEX_PROXY_DEBUG")) == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "vmsh codex proxy[%d]: %s\n", requestID, fmt.Sprintf(format, args...))
}
