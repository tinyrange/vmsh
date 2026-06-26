package vmshd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/tinyrange/vmsh/internal/backend"
	"golang.org/x/net/websocket"
	"j5.nz/cc/client"
)

type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

type TerminalStream struct {
	ws        *websocket.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func NewHTTPClient(state backend.DaemonState) (*HTTPClient, error) {
	token, err := backend.ReadDaemonToken(state.TokenPath)
	if err != nil {
		return nil, err
	}
	return &HTTPClient{
		baseURL: "http://" + state.Addr,
		token:   token,
		client:  http.DefaultClient,
	}, nil
}

func (c *HTTPClient) CreateSession(req CreateSessionRequest) (Session, error) {
	var session Session
	err := c.doJSON(http.MethodPost, "/vmsh/sessions", req, &session)
	return session, err
}

func (c *HTTPClient) Status() (Status, error) {
	var status Status
	err := c.doJSON(http.MethodGet, "/vmsh/status", nil, &status)
	return status, err
}

func (c *HTTPClient) Sessions() ([]SessionSummary, error) {
	var sessions []SessionSummary
	err := c.doJSON(http.MethodGet, "/vmsh/sessions", nil, &sessions)
	return sessions, err
}

func (c *HTTPClient) Session(id string) (Session, error) {
	var session Session
	err := c.doJSON(http.MethodGet, "/vmsh/sessions/"+url.PathEscape(id), nil, &session)
	return session, err
}

func (c *HTTPClient) UpdateSession(id string, req UpdateSessionRequest) (Session, error) {
	var session Session
	err := c.doJSON(http.MethodPatch, "/vmsh/sessions/"+url.PathEscape(id), req, &session)
	return session, err
}

func (c *HTTPClient) Jobs() ([]JobSummary, error) {
	var jobs []JobSummary
	err := c.doJSON(http.MethodGet, "/vmsh/jobs", nil, &jobs)
	return jobs, err
}

func (c *HTTPClient) StartHostJob(sessionID string, req StartHostJobRequest) (JobSummary, error) {
	var job JobSummary
	err := c.doJSON(http.MethodPost, "/vmsh/sessions/"+url.PathEscape(sessionID)+"/jobs", req, &job)
	return job, err
}

func (c *HTTPClient) CancelHostJob(sessionID string, jobID int) (JobSummary, error) {
	var job JobSummary
	path := fmt.Sprintf("/vmsh/sessions/%s/jobs/%d", url.PathEscape(sessionID), jobID)
	err := c.doJSON(http.MethodDelete, path, nil, &job)
	return job, err
}

func (c *HTTPClient) AttachSession(id string, req AttachSessionRequest) (AttachSessionResponse, error) {
	var resp AttachSessionResponse
	err := c.doJSON(http.MethodPost, "/vmsh/sessions/"+url.PathEscape(id)+"/attach", req, &resp)
	return resp, err
}

func (c *HTTPClient) UpdateTerminal(sessionID, attachmentID string, req Terminal) (AttachSessionResponse, error) {
	var resp AttachSessionResponse
	path := "/vmsh/sessions/" + url.PathEscape(sessionID) + "/attachments/" + url.PathEscape(attachmentID) + "/terminal"
	err := c.doJSON(http.MethodPost, path, req, &resp)
	return resp, err
}

func (c *HTTPClient) DialTerminalStream(ctx context.Context, sessionID, attachmentID string) (*TerminalStream, error) {
	path := "/vmsh/sessions/" + url.PathEscape(sessionID) + "/attachments/" + url.PathEscape(attachmentID) + "/stream"
	wsURL, err := websocketURL(c.baseURL, path)
	if err != nil {
		return nil, err
	}
	cfg, err := websocket.NewConfig(wsURL, c.baseURL)
	if err != nil {
		return nil, err
	}
	if cfg.Header == nil {
		cfg.Header = http.Header{}
	}
	cfg.Header.Set("Authorization", "Bearer "+c.token)
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		return nil, err
	}
	stream := &TerminalStream{ws: ws, done: make(chan struct{})}
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = stream.Close()
			case <-stream.done:
			}
		}()
	}
	return stream, nil
}

func (s *TerminalStream) Send(msg TerminalStreamMessage) error {
	if s == nil || s.ws == nil {
		return fmt.Errorf("terminal stream is closed")
	}
	return websocket.JSON.Send(s.ws, msg)
}

func (s *TerminalStream) Receive() (TerminalStreamMessage, error) {
	var msg TerminalStreamMessage
	if s == nil || s.ws == nil {
		return msg, fmt.Errorf("terminal stream is closed")
	}
	err := websocket.JSON.Receive(s.ws, &msg)
	return msg, err
}

func (s *TerminalStream) Resize(term Terminal) error {
	return s.Send(TerminalStreamMessage{Kind: "resize", Terminal: &term})
}

func (s *TerminalStream) Write(data []byte) error {
	return s.Send(TerminalStreamMessage{Kind: "stdin", Data: data})
}

func (s *TerminalStream) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		if s.ws != nil {
			err = s.ws.Close()
			s.ws = nil
		}
	})
	return err
}

func (c *HTTPClient) DetachSession(id string, req DetachSessionRequest) (Session, error) {
	var session Session
	err := c.doJSON(http.MethodPost, "/vmsh/sessions/"+url.PathEscape(id)+"/detach", req, &session)
	return session, err
}

func (c *HTTPClient) doJSON(method, path string, reqBody any, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(reqBody); err != nil {
			return err
		}
		body = buf
	}
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr client.ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && strings.TrimSpace(apiErr.Error) != "" {
			return fmt.Errorf("%s", apiErr.Error)
		}
		return fmt.Errorf("vmshd request %s %s returned status %d", method, path, resp.StatusCode)
	}
	if respBody == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(respBody)
}

func websocketURL(baseURL, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported base URL scheme %q", u.Scheme)
	}
	u.Path = path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
