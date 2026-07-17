package vmshd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"j5.nz/cc/client"
)

const (
	mcpMaxArtifacts          = 64
	mcpMaxArtifactBytes      = 64 << 20
	mcpMaxArtifactTotalBytes = 256 << 20
	mcpArtifactInputChunk    = 256 << 10
)

type mcpArtifact struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	SourceVM  string    `json:"source_vm,omitempty"`
	Source    string    `json:"source,omitempty"`
	data      []byte
}

type mcpArtifactExportInput struct {
	VMID string `json:"vm_id" jsonschema:"ID returned by vm_create"`
	Path string `json:"path" jsonschema:"file or directory path inside the source guest"`
	Name string `json:"name,omitempty" jsonschema:"optional artifact label"`
	User string `json:"user,omitempty" jsonschema:"guest user used to read the source; defaults to 1000:1000"`
}

type mcpArtifactOutput struct {
	Artifact mcpArtifact `json:"artifact"`
}

func (e *mcpEndpoint) exportArtifact(ctx context.Context, _ *mcp.CallToolRequest, in mcpArtifactExportInput) (*mcp.CallToolResult, mcpArtifactOutput, error) {
	id, err := e.ownedVMID(in.VMID)
	if err != nil {
		return nil, mcpArtifactOutput{}, err
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		return nil, mcpArtifactOutput{}, fmt.Errorf("path is required")
	}
	user := strings.TrimSpace(in.User)
	if user == "" {
		user = "1000:1000"
	}
	data, err := e.archiveGuestPath(ctx, id, path, user)
	if err != nil {
		return nil, mcpArtifactOutput{}, err
	}
	artifact, err := e.storeArtifact(in.Name, id, path, data)
	if err != nil {
		return nil, mcpArtifactOutput{}, err
	}
	return nil, mcpArtifactOutput{Artifact: artifact}, nil
}

type mcpArtifactImportInput struct {
	ArtifactID string `json:"artifact_id" jsonschema:"ID returned by vm_artifact_export"`
	VMID       string `json:"vm_id" jsonschema:"ID returned by vm_create"`
	Path       string `json:"path" jsonschema:"destination path inside the guest"`
	Directory  bool   `json:"directory,omitempty" jsonschema:"treat path as an existing destination directory"`
	User       string `json:"user,omitempty" jsonschema:"guest user used to write the destination; defaults to 1000:1000"`
}

type mcpArtifactImportOutput struct {
	Imported bool  `json:"imported"`
	Bytes    int64 `json:"bytes"`
}

func (e *mcpEndpoint) importArtifact(ctx context.Context, _ *mcp.CallToolRequest, in mcpArtifactImportInput) (*mcp.CallToolResult, mcpArtifactImportOutput, error) {
	id, err := e.ownedVMID(in.VMID)
	if err != nil {
		return nil, mcpArtifactImportOutput{}, err
	}
	path := strings.TrimSpace(in.Path)
	if path == "" {
		return nil, mcpArtifactImportOutput{}, fmt.Errorf("path is required")
	}
	artifact, err := e.artifact(in.ArtifactID)
	if err != nil {
		return nil, mcpArtifactImportOutput{}, err
	}
	user := strings.TrimSpace(in.User)
	if user == "" {
		user = "1000:1000"
	}
	if err := e.extractGuestArchive(ctx, id, path, in.Directory, user, artifact.data); err != nil {
		return nil, mcpArtifactImportOutput{}, err
	}
	return nil, mcpArtifactImportOutput{Imported: true, Bytes: int64(len(artifact.data))}, nil
}

type mcpCopyInput struct {
	SourceVM             string `json:"source_vm" jsonschema:"source VM ID returned by vm_create"`
	SourcePath           string `json:"source_path" jsonschema:"source file or directory inside the source guest"`
	DestinationVM        string `json:"destination_vm" jsonschema:"destination VM ID returned by vm_create"`
	DestinationPath      string `json:"destination_path" jsonschema:"destination path inside the destination guest"`
	DestinationDirectory bool   `json:"destination_directory,omitempty" jsonschema:"treat destination_path as an existing directory"`
	SourceUser           string `json:"source_user,omitempty" jsonschema:"source guest user; defaults to 1000:1000"`
	DestinationUser      string `json:"destination_user,omitempty" jsonschema:"destination guest user; defaults to 1000:1000"`
}

type mcpCopyOutput struct {
	Copied bool  `json:"copied"`
	Bytes  int64 `json:"archive_bytes"`
}

func (e *mcpEndpoint) copyGuestPath(ctx context.Context, _ *mcp.CallToolRequest, in mcpCopyInput) (*mcp.CallToolResult, mcpCopyOutput, error) {
	sourceID, err := e.ownedVMID(in.SourceVM)
	if err != nil {
		return nil, mcpCopyOutput{}, err
	}
	destinationID, err := e.ownedVMID(in.DestinationVM)
	if err != nil {
		return nil, mcpCopyOutput{}, err
	}
	sourcePath := strings.TrimSpace(in.SourcePath)
	destinationPath := strings.TrimSpace(in.DestinationPath)
	if sourcePath == "" || destinationPath == "" {
		return nil, mcpCopyOutput{}, fmt.Errorf("source_path and destination_path are required")
	}
	sourceUser := firstNonEmpty(strings.TrimSpace(in.SourceUser), "1000:1000")
	destinationUser := firstNonEmpty(strings.TrimSpace(in.DestinationUser), "1000:1000")
	data, err := e.archiveGuestPath(ctx, sourceID, sourcePath, sourceUser)
	if err != nil {
		return nil, mcpCopyOutput{}, err
	}
	if err := e.extractGuestArchive(ctx, destinationID, destinationPath, in.DestinationDirectory, destinationUser, data); err != nil {
		return nil, mcpCopyOutput{}, err
	}
	return nil, mcpCopyOutput{Copied: true, Bytes: int64(len(data))}, nil
}

type mcpArtifactListInput struct{}
type mcpArtifactListOutput struct {
	Artifacts []mcpArtifact `json:"artifacts"`
}

func (e *mcpEndpoint) listArtifacts(context.Context, *mcp.CallToolRequest, mcpArtifactListInput) (*mcp.CallToolResult, mcpArtifactListOutput, error) {
	e.mu.Lock()
	artifacts := make([]mcpArtifact, 0, len(e.artifacts))
	for _, artifact := range e.artifacts {
		artifacts = append(artifacts, artifact.metadata())
	}
	e.mu.Unlock()
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].CreatedAt.Before(artifacts[j].CreatedAt) })
	return nil, mcpArtifactListOutput{Artifacts: artifacts}, nil
}

type mcpArtifactDeleteInput struct {
	ArtifactID string `json:"artifact_id" jsonschema:"ID returned by vm_artifact_export"`
}
type mcpArtifactDeleteOutput struct {
	Deleted bool `json:"deleted"`
}

func (e *mcpEndpoint) deleteArtifact(_ context.Context, _ *mcp.CallToolRequest, in mcpArtifactDeleteInput) (*mcp.CallToolResult, mcpArtifactDeleteOutput, error) {
	id := strings.TrimSpace(in.ArtifactID)
	e.mu.Lock()
	_, ok := e.artifacts[id]
	if ok {
		delete(e.artifacts, id)
	}
	e.mu.Unlock()
	if !ok {
		return nil, mcpArtifactDeleteOutput{}, fmt.Errorf("artifact %q is not owned by this MCP session", id)
	}
	return nil, mcpArtifactDeleteOutput{Deleted: true}, nil
}

func (e *mcpEndpoint) archiveGuestPath(ctx context.Context, vmID, path, user string) ([]byte, error) {
	data := make([]byte, 0, 1<<20)
	exitSeen := false
	exitCode := 0
	var eventErr string
	err := e.control.ExecStreamInContext(ctx, vmID, client.ExecRequest{Kind: "fs_archive", Path: path, User: user}, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			chunk := event.Data
			if len(chunk) == 0 && event.Output != "" {
				chunk = []byte(event.Output)
			}
			if len(data)+len(chunk) > mcpMaxArtifactBytes {
				return fmt.Errorf("artifact exceeds the %d MiB MCP limit", mcpMaxArtifactBytes>>20)
			}
			data = append(data, chunk...)
		case "error":
			eventErr = firstNonEmpty(event.Error, event.Output, "guest archive failed")
		case "exit":
			exitSeen = true
			exitCode = event.ExitCode
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("archive guest path: %s", conciseCommandError(err))
	}
	if eventErr != "" {
		return nil, fmt.Errorf("archive guest path: %s", eventErr)
	}
	if !exitSeen || exitCode != 0 {
		return nil, fmt.Errorf("archive guest path exited with status %d", exitCode)
	}
	return data, nil
}

func (e *mcpEndpoint) extractGuestArchive(ctx context.Context, vmID, path string, directory bool, user string, data []byte) error {
	inputs := make(chan client.ExecInput, 4)
	go func() {
		defer close(inputs)
		for offset := 0; offset < len(data); offset += mcpArtifactInputChunk {
			end := offset + mcpArtifactInputChunk
			if end > len(data) {
				end = len(data)
			}
			select {
			case inputs <- client.ExecInput{Kind: "stdin", Data: data[offset:end]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	exitSeen := false
	exitCode := 0
	var eventErr string
	err := e.control.ExecStreamInContext(ctx, vmID, client.ExecRequest{
		Kind: "fs_extract", Path: path, Directory: directory, User: user,
		ArchiveLimits: &client.ArchiveLimits{MaxEntries: 100000, MaxFileBytes: mcpMaxArtifactBytes, MaxExpandedBytes: mcpMaxArtifactBytes * 4},
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "error":
			eventErr = firstNonEmpty(event.Error, event.Output, "guest extract failed")
		case "exit":
			exitSeen = true
			exitCode = event.ExitCode
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("extract guest archive: %s", conciseCommandError(err))
	}
	if eventErr != "" {
		return fmt.Errorf("extract guest archive: %s", eventErr)
	}
	if !exitSeen || exitCode != 0 {
		return fmt.Errorf("extract guest archive exited with status %d", exitCode)
	}
	return nil
}

func (e *mcpEndpoint) storeArtifact(name, sourceVM, source string, data []byte) (mcpArtifact, error) {
	if len(data) > mcpMaxArtifactBytes {
		return mcpArtifact{}, fmt.Errorf("artifact exceeds the %d MiB MCP limit", mcpMaxArtifactBytes>>20)
	}
	id, err := randomMCPID("artifact")
	if err != nil {
		return mcpArtifact{}, err
	}
	digest := sha256.Sum256(data)
	artifact := &mcpArtifact{ID: id, Name: strings.TrimSpace(name), Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), CreatedAt: time.Now().UTC(), SourceVM: sourceVM, Source: source, data: append([]byte(nil), data...)}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return mcpArtifact{}, fmt.Errorf("MCP endpoint is stopped")
	}
	if len(e.artifacts) >= mcpMaxArtifacts {
		return mcpArtifact{}, fmt.Errorf("MCP artifact limit reached (%d)", mcpMaxArtifacts)
	}
	total := len(data)
	for _, existing := range e.artifacts {
		total += len(existing.data)
	}
	if total > mcpMaxArtifactTotalBytes {
		return mcpArtifact{}, fmt.Errorf("MCP artifact storage limit reached (%d MiB)", mcpMaxArtifactTotalBytes>>20)
	}
	e.artifacts[id] = artifact
	return artifact.metadata(), nil
}

func (e *mcpEndpoint) artifact(id string) (*mcpArtifact, error) {
	id = strings.TrimSpace(id)
	e.mu.Lock()
	artifact := e.artifacts[id]
	if artifact != nil {
		copy := *artifact
		copy.data = append([]byte(nil), artifact.data...)
		artifact = &copy
	}
	e.mu.Unlock()
	if artifact == nil {
		return nil, fmt.Errorf("artifact %q is not owned by this MCP session", id)
	}
	return artifact, nil
}

func (a *mcpArtifact) metadata() mcpArtifact {
	copy := *a
	copy.data = nil
	return copy
}
