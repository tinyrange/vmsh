package vmshd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tinyrange/vmsh/internal/backend"
	"j5.nz/cc/client"
)

type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
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

func (c *HTTPClient) UpdateSession(id string, req UpdateSessionRequest) (Session, error) {
	var session Session
	err := c.doJSON(http.MethodPatch, "/vmsh/sessions/"+id, req, &session)
	return session, err
}

func (c *HTTPClient) Jobs() ([]JobSummary, error) {
	var jobs []JobSummary
	err := c.doJSON(http.MethodGet, "/vmsh/jobs", nil, &jobs)
	return jobs, err
}

func (c *HTTPClient) AttachSession(id string, req AttachSessionRequest) (AttachSessionResponse, error) {
	var resp AttachSessionResponse
	err := c.doJSON(http.MethodPost, "/vmsh/sessions/"+id+"/attach", req, &resp)
	return resp, err
}

func (c *HTTPClient) UpdateTerminal(sessionID, attachmentID string, req Terminal) (AttachSessionResponse, error) {
	var resp AttachSessionResponse
	path := "/vmsh/sessions/" + sessionID + "/attachments/" + attachmentID + "/terminal"
	err := c.doJSON(http.MethodPost, path, req, &resp)
	return resp, err
}

func (c *HTTPClient) DetachSession(id string, req DetachSessionRequest) (Session, error) {
	var session Session
	err := c.doJSON(http.MethodPost, "/vmsh/sessions/"+id+"/detach", req, &session)
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
