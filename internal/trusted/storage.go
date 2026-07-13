package trusted

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func LoadProfile(path string) (Profile, error) {
	contents, err := readOwnerFile(path)
	if err != nil {
		return Profile{}, err
	}
	var profile Profile
	if err := json.Unmarshal(contents, &profile); err != nil {
		return Profile{}, policyError(DeniedProfile, err.Error())
	}
	if err := VerifyProfile(profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func SealProfile(path string) (Profile, error) {
	contents, err := readOwnerFile(path)
	if err != nil {
		return Profile{}, err
	}
	var profile Profile
	if err := json.Unmarshal(contents, &profile); err != nil {
		return Profile{}, policyError(DeniedProfile, err.Error())
	}
	if err := FinalizeProfile(&profile); err != nil {
		return Profile{}, err
	}
	if err := writeOwnerJSON(path, profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func LoadGrant(path string) (Grant, error) {
	contents, err := readOwnerFile(path)
	if err != nil {
		return Grant{}, err
	}
	var grant Grant
	if err := json.Unmarshal(contents, &grant); err != nil {
		return Grant{}, policyError(DeniedNoGrant, err.Error())
	}
	return grant, nil
}

func SaveGrant(path string, grant Grant) error {
	return writeOwnerJSON(path, grant)
}

func writeOwnerJSON(path string, value any) error {
	if !filepath.IsAbs(path) {
		return policyError(DeniedNoGrant, "grant path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".grant-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func readOwnerFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, policyError(DeniedProfile, "security file path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || fileAccessibleByOthers(info) {
		return nil, fmt.Errorf("security file %q must be regular and owner-only", path)
	}
	return os.ReadFile(path)
}
