package trusted

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const ProtocolVersion = 1

type RiskClass string

const (
	RiskDelegated     RiskClass = "delegated"
	RiskWorkspaceCode RiskClass = "workspace-code"
	RiskTargetUser    RiskClass = "target-user"
)

type Profile struct {
	Version          int               `json:"version"`
	ID               string            `json:"id"`
	Risk             RiskClass         `json:"risk"`
	TargetID         string            `json:"target_id"`
	HandshakeTimeout string            `json:"handshake_timeout"`
	Roots            map[string]Root   `json:"roots"`
	DefaultRootID    string            `json:"default_root_id"`
	Actions          map[string]Action `json:"actions"`
	Digest           string            `json:"digest"`
}

type Root struct {
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
}

type Action struct {
	Executable        string            `json:"executable"`
	ExecutableDigest  string            `json:"executable_digest,omitempty"`
	RootIDs           []string          `json:"root_ids"`
	ArgumentRules     []ArgumentRule    `json:"argument_rules,omitempty"`
	AllowTrailingArgs bool              `json:"allow_trailing_args,omitempty"`
	Environment       map[string]string `json:"environment,omitempty"`
	OverridableEnv    []string          `json:"overridable_env,omitempty"`
	MaxRequestBytes   int               `json:"max_request_bytes"`
	MaxDuration       string            `json:"max_duration"`
	AllowStdin        bool              `json:"allow_stdin,omitempty"`
	AllowNetwork      bool              `json:"allow_network,omitempty"`
	AllowCredentials  bool              `json:"allow_credentials,omitempty"`
	AllowTerminal     bool              `json:"allow_terminal,omitempty"`
	AllowDetach       bool              `json:"allow_detach,omitempty"`
}

type ArgumentRule struct {
	Position int    `json:"position"`
	Pattern  string `json:"pattern"`
}

type Grant struct {
	ID                   string    `json:"id"`
	SourceVMID           string    `json:"source_vm_id"`
	SourceGeneration     uint64    `json:"source_generation"`
	TargetID             string    `json:"target_id"`
	ProfileID            string    `json:"profile_id"`
	ProfileDigest        string    `json:"profile_digest"`
	RevocationGeneration uint64    `json:"revocation_generation"`
	Revoked              bool      `json:"revoked"`
	CreatedAt            time.Time `json:"created_at"`
}

type Request struct {
	Version          int               `json:"version"`
	CallID           string            `json:"call_id"`
	Sequence         uint64            `json:"sequence"`
	SourceVMID       string            `json:"source_vm_id"`
	SourceGeneration uint64            `json:"source_generation"`
	TargetID         string            `json:"target_id"`
	ProfileDigest    string            `json:"profile_digest"`
	ActionID         string            `json:"action_id"`
	Arguments        []string          `json:"arguments"`
	RootID           string            `json:"root_id"`
	RelativeCWD      string            `json:"relative_cwd"`
	Environment      map[string]string `json:"environment,omitempty"`
	Stdin            bool              `json:"stdin,omitempty"`
	Terminal         bool              `json:"terminal,omitempty"`
	Detach           bool              `json:"detach,omitempty"`
	Deadline         time.Time         `json:"deadline,omitempty"`
}

type Decision struct {
	ProfileDigest string
	ActionID      string
	Executable    string
	Arguments     []string
	CWD           string
	Environment   []string
	Deadline      time.Time
}

type DenialReason string

const (
	DeniedNoGrant          DenialReason = "no_grant"
	DeniedRevoked          DenialReason = "revoked"
	DeniedSource           DenialReason = "source_mismatch"
	DeniedTarget           DenialReason = "target_mismatch"
	DeniedProfile          DenialReason = "profile_mismatch"
	DeniedAction           DenialReason = "action_denied"
	DeniedArguments        DenialReason = "arguments_denied"
	DeniedEnvironment      DenialReason = "environment_denied"
	DeniedWorkingDirectory DenialReason = "working_directory_denied"
	DeniedExecutable       DenialReason = "executable_denied"
	DeniedPrivilege        DenialReason = "privilege_denied"
	DeniedDeadline         DenialReason = "deadline_denied"
	DeniedMalformedRequest DenialReason = "malformed_request"
	DeniedReplay           DenialReason = "replay"
)

type PolicyError struct {
	Reason DenialReason `json:"reason"`
	Detail string       `json:"detail,omitempty"`
}

func (e *PolicyError) Error() string {
	return "trusted call denied: " + string(e.Reason) + ": " + e.Detail
}

func FinalizeProfile(profile *Profile) error {
	if profile == nil {
		return policyError(DeniedProfile, "profile is required")
	}
	profile.Digest = ""
	if err := validateProfile(*profile); err != nil {
		return err
	}
	payload, err := canonicalProfile(*profile)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(payload)
	profile.Digest = hex.EncodeToString(digest[:])
	return nil
}

func VerifyProfile(profile Profile) error {
	digest := profile.Digest
	if len(digest) != sha256.Size*2 {
		return policyError(DeniedProfile, "profile digest is invalid")
	}
	profile.Digest = ""
	if err := validateProfile(profile); err != nil {
		return err
	}
	payload, err := canonicalProfile(profile)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(payload)
	if !strings.EqualFold(digest, hex.EncodeToString(actual[:])) {
		return policyError(DeniedProfile, "profile changed after it was granted")
	}
	return nil
}

func Evaluate(profile Profile, grant *Grant, request Request, now time.Time) (Decision, error) {
	if grant == nil {
		return Decision{}, policyError(DeniedNoGrant, "no grant is attached to this gateway")
	}
	if err := VerifyProfile(profile); err != nil {
		return Decision{}, err
	}
	if grant.Revoked {
		return Decision{}, policyError(DeniedRevoked, "grant is revoked")
	}
	if request.Version != ProtocolVersion || request.CallID == "" || request.Sequence == 0 {
		return Decision{}, policyError(DeniedMalformedRequest, "version, call_id, and sequence are required")
	}
	if request.SourceVMID != grant.SourceVMID || request.SourceGeneration != grant.SourceGeneration {
		return Decision{}, policyError(DeniedSource, "source VM identity or generation does not match")
	}
	if request.TargetID != grant.TargetID || request.TargetID != profile.TargetID {
		return Decision{}, policyError(DeniedTarget, "target does not match the grant")
	}
	if grant.ProfileID != profile.ID || request.ProfileDigest != profile.Digest || grant.ProfileDigest != profile.Digest {
		return Decision{}, policyError(DeniedProfile, "profile ID or digest does not match the grant")
	}
	action, ok := profile.Actions[request.ActionID]
	if !ok {
		return Decision{}, policyError(DeniedAction, "action is not present in the profile")
	}
	if request.Stdin && !action.AllowStdin || request.Terminal && !action.AllowTerminal || request.Detach && !action.AllowDetach {
		return Decision{}, policyError(DeniedPrivilege, "request asks for an ungranted stream, terminal, or detached job")
	}
	requestBytes, _ := json.Marshal(request)
	if action.MaxRequestBytes <= 0 || len(requestBytes) > action.MaxRequestBytes {
		return Decision{}, policyError(DeniedMalformedRequest, "request exceeds the action metadata limit")
	}
	if err := validateArguments(action, request.Arguments); err != nil {
		return Decision{}, err
	}
	root, ok := profile.Roots[request.RootID]
	if !ok || !contains(action.RootIDs, request.RootID) {
		return Decision{}, policyError(DeniedWorkingDirectory, "working directory root is not granted to the action")
	}
	cwd, err := resolveBeneath(root.Path, request.RelativeCWD)
	if err != nil {
		return Decision{}, err
	}
	executable, err := resolveExecutable(action)
	if err != nil {
		return Decision{}, err
	}
	environment := make([]string, 0, len(action.Environment)+len(request.Environment))
	for key, value := range action.Environment {
		environment = append(environment, key+"="+value)
	}
	for key, value := range request.Environment {
		if !contains(action.OverridableEnv, key) || strings.ContainsRune(key, '=') || strings.ContainsRune(value, 0) {
			return Decision{}, policyError(DeniedEnvironment, fmt.Sprintf("environment key %q is not source-overridable", key))
		}
		environment = append(environment, key+"="+value)
	}
	sort.Strings(environment)
	maximum, err := time.ParseDuration(action.MaxDuration)
	if err != nil || maximum <= 0 {
		return Decision{}, policyError(DeniedDeadline, "action does not have a valid evidence-based maximum duration")
	}
	deadline := request.Deadline
	if deadline.IsZero() || !deadline.After(now) || deadline.After(now.Add(maximum)) {
		return Decision{}, policyError(DeniedDeadline, "deadline is absent, expired, or exceeds the action maximum")
	}
	return Decision{ProfileDigest: profile.Digest, ActionID: request.ActionID, Executable: executable, Arguments: append([]string(nil), request.Arguments...), CWD: cwd, Environment: environment, Deadline: deadline}, nil
}

func RequestDigest(request Request) (string, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func validateProfile(profile Profile) error {
	if profile.Version != 1 || profile.ID == "" || profile.TargetID == "" {
		return policyError(DeniedProfile, "version, id, and target_id are required")
	}
	if timeout, err := time.ParseDuration(profile.HandshakeTimeout); err != nil || timeout <= 0 {
		return policyError(DeniedProfile, "handshake_timeout must be a measured positive duration")
	}
	if profile.Risk != RiskDelegated && profile.Risk != RiskWorkspaceCode && profile.Risk != RiskTargetUser {
		return policyError(DeniedProfile, "risk class is invalid")
	}
	if len(profile.Roots) == 0 || len(profile.Actions) == 0 {
		return policyError(DeniedProfile, "at least one root and action are required")
	}
	if _, ok := profile.Roots[profile.DefaultRootID]; !ok {
		return policyError(DeniedProfile, "default_root_id must name a profile root")
	}
	for id, root := range profile.Roots {
		if id == "" || !filepath.IsAbs(root.Path) {
			return policyError(DeniedProfile, "root IDs must be non-empty and paths absolute")
		}
	}
	for id, action := range profile.Actions {
		if id == "" || !filepath.IsAbs(action.Executable) || len(action.RootIDs) == 0 {
			return policyError(DeniedProfile, "actions need an ID, absolute executable, and cwd roots")
		}
		if profile.Risk == RiskDelegated && (action.AllowNetwork || action.AllowCredentials || action.AllowTerminal || action.AllowDetach) {
			return policyError(DeniedProfile, "delegated actions cannot grant network, credentials, terminal, or detached jobs")
		}
		if profile.Risk == RiskDelegated && (action.ExecutableDigest == "" || action.AllowTrailingArgs) {
			return policyError(DeniedProfile, "delegated actions require a fixed executable digest and complete argument rules")
		}
		for _, rootID := range action.RootIDs {
			if _, ok := profile.Roots[rootID]; !ok {
				return policyError(DeniedProfile, "action references an unknown root")
			}
		}
		for _, rule := range action.ArgumentRules {
			if rule.Position < 0 {
				return policyError(DeniedProfile, "argument positions cannot be negative")
			}
			if _, err := regexp.Compile(rule.Pattern); err != nil {
				return policyError(DeniedProfile, "argument rule is not a valid regular expression")
			}
		}
		for key, value := range action.Environment {
			if key == "" || strings.ContainsRune(key, '=') || strings.ContainsRune(key, 0) || strings.ContainsRune(value, 0) {
				return policyError(DeniedProfile, "target environment contains an invalid key or value")
			}
		}
		for _, key := range action.OverridableEnv {
			if key == "" || strings.ContainsRune(key, '=') || strings.ContainsRune(key, 0) {
				return policyError(DeniedProfile, "source-overridable environment key is invalid")
			}
		}
	}
	return nil
}

func validateArguments(action Action, arguments []string) error {
	for _, argument := range arguments {
		if strings.ContainsRune(argument, 0) {
			return policyError(DeniedArguments, "arguments cannot contain NUL")
		}
	}
	matched := make(map[int]bool, len(action.ArgumentRules))
	for _, rule := range action.ArgumentRules {
		if rule.Position >= len(arguments) {
			return policyError(DeniedArguments, "a required argument is absent")
		}
		pattern, _ := regexp.Compile("^(?:" + rule.Pattern + ")$")
		if !pattern.MatchString(arguments[rule.Position]) {
			return policyError(DeniedArguments, fmt.Sprintf("argument %d does not match its rule", rule.Position))
		}
		matched[rule.Position] = true
	}
	if !action.AllowTrailingArgs && len(matched) != len(arguments) {
		return policyError(DeniedArguments, "unmatched trailing arguments are not permitted")
	}
	return nil
}

func resolveBeneath(rootPath, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", policyError(DeniedWorkingDirectory, "working directory must be relative to a granted root")
	}
	root, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", policyError(DeniedWorkingDirectory, err.Error())
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(root, filepath.Clean(relative)))
	if err != nil {
		return "", policyError(DeniedWorkingDirectory, err.Error())
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || runtime.GOOS == "windows" && !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(candidate)) {
		return "", policyError(DeniedWorkingDirectory, "working directory escapes its granted root")
	}
	return candidate, nil
}

func resolveExecutable(action Action) (string, error) {
	resolved, err := filepath.EvalSymlinks(action.Executable)
	if err != nil {
		return "", policyError(DeniedExecutable, err.Error())
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || !fileIsExecutable(resolved, info) {
		return "", policyError(DeniedExecutable, "executable is not a runnable regular file")
	}
	if action.ExecutableDigest != "" {
		contents, err := os.ReadFile(resolved)
		if err != nil {
			return "", policyError(DeniedExecutable, err.Error())
		}
		digest := sha256.Sum256(contents)
		if !strings.EqualFold(action.ExecutableDigest, hex.EncodeToString(digest[:])) {
			return "", policyError(DeniedExecutable, "executable digest changed")
		}
	}
	return resolved, nil
}

func canonicalProfile(profile Profile) ([]byte, error) {
	profile.Digest = ""
	return json.Marshal(profile)
}

func policyError(reason DenialReason, detail string) error {
	return &PolicyError{Reason: reason, Detail: detail}
}

func IsDenial(err error, reason DenialReason) bool {
	var policyErr *PolicyError
	return errors.As(err, &policyErr) && policyErr.Reason == reason
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
