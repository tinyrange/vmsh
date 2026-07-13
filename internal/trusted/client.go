package trusted

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

type GuestConfig struct {
	Version          int               `json:"version"`
	Address          string            `json:"address"`
	Port             int               `json:"port"`
	Token            string            `json:"token"`
	SourceGeneration uint64            `json:"source_generation"`
	TargetID         string            `json:"target_id"`
	ProfileDigest    string            `json:"profile_digest"`
	DefaultRootID    string            `json:"default_root_id"`
	GuestRoot        string            `json:"guest_root"`
	ActionDeadlines  map[string]string `json:"action_deadlines"`
}

type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("trusted action exited with status %d", e.Code) }

var guestSequence atomic.Uint64

func LoadGuestConfig(path string) (GuestConfig, error) {
	contents, err := readOwnerFile(path)
	if err != nil {
		return GuestConfig{}, err
	}
	var config GuestConfig
	if err := json.Unmarshal(contents, &config); err != nil {
		return GuestConfig{}, err
	}
	if config.Version != 1 || net.ParseIP(config.Address) == nil || config.Port <= 0 || len(config.Token) < 32 || config.TargetID == "" || config.ProfileDigest == "" || config.DefaultRootID == "" || !filepath.IsAbs(config.GuestRoot) {
		return GuestConfig{}, fmt.Errorf("trusted call configuration is incomplete")
	}
	return config, nil
}

func Call(ctx context.Context, config GuestConfig, action string, arguments []string, stdout, stderr io.Writer) error {
	maximum, err := time.ParseDuration(config.ActionDeadlines[action])
	if err != nil || maximum <= 0 {
		return fmt.Errorf("action %q has no fact-based deadline", action)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(config.GuestRoot, cwd)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) > 2 && relative[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("current directory is outside the granted guest workspace")
	}
	now := time.Now()
	sequence := uint64(now.UnixNano())
	for {
		previous := guestSequence.Load()
		if sequence <= previous {
			sequence = previous + 1
		}
		if guestSequence.CompareAndSwap(previous, sequence) {
			break
		}
	}
	callID, err := NewToken()
	if err != nil {
		return err
	}
	request := Request{Version: ProtocolVersion, CallID: callID, Sequence: sequence, TargetID: config.TargetID, ProfileDigest: config.ProfileDigest, ActionID: action, Arguments: arguments, RootID: config.DefaultRootID, RelativeCWD: relative, Deadline: now.Add(maximum)}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(config.Address, fmt.Sprint(config.Port)))
	if err != nil {
		return err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	if err := json.NewEncoder(connection).Encode(Envelope{Token: config.Token, Request: request}); err != nil {
		return err
	}
	decoder := json.NewDecoder(connection)
	accepted := false
	terminal := false
	for {
		var event Event
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF && !terminal {
				return fmt.Errorf("trusted call ended without a terminal event")
			}
			return err
		}
		switch event.Kind {
		case "accepted":
			if accepted || terminal {
				return fmt.Errorf("trusted call returned a duplicate accepted event")
			}
			accepted = true
		case "output":
			if !accepted || terminal {
				return fmt.Errorf("trusted call returned output outside an accepted job")
			}
			writer := stdout
			if event.Stream == "stderr" {
				writer = stderr
			} else if event.Stream != "stdout" {
				return fmt.Errorf("trusted call returned unknown stream %q", event.Stream)
			}
			if _, err := writer.Write(event.Data); err != nil {
				return err
			}
		case "exit":
			if !accepted || terminal || event.ExitCode == nil {
				return fmt.Errorf("trusted call returned an invalid exit event")
			}
			terminal = true
			if *event.ExitCode != 0 {
				return ExitError{Code: *event.ExitCode}
			}
			return nil
		case "error":
			if terminal || event.Error == nil {
				return fmt.Errorf("trusted call returned an invalid error event")
			}
			terminal = true
			return event.Error
		default:
			return fmt.Errorf("trusted call returned unknown event %q", event.Kind)
		}
	}
}
