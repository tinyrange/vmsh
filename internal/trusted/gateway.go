package trusted

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const maxEventChunk = 32 * 1024

type GatewayConfig struct {
	Profile          Profile
	Grant            Grant
	Token            string
	HandshakeTimeout time.Duration
	AuditPath        string
}

type Envelope struct {
	Token   string  `json:"token"`
	Request Request `json:"request"`
}

type Event struct {
	Kind     string       `json:"kind"`
	JobID    string       `json:"job_id,omitempty"`
	Stream   string       `json:"stream,omitempty"`
	Data     []byte       `json:"data,omitempty"`
	ExitCode *int         `json:"exit_code,omitempty"`
	Error    *PolicyError `json:"error,omitempty"`
	AuditRef string       `json:"audit_ref,omitempty"`
}

type Gateway struct {
	config       GatewayConfig
	listener     net.Listener
	replay       map[string]string
	lastSequence uint64
	mu           sync.Mutex
	once         sync.Once
	revoked      atomic.Bool
}

func ListenGateway(config GatewayConfig) (*Gateway, error) {
	if !ownerOnlyFilesSupported() {
		return nil, policyError(DeniedPrivilege, "trusted calls require owner-only file enforcement on this platform")
	}
	if err := VerifyProfile(config.Profile); err != nil {
		return nil, err
	}
	if config.Profile.Risk != RiskDelegated {
		return nil, policyError(DeniedPrivilege, "the initial gateway only executes narrow delegated profiles")
	}
	if config.Grant.ProfileDigest != config.Profile.Digest || config.Grant.ProfileID != config.Profile.ID || config.Grant.Revoked {
		return nil, policyError(DeniedNoGrant, "gateway grant is absent, stale, or revoked")
	}
	if len(config.Token) < 32 || config.HandshakeTimeout <= 0 {
		return nil, policyError(DeniedMalformedRequest, "a strong token and measured handshake timeout are required")
	}
	if config.AuditPath == "" {
		return nil, policyError(DeniedMalformedRequest, "owner-only audit path is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	gateway := &Gateway{config: config, listener: listener, replay: make(map[string]string)}
	go gateway.serve()
	return gateway, nil
}

func NewToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (g *Gateway) Port() int { return g.listener.Addr().(*net.TCPAddr).Port }

func (g *Gateway) Close() error {
	var err error
	g.once.Do(func() { err = g.listener.Close() })
	return err
}

func (g *Gateway) Revoke() { g.revoked.Store(true) }

func (g *Gateway) serve() {
	for {
		connection, err := g.listener.Accept()
		if err != nil {
			return
		}
		go g.handle(connection)
	}
}

func (g *Gateway) handle(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(g.config.HandshakeTimeout))
	limit := maxRequestSize(g.config.Profile)
	line, err := bufio.NewReader(io.LimitReader(connection, int64(limit)+1)).ReadBytes('\n')
	if err != nil || len(line) > limit {
		if err == nil {
			err = policyError(DeniedMalformedRequest, "request exceeds the gateway metadata limit")
		}
		g.writeDenied(connection, "", policyError(DeniedMalformedRequest, err.Error()))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var envelope Envelope
	if err = decoder.Decode(&envelope); err != nil {
		g.writeDenied(connection, "", policyError(DeniedMalformedRequest, err.Error()))
		return
	}
	_ = connection.SetReadDeadline(time.Time{})
	if subtle.ConstantTimeCompare([]byte(envelope.Token), []byte(g.config.Token)) != 1 {
		g.writeDenied(connection, envelope.Request.CallID, policyError(DeniedSource, "gateway token is invalid"))
		return
	}
	if g.revoked.Load() {
		g.writeDenied(connection, envelope.Request.CallID, policyError(DeniedRevoked, "grant is revoked"))
		return
	}
	// The per-VM gateway determines source identity; guest fields cannot spoof it.
	envelope.Request.SourceVMID = g.config.Grant.SourceVMID
	envelope.Request.SourceGeneration = g.config.Grant.SourceGeneration
	digest, err := RequestDigest(envelope.Request)
	if err != nil {
		g.writeDenied(connection, envelope.Request.CallID, policyError(DeniedMalformedRequest, err.Error()))
		return
	}
	if err := g.reserve(envelope.Request.CallID, digest, envelope.Request.Sequence); err != nil {
		g.writeDenied(connection, envelope.Request.CallID, err)
		return
	}
	decision, err := Evaluate(g.config.Profile, &g.config.Grant, envelope.Request, time.Now())
	if err != nil {
		g.audit(envelope.Request, digest, "denied", -1, err)
		g.writeDenied(connection, envelope.Request.CallID, err)
		return
	}
	if envelope.Request.Stdin || unsupportedPrivilege(g.config.Profile.Actions[decision.ActionID]) {
		err := policyError(DeniedPrivilege, "delegated gateways do not support stdin, terminal, network, credentials, or detached jobs")
		g.audit(envelope.Request, digest, "denied", -1, err)
		g.writeDenied(connection, envelope.Request.CallID, err)
		return
	}
	g.execute(connection, envelope.Request, digest, decision)
}

func (g *Gateway) reserve(callID, digest string, sequence uint64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.replay[callID]; ok {
		detail := "call ID was already accepted"
		if existing != digest {
			detail = "call ID was replayed with different request data"
		}
		return policyError(DeniedReplay, detail)
	}
	if sequence <= g.lastSequence {
		return policyError(DeniedReplay, "source sequence did not increase")
	}
	g.replay[callID] = digest
	g.lastSequence = sequence
	return nil
}

func (g *Gateway) execute(connection net.Conn, request Request, digest string, decision Decision) {
	ctx, cancel := context.WithDeadline(context.Background(), decision.Deadline)
	defer cancel()
	go func() {
		buffer := make([]byte, 1)
		_, _ = connection.Read(buffer)
		cancel()
	}()
	command := exec.CommandContext(ctx, decision.Executable, decision.Arguments...)
	command.Dir = decision.CWD
	command.Env = decision.Environment
	configureProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		g.finishError(connection, request, digest, err)
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		g.finishError(connection, request, digest, err)
		return
	}
	if err := command.Start(); err != nil {
		g.finishError(connection, request, digest, err)
		return
	}
	jobID := randomID()
	encoder := &lockedEncoder{encoder: json.NewEncoder(connection)}
	_ = encoder.Encode(Event{Kind: "accepted", JobID: jobID})
	var streams sync.WaitGroup
	streams.Add(2)
	go streamEvents(&streams, encoder, "stdout", stdout)
	go streamEvents(&streams, encoder, "stderr", stderr)
	err = command.Wait()
	streams.Wait()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		} else if ctx.Err() != nil {
			exitCode = -1
		} else {
			g.finishError(connection, request, digest, err)
			return
		}
	}
	auditRef := g.audit(request, digest, "exit", exitCode, nil)
	_ = encoder.Encode(Event{Kind: "exit", JobID: jobID, ExitCode: &exitCode, AuditRef: auditRef})
}

func streamEvents(group *sync.WaitGroup, encoder *lockedEncoder, stream string, reader io.Reader) {
	defer group.Done()
	buffer := make([]byte, maxEventChunk)
	for {
		count, err := reader.Read(buffer)
		if count > 0 && encoder.Encode(Event{Kind: "output", Stream: stream, Data: append([]byte(nil), buffer[:count]...)}) != nil {
			return
		}
		if err != nil {
			return
		}
	}
}

type lockedEncoder struct {
	mu      sync.Mutex
	encoder *json.Encoder
}

func (e *lockedEncoder) Encode(value any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(value)
}

func (g *Gateway) writeDenied(writer io.Writer, callID string, err error) {
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) {
		policyErr = &PolicyError{Reason: DeniedMalformedRequest, Detail: err.Error()}
	}
	_ = json.NewEncoder(writer).Encode(Event{Kind: "error", JobID: callID, Error: policyErr})
}

func (g *Gateway) finishError(writer io.Writer, request Request, digest string, err error) {
	policyErr := &PolicyError{Reason: DeniedAction, Detail: err.Error()}
	auditRef := g.audit(request, digest, "error", -1, err)
	_ = json.NewEncoder(writer).Encode(Event{Kind: "error", JobID: request.CallID, Error: policyErr, AuditRef: auditRef})
}

type AuditRecord struct {
	Reference        string       `json:"reference"`
	At               time.Time    `json:"at"`
	CallID           string       `json:"call_id"`
	RequestDigest    string       `json:"request_digest"`
	SourceVMID       string       `json:"source_vm_id"`
	SourceGeneration uint64       `json:"source_generation"`
	TargetID         string       `json:"target_id"`
	ProfileID        string       `json:"profile_id"`
	ProfileDigest    string       `json:"profile_digest"`
	ActionID         string       `json:"action_id"`
	RootID           string       `json:"root_id"`
	RelativeCWD      string       `json:"relative_cwd"`
	Result           string       `json:"result"`
	ExitCode         int          `json:"exit_code,omitempty"`
	DenialReason     DenialReason `json:"denial_reason,omitempty"`
}

func (g *Gateway) audit(request Request, digest, result string, exitCode int, callErr error) string {
	reference := randomID()
	record := AuditRecord{Reference: reference, At: time.Now().UTC(), CallID: request.CallID, RequestDigest: digest, SourceVMID: g.config.Grant.SourceVMID, SourceGeneration: g.config.Grant.SourceGeneration, TargetID: request.TargetID, ProfileID: g.config.Profile.ID, ProfileDigest: g.config.Profile.Digest, ActionID: request.ActionID, RootID: request.RootID, RelativeCWD: request.RelativeCWD, Result: result, ExitCode: exitCode}
	var policyErr *PolicyError
	if errors.As(callErr, &policyErr) {
		record.DenialReason = policyErr.Reason
	}
	file, err := os.OpenFile(g.config.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || fileAccessibleByOthers(info) {
		return ""
	}
	if json.NewEncoder(file).Encode(record) != nil {
		return ""
	}
	return reference
}

func maxRequestSize(profile Profile) int {
	maximum := 0
	for _, action := range profile.Actions {
		if action.MaxRequestBytes > maximum {
			maximum = action.MaxRequestBytes
		}
	}
	return maximum + 1024
}

func unsupportedPrivilege(action Action) bool {
	return action.AllowNetwork || action.AllowCredentials || action.AllowTerminal || action.AllowDetach
}

func randomID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return hex.EncodeToString(value)
}
