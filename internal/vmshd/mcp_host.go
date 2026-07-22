package vmshd

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"j5.nz/cc/client"
)

const (
	mcpHostChallengeLifetime = time.Hour
	mcpHostManifestMaxBytes  = 8 << 20
)

type mcpHostChallenge struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Directory    string    `json:"directory"`
	ResponsePath string    `json:"response_path"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Nonce        string    `json:"-"`
}

type mcpHostReadGrant struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Directory bool      `json:"directory"`
	CreatedAt time.Time `json:"created_at"`
	info      os.FileInfo
}

type mcpHostReadChallengeInput struct {
	Directory string `json:"directory" jsonschema:"absolute host directory in which the agent can create the response file; no path component may be a symlink"`
}

type mcpHostReadChallengeOutput struct {
	ChallengeID  string    `json:"challenge_id"`
	ResponsePath string    `json:"response_path"`
	ExpiresAt    time.Time `json:"expires_at"`
	Response     string    `json:"response_format"`
}

func (e *mcpEndpoint) createHostReadChallenge(_ context.Context, _ *mcp.CallToolRequest, in mcpHostReadChallengeInput) (*mcp.CallToolResult, mcpHostReadChallengeOutput, error) {
	directory, err := lexicalAbsoluteHostDirectory(in.Directory)
	if err != nil {
		return nil, mcpHostReadChallengeOutput{}, err
	}
	id, err := randomMCPID("hostread")
	if err != nil {
		return nil, mcpHostReadChallengeOutput{}, err
	}
	nonce, err := randomMCPSecret()
	if err != nil {
		return nil, mcpHostReadChallengeOutput{}, err
	}
	now := time.Now().UTC()
	challenge := &mcpHostChallenge{
		ID:           id,
		Kind:         "read",
		Directory:    directory,
		ResponsePath: filepath.Join(directory, ".vmsh-read-"+nonce),
		CreatedAt:    now,
		ExpiresAt:    now.Add(mcpHostChallengeLifetime),
	}
	if err := e.storeHostChallenge(challenge); err != nil {
		return nil, mcpHostReadChallengeOutput{}, err
	}
	return nil, mcpHostReadChallengeOutput{
		ChallengeID:  challenge.ID,
		ResponsePath: challenge.ResponsePath,
		ExpiresAt:    challenge.ExpiresAt,
		Response:     `{"paths":["direct-child", "."]}`,
	}, nil
}

type mcpHostReadManifest struct {
	Paths []string `json:"paths"`
}

type mcpHostReadClaimInput struct {
	ChallengeID string `json:"challenge_id" jsonschema:"ID returned by vm_host_read_challenge after writing the requested response file"`
}

type mcpHostReadClaimOutput struct {
	Grants []mcpHostReadGrant `json:"grants"`
}

func (e *mcpEndpoint) claimHostReadChallenge(_ context.Context, _ *mcp.CallToolRequest, in mcpHostReadClaimInput) (*mcp.CallToolResult, mcpHostReadClaimOutput, error) {
	challenge, err := e.takeHostChallenge(in.ChallengeID, "read")
	if err != nil {
		return nil, mcpHostReadClaimOutput{}, err
	}
	restore := true
	defer func() {
		if restore {
			e.restoreHostChallenge(challenge)
		}
	}()
	if err := validateHostPathWithoutSymlinks(challenge.Directory, true); err != nil {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("validate challenge directory: %w", err)
	}
	responseInfo, err := os.Lstat(challenge.ResponsePath)
	if err != nil {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("open challenge response %q: %w", challenge.ResponsePath, err)
	}
	if !responseInfo.Mode().IsRegular() || responseInfo.Mode()&os.ModeSymlink != 0 {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("challenge response %q must be a regular file", challenge.ResponsePath)
	}
	if responseInfo.Size() > mcpHostManifestMaxBytes {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("challenge response metadata is too large (%d bytes)", responseInfo.Size())
	}
	response, err := os.Open(challenge.ResponsePath)
	if err != nil {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("open challenge response %q: %w", challenge.ResponsePath, err)
	}
	openedInfo, statErr := response.Stat()
	if statErr != nil || !os.SameFile(responseInfo, openedInfo) {
		_ = response.Close()
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("challenge response changed while it was being opened")
	}
	var manifest mcpHostReadManifest
	decoder := json.NewDecoder(io.LimitReader(response, mcpHostManifestMaxBytes+1))
	decodeErr := decoder.Decode(&manifest)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			decodeErr = fmt.Errorf("response must contain one JSON object")
		}
	}
	closeErr := response.Close()
	if decodeErr != nil {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("parse challenge response: %w", decodeErr)
	}
	if closeErr != nil {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("close challenge response: %w", closeErr)
	}
	if len(manifest.Paths) == 0 {
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("challenge response must name at least one path")
	}
	type candidate struct {
		path string
		info os.FileInfo
	}
	candidates := make([]candidate, 0, len(manifest.Paths))
	seen := make(map[string]struct{}, len(manifest.Paths))
	for _, name := range manifest.Paths {
		if err := validateDirectHostGrantName(name); err != nil {
			return nil, mcpHostReadClaimOutput{}, err
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		path := challenge.Directory
		if name != "." {
			path = filepath.Join(challenge.Directory, name)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, mcpHostReadClaimOutput{}, fmt.Errorf("inspect granted path %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, mcpHostReadClaimOutput{}, fmt.Errorf("granted path %q is a symlink", path)
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil, mcpHostReadClaimOutput{}, fmt.Errorf("granted path %q is not a regular file or directory", path)
		}
		if info.IsDir() && name != "." {
			return nil, mcpHostReadClaimOutput{}, fmt.Errorf("directory %q needs a challenge response created inside that directory", path)
		}
		candidates = append(candidates, candidate{path: path, info: info})
	}
	grants := make([]mcpHostReadGrant, 0, len(candidates))
	for _, candidate := range candidates {
		id, err := randomMCPID("hostgrant")
		if err != nil {
			return nil, mcpHostReadClaimOutput{}, err
		}
		grants = append(grants, mcpHostReadGrant{ID: id, Path: candidate.path, Directory: candidate.info.IsDir(), CreatedAt: time.Now().UTC(), info: candidate.info})
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("MCP endpoint is stopped")
	}
	if err := os.Remove(challenge.ResponsePath); err != nil {
		e.mu.Unlock()
		return nil, mcpHostReadClaimOutput{}, fmt.Errorf("remove consumed challenge response: %w", err)
	}
	if e.hostReadGrants == nil {
		e.hostReadGrants = make(map[string]*mcpHostReadGrant)
	}
	for i := range grants {
		grant := grants[i]
		e.hostReadGrants[grant.ID] = &grant
	}
	e.mu.Unlock()
	restore = false
	return nil, mcpHostReadClaimOutput{Grants: grants}, nil
}

func validateDirectHostGrantName(name string) error {
	if name == "." {
		return nil
	}
	if name == "" || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("granted path %q must be a direct child name without separators", name)
	}
	return nil
}

type mcpCopyFromHostInput struct {
	GrantID              string `json:"grant_id" jsonschema:"read grant ID returned by vm_host_read_claim"`
	VMID                 string `json:"vm_id" jsonschema:"destination VM ID returned by vm_create"`
	DestinationPath      string `json:"destination_path" jsonschema:"destination path inside the guest"`
	DestinationDirectory bool   `json:"destination_directory,omitempty" jsonschema:"treat destination_path as an existing directory"`
	User                 string `json:"user,omitempty" jsonschema:"guest user used to write the destination; built-in BSD guests currently support only root; defaults to 1000:1000 otherwise"`
}

type mcpCopyFromHostOutput struct {
	Copied bool `json:"copied"`
}

func (e *mcpEndpoint) copyHostPathToVM(ctx context.Context, _ *mcp.CallToolRequest, in mcpCopyFromHostInput) (*mcp.CallToolResult, mcpCopyFromHostOutput, error) {
	reservation, err := e.beginArtifactOperationContext(ctx, 0)
	if err != nil {
		return nil, mcpCopyFromHostOutput{}, err
	}
	defer reservation.release()
	vm, err := e.ownedVM(in.VMID)
	if err != nil {
		return nil, mcpCopyFromHostOutput{}, err
	}
	if in.DestinationPath == "" {
		return nil, mcpCopyFromHostOutput{}, fmt.Errorf("destination_path is required")
	}
	if len(in.DestinationPath) > mcpMaxPathBytes {
		return nil, mcpCopyFromHostOutput{}, fmt.Errorf("destination_path exceeds the %d-byte limit", mcpMaxPathBytes)
	}
	user, err := mcpGuestUser(vm, in.User)
	if err != nil {
		return nil, mcpCopyFromHostOutput{}, err
	}
	grant, err := e.hostReadGrant(in.GrantID)
	if err != nil {
		return nil, mcpCopyFromHostOutput{}, err
	}
	current, err := os.Lstat(grant.Path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(grant.info, current) {
		if err == nil {
			err = fmt.Errorf("granted path changed after the grant was issued")
		}
		return nil, mcpCopyFromHostOutput{}, fmt.Errorf("reopen granted path %q: %w", grant.Path, err)
	}
	if err := validateMCPHostGrantContents(grant.Path); err != nil {
		return nil, mcpCopyFromHostOutput{}, err
	}
	if err := e.extractHostPathIntoGuest(reservation.ctx, vm.ID, in.DestinationPath, in.DestinationDirectory, user, grant.Path); err != nil {
		return nil, mcpCopyFromHostOutput{}, err
	}
	return nil, mcpCopyFromHostOutput{Copied: true}, nil
}

type mcpHostWriteChallengeInput struct {
	Path string `json:"path" jsonschema:"absolute output filename to create with the returned nonce as its exact contents; no parent component may be a symlink"`
}

type mcpHostWriteChallengeOutput struct {
	ChallengeID string    `json:"challenge_id"`
	Path        string    `json:"path"`
	Nonce       string    `json:"nonce"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (e *mcpEndpoint) createHostWriteChallenge(_ context.Context, _ *mcp.CallToolRequest, in mcpHostWriteChallengeInput) (*mcp.CallToolResult, mcpHostWriteChallengeOutput, error) {
	path, err := lexicalAbsoluteHostOutput(in.Path)
	if err != nil {
		return nil, mcpHostWriteChallengeOutput{}, err
	}
	id, err := randomMCPID("hostwrite")
	if err != nil {
		return nil, mcpHostWriteChallengeOutput{}, err
	}
	nonce, err := randomMCPSecret()
	if err != nil {
		return nil, mcpHostWriteChallengeOutput{}, err
	}
	now := time.Now().UTC()
	challenge := &mcpHostChallenge{ID: id, Kind: "write", Directory: filepath.Dir(path), ResponsePath: path, CreatedAt: now, ExpiresAt: now.Add(mcpHostChallengeLifetime), Nonce: nonce}
	if err := e.storeHostChallenge(challenge); err != nil {
		return nil, mcpHostWriteChallengeOutput{}, err
	}
	return nil, mcpHostWriteChallengeOutput{ChallengeID: id, Path: path, Nonce: nonce, ExpiresAt: challenge.ExpiresAt}, nil
}

type mcpCopyToHostInput struct {
	ChallengeID string `json:"challenge_id" jsonschema:"ID returned by vm_host_write_challenge after creating its exact nonce placeholder"`
	VMID        string `json:"vm_id" jsonschema:"source VM ID returned by vm_create"`
	SourcePath  string `json:"source_path" jsonschema:"regular file path inside the guest"`
	User        string `json:"user,omitempty" jsonschema:"guest user used to read the source; built-in BSD guests currently support only root; defaults to 1000:1000 otherwise"`
}

type mcpCopyToHostOutput struct {
	Copied bool   `json:"copied"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
}

func (e *mcpEndpoint) copyVMFileToHost(ctx context.Context, _ *mcp.CallToolRequest, in mcpCopyToHostInput) (*mcp.CallToolResult, mcpCopyToHostOutput, error) {
	reservation, err := e.beginArtifactOperationContext(ctx, 0)
	if err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	defer reservation.release()
	vm, err := e.ownedVM(in.VMID)
	if err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	if in.SourcePath == "" {
		return nil, mcpCopyToHostOutput{}, fmt.Errorf("source_path is required")
	}
	if len(in.SourcePath) > mcpMaxPathBytes {
		return nil, mcpCopyToHostOutput{}, fmt.Errorf("source_path exceeds the %d-byte limit", mcpMaxPathBytes)
	}
	user, err := mcpGuestUser(vm, in.User)
	if err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	challenge, err := e.takeHostChallenge(in.ChallengeID, "write")
	if err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	restore := true
	defer func() {
		if restore {
			e.restoreHostChallenge(challenge)
		}
	}()
	placeholderInfo, err := validateHostWritePlaceholder(challenge)
	if err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	stage, err := os.CreateTemp(challenge.Directory, "."+filepath.Base(challenge.ResponsePath)+".vmsh-write-")
	if err != nil {
		return nil, mcpCopyToHostOutput{}, fmt.Errorf("create staged host output: %w", err)
	}
	stagePath := stage.Name()
	keepStage := false
	defer func() {
		_ = stage.Close()
		if !keepStage {
			_ = os.Remove(stagePath)
		}
	}()
	bytesWritten, err := e.extractGuestFileToHost(reservation.ctx, vm.ID, in.SourcePath, user, stage)
	if err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	if err := stage.Sync(); err != nil {
		return nil, mcpCopyToHostOutput{}, fmt.Errorf("sync staged host output: %w", err)
	}
	if err := stage.Close(); err != nil {
		return nil, mcpCopyToHostOutput{}, fmt.Errorf("close staged host output: %w", err)
	}
	if err := revalidateHostWritePlaceholder(challenge, placeholderInfo); err != nil {
		return nil, mcpCopyToHostOutput{}, err
	}
	if err := replaceMCPHostFile(stagePath, challenge.ResponsePath); err != nil {
		return nil, mcpCopyToHostOutput{}, fmt.Errorf("publish staged host output while retaining the nonce placeholder: %w", err)
	}
	keepStage = true
	restore = false
	return nil, mcpCopyToHostOutput{Copied: true, Path: challenge.ResponsePath, Bytes: bytesWritten}, nil
}

type mcpHostGrantListInput struct{}
type mcpHostGrantListOutput struct {
	Grants     []mcpHostReadGrant `json:"read_grants"`
	Challenges []mcpHostChallenge `json:"pending_challenges"`
}

func (e *mcpEndpoint) listHostGrants(context.Context, *mcp.CallToolRequest, mcpHostGrantListInput) (*mcp.CallToolResult, mcpHostGrantListOutput, error) {
	now := time.Now()
	e.mu.Lock()
	e.pruneExpiredHostChallengesLocked(now)
	grants := make([]mcpHostReadGrant, 0, len(e.hostReadGrants))
	for _, grant := range e.hostReadGrants {
		grants = append(grants, grant.metadata())
	}
	challenges := make([]mcpHostChallenge, 0, len(e.hostChallenges))
	for _, challenge := range e.hostChallenges {
		challenges = append(challenges, challenge.metadata())
	}
	e.mu.Unlock()
	sort.Slice(grants, func(i, j int) bool { return grants[i].CreatedAt.Before(grants[j].CreatedAt) })
	sort.Slice(challenges, func(i, j int) bool { return challenges[i].CreatedAt.Before(challenges[j].CreatedAt) })
	return nil, mcpHostGrantListOutput{Grants: grants, Challenges: challenges}, nil
}

type mcpHostGrantRevokeInput struct {
	ID string `json:"id" jsonschema:"read grant ID or pending challenge ID owned by this MCP session"`
}
type mcpHostGrantRevokeOutput struct {
	Revoked bool `json:"revoked"`
}

func (e *mcpEndpoint) revokeHostGrant(_ context.Context, _ *mcp.CallToolRequest, in mcpHostGrantRevokeInput) (*mcp.CallToolResult, mcpHostGrantRevokeOutput, error) {
	id := strings.TrimSpace(in.ID)
	e.mu.Lock()
	_, grantOK := e.hostReadGrants[id]
	_, challengeOK := e.hostChallenges[id]
	delete(e.hostReadGrants, id)
	delete(e.hostChallenges, id)
	e.mu.Unlock()
	if !grantOK && !challengeOK {
		return nil, mcpHostGrantRevokeOutput{}, fmt.Errorf("host grant or challenge %q is not owned by this MCP session", id)
	}
	return nil, mcpHostGrantRevokeOutput{Revoked: true}, nil
}

func (e *mcpEndpoint) storeHostChallenge(challenge *mcpHostChallenge) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("MCP endpoint is stopped")
	}
	e.pruneExpiredHostChallengesLocked(time.Now())
	if e.hostChallenges == nil {
		e.hostChallenges = make(map[string]*mcpHostChallenge)
	}
	e.hostChallenges[challenge.ID] = challenge
	return nil
}

func (e *mcpEndpoint) takeHostChallenge(id, kind string) (*mcpHostChallenge, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruneExpiredHostChallengesLocked(time.Now())
	challenge := e.hostChallenges[id]
	if challenge == nil || challenge.Kind != kind {
		return nil, fmt.Errorf("%s host challenge %q is not pending in this MCP session", kind, id)
	}
	delete(e.hostChallenges, id)
	return challenge, nil
}

func (e *mcpEndpoint) restoreHostChallenge(challenge *mcpHostChallenge) {
	if challenge == nil || time.Now().After(challenge.ExpiresAt) {
		return
	}
	e.mu.Lock()
	if !e.closed {
		if e.hostChallenges == nil {
			e.hostChallenges = make(map[string]*mcpHostChallenge)
		}
		e.hostChallenges[challenge.ID] = challenge
	}
	e.mu.Unlock()
}

func (e *mcpEndpoint) pruneExpiredHostChallengesLocked(now time.Time) {
	for id, challenge := range e.hostChallenges {
		if now.After(challenge.ExpiresAt) {
			delete(e.hostChallenges, id)
		}
	}
}

func (e *mcpEndpoint) hostReadGrant(id string) (*mcpHostReadGrant, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	grant := e.hostReadGrants[id]
	e.mu.Unlock()
	if grant == nil {
		return nil, fmt.Errorf("host read grant %q is not owned by this MCP session", id)
	}
	copy := *grant
	return &copy, nil
}

func (g mcpHostReadGrant) metadata() mcpHostReadGrant {
	g.info = nil
	return g
}

func (c mcpHostChallenge) metadata() mcpHostChallenge {
	c.Nonce = ""
	return c
}

func lexicalAbsoluteHostDirectory(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("directory is required")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("directory must be an absolute host path")
	}
	return filepath.Clean(raw), nil
}

func lexicalAbsoluteHostOutput(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("path must be an absolute host filename")
	}
	clean := filepath.Clean(raw)
	if filepath.Base(clean) == "." || filepath.Base(clean) == string(filepath.Separator) {
		return "", fmt.Errorf("path must name a host output file")
	}
	return clean, nil
}

func validateHostPathWithoutSymlinks(path string, directory bool) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("path must be absolute")
	}
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean[len(volume):], string(filepath.Separator))
	current := volume + string(filepath.Separator)
	if remainder == "" {
		current = clean
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path traverses symlink %q", current)
		}
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("%q is not a directory", clean)
	}
	return nil
}

func validateHostWritePlaceholder(challenge *mcpHostChallenge) (os.FileInfo, error) {
	if err := validateHostPathWithoutSymlinks(challenge.Directory, true); err != nil {
		return nil, fmt.Errorf("validate output directory: %w", err)
	}
	info, err := os.Lstat(challenge.ResponsePath)
	if err != nil {
		return nil, fmt.Errorf("open write-challenge placeholder %q: %w", challenge.ResponsePath, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("write-challenge placeholder %q must be a regular file", challenge.ResponsePath)
	}
	opened, err := os.Open(challenge.ResponsePath)
	if err != nil {
		return nil, err
	}
	openedInfo, statErr := opened.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = opened.Close()
		return nil, fmt.Errorf("write-challenge placeholder changed while it was being opened")
	}
	contents, readErr := io.ReadAll(io.LimitReader(opened, int64(len(challenge.Nonce)+1)))
	closeErr := opened.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read write-challenge placeholder: %w", readErr)
	}
	if string(contents) != challenge.Nonce {
		return nil, fmt.Errorf("write-challenge placeholder contents do not match the nonce")
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return info, nil
}

func revalidateHostWritePlaceholder(challenge *mcpHostChallenge, expected os.FileInfo) error {
	current, err := validateHostWritePlaceholder(challenge)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("write-challenge placeholder was replaced while guest data was being copied; staged data was discarded")
	}
	return nil
}

func (e *mcpEndpoint) extractHostPathIntoGuest(ctx context.Context, vmID, destination string, destinationDirectory bool, user, source string) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	inputs := make(chan client.ExecInput, 4)
	archiveDone := make(chan error, 1)
	go func() {
		writer := &mcpExecInputWriter{ctx: streamCtx, inputs: inputs}
		err := writeMCPHostPathTar(writer, source)
		if err == nil {
			err = writer.closeInput()
		}
		close(inputs)
		archiveDone <- err
	}()
	exitSeen := false
	exitCode := 0
	var eventErr string
	var stderr []byte
	err := e.control.ExecStreamInContext(streamCtx, vmID, client.ExecRequest{Kind: "fs_extract", Path: destination, Directory: destinationDirectory, User: user}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stderr":
			stderr = appendArtifactDiagnostic(stderr, event.Data, event.Output)
		case "output":
			if event.Stream == "stderr" {
				stderr = appendArtifactDiagnostic(stderr, event.Data, event.Output)
			}
		case "error":
			eventErr = firstNonEmpty(event.Error, event.Output, "guest extract failed")
		case "exit":
			exitSeen = true
			exitCode = event.ExitCode
		}
		return nil
	})
	cancel()
	archiveErr := <-archiveDone
	if archiveErr != nil {
		return fmt.Errorf("archive granted host path: %w", archiveErr)
	}
	if err != nil && !exitSeen {
		return fmt.Errorf("extract host path in guest: %s", conciseCommandError(err))
	}
	if eventErr != "" {
		return fmt.Errorf("extract host path in guest: %s", eventErr)
	}
	if !exitSeen || exitCode != 0 {
		return artifactExitError("extract host path in guest", exitCode, stderr)
	}
	return nil
}

type mcpExecInputWriter struct {
	ctx    context.Context
	inputs chan<- client.ExecInput
}

func (w *mcpExecInputWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		length := len(p)
		if length > mcpArtifactInputChunk {
			length = mcpArtifactInputChunk
		}
		chunk := append([]byte(nil), p[:length]...)
		select {
		case w.inputs <- client.ExecInput{Kind: "stdin", Data: chunk}:
			written += length
			p = p[length:]
		case <-w.ctx.Done():
			return written, w.ctx.Err()
		}
	}
	return written, nil
}

func (w *mcpExecInputWriter) closeInput() error {
	select {
	case w.inputs <- client.ExecInput{Kind: "stdin_close"}:
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func writeMCPHostPathTar(w io.Writer, source string) error {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source is a symlink")
	}
	tw := tar.NewWriter(w)
	rootName := filepath.Base(source)
	writeErr := writeMCPHostTarEntry(tw, source, rootName, rootInfo)
	if writeErr == nil && rootInfo.IsDir() {
		entries, err := os.ReadDir(source)
		if err != nil {
			writeErr = err
		} else {
			for _, entry := range entries {
				path := filepath.Join(source, entry.Name())
				info, err := os.Lstat(path)
				if err != nil {
					writeErr = err
					break
				}
				if err := writeMCPHostTarEntry(tw, path, filepath.Join(rootName, entry.Name()), info); err != nil {
					writeErr = err
					break
				}
			}
		}
	}
	return errors.Join(writeErr, tw.Close())
}

func validateMCPHostGrantContents(source string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(source, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("granted directory contains symlink %q", path)
		}
		if info.IsDir() {
			return fmt.Errorf("subdirectory %q needs its own host read challenge", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("granted directory contains unsupported file %q", path)
		}
	}
	return nil
}

func writeMCPHostTarEntry(tw *tar.Writer, path, archiveName string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("granted path contains symlink %q", path)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("granted path contains unsupported file %q", path)
	}
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(archiveName)
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("source file %q changed while it was being opened", path)
	}
	_, copyErr := io.CopyN(tw, file, info.Size())
	closeErr := file.Close()
	return errors.Join(copyErr, closeErr)
}

func (e *mcpEndpoint) extractGuestFileToHost(ctx context.Context, vmID, source, user string, stage *os.File) (int64, error) {
	reader, writer := io.Pipe()
	type extractionResult struct {
		bytes int64
		err   error
	}
	extractDone := make(chan extractionResult, 1)
	go func() {
		bytes, err := extractSingleMCPGuestFile(reader, stage)
		_ = reader.CloseWithError(err)
		extractDone <- extractionResult{bytes: bytes, err: err}
	}()
	exitSeen := false
	exitCode := 0
	var eventErr string
	var stderr []byte
	err := e.control.ExecStreamInContext(ctx, vmID, client.ExecRequest{Kind: "fs_archive", Path: source, User: user}, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			if event.Stream == "stderr" {
				stderr = appendArtifactDiagnostic(stderr, event.Data, event.Output)
				return nil
			}
			chunk := event.Data
			if len(chunk) == 0 && event.Output != "" {
				chunk = []byte(event.Output)
			}
			if len(chunk) > 0 {
				if _, err := writer.Write(chunk); err != nil {
					return err
				}
			}
		case "stderr":
			stderr = appendArtifactDiagnostic(stderr, event.Data, event.Output)
		case "error":
			eventErr = firstNonEmpty(event.Error, event.Output, "guest archive failed")
		case "exit":
			exitSeen = true
			exitCode = event.ExitCode
		}
		return nil
	})
	if err != nil && !exitSeen {
		_ = writer.CloseWithError(err)
	} else if eventErr != "" {
		_ = writer.CloseWithError(errors.New(eventErr))
	} else if !exitSeen || exitCode != 0 {
		_ = writer.CloseWithError(artifactExitError("archive guest file", exitCode, stderr))
	} else {
		_ = writer.Close()
	}
	extracted := <-extractDone
	if extracted.err != nil {
		return 0, fmt.Errorf("stage guest file on host: %w", extracted.err)
	}
	if err != nil && !exitSeen {
		return 0, fmt.Errorf("archive guest file: %s", conciseCommandError(err))
	}
	if eventErr != "" {
		return 0, fmt.Errorf("archive guest file: %s", eventErr)
	}
	if !exitSeen || exitCode != 0 {
		return 0, artifactExitError("archive guest file", exitCode, stderr)
	}
	return extracted.bytes, nil
}

func extractSingleMCPGuestFile(r io.Reader, stage *os.File) (int64, error) {
	tr := tar.NewReader(r)
	var fileHeader *tar.Header
	var bytesWritten int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			return 0, fmt.Errorf("guest path is a directory; vm_copy_to_host accepts one regular file")
		case tar.TypeReg, byte(0):
			if fileHeader != nil {
				return 0, fmt.Errorf("guest path archived more than one regular file")
			}
			copyHeader := *header
			fileHeader = &copyHeader
			bytesWritten, err = copyMCPHostTarRegular(stage, header, tr)
			if err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("guest path is not a regular file")
		}
	}
	if fileHeader == nil {
		return 0, fmt.Errorf("guest archive did not contain a regular file")
	}
	if err := stage.Chmod(os.FileMode(fileHeader.Mode).Perm()); err != nil {
		return 0, err
	}
	if !fileHeader.ModTime.IsZero() {
		if err := os.Chtimes(stage.Name(), fileHeader.ModTime, fileHeader.ModTime); err != nil {
			return 0, err
		}
	}
	return bytesWritten, nil
}

func copyMCPHostTarRegular(stage *os.File, header *tar.Header, tr *tar.Reader) (int64, error) {
	logicalSize, err := mcpArchiveLogicalSize(header)
	if err != nil {
		return 0, err
	}
	if err := stage.Truncate(0); err != nil {
		return 0, err
	}
	if _, err := stage.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if header.PAXRecords == nil || header.PAXRecords["VMSH.sparse.size"] == "" {
		written, err := io.CopyN(stage, tr, header.Size)
		return written, err
	}
	values := strings.Split(header.PAXRecords["VMSH.sparse.map"], ",")
	for index := 0; index < len(values); index += 2 {
		offset, parseErr := parseMCPHostSparseInt(values[index])
		if parseErr != nil {
			return 0, parseErr
		}
		length, parseErr := parseMCPHostSparseInt(values[index+1])
		if parseErr != nil {
			return 0, parseErr
		}
		if _, err := stage.Seek(offset, io.SeekStart); err != nil {
			return 0, err
		}
		written, err := io.CopyN(stage, tr, length)
		if err != nil {
			return 0, err
		}
		if written != length {
			return 0, io.ErrUnexpectedEOF
		}
	}
	if err := stage.Truncate(logicalSize); err != nil {
		return 0, err
	}
	return logicalSize, nil
}

func parseMCPHostSparseInt(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid sparse archive extent %q", value)
	}
	return parsed, nil
}
