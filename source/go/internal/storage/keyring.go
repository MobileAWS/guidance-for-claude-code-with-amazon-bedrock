package storage

import (
	"encoding/json"
	"errors"
	"runtime"

	"github.com/99designs/keyring"
	"ccwb-go/internal/federation"
)

const serviceName = "claude-code-with-bedrock"

func openKeyring() (keyring.Keyring, error) {
	return keyring.Open(keyring.Config{
		ServiceName: serviceName,
		// macOS Keychain
		KeychainName:             "login",
		KeychainTrustApplication: true,
		// Linux Secret Service
		LibSecretCollectionName: serviceName,
		// Windows Credential Manager
		WinCredPrefix: serviceName,
	})
}

// UnifiedSession holds all session data in a single keychain entry.
// This reduces macOS Keychain permission prompts from 4 to 1.
type UnifiedSession struct {
	Credentials *federation.AWSCredentials `json:"credentials,omitempty"`
	Monitoring  *MonitoringTokenData       `json:"monitoring,omitempty"`
}

// readUnifiedSession reads the combined session from one keychain item.
func readUnifiedSession(kr keyring.Keyring, profile string) (*UnifiedSession, error) {
	item, err := kr.Get(profile + "-session")
	if err != nil {
		return nil, err
	}
	var session UnifiedSession
	if err := json.Unmarshal(item.Data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// writeUnifiedSession writes the combined session to one keychain item.
func writeUnifiedSession(kr keyring.Keyring, profile string, session *UnifiedSession) error {
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	return kr.Set(keyring.Item{
		Key:  profile + "-session",
		Data: data,
	})
}

// ReadFromKeyring reads AWS credentials from the OS keyring.
// Tries unified session first, falls back to legacy separate entry.
func ReadFromKeyring(profile string) (*federation.AWSCredentials, error) {
	kr, err := openKeyring()
	if err != nil {
		return nil, err
	}

	if runtime.GOOS == "windows" {
		return readFromKeyringWindows(kr, profile)
	}

	// Try unified session first (single keychain prompt)
	session, err := readUnifiedSession(kr, profile)
	if err == nil && session.Credentials != nil {
		return session.Credentials, nil
	}

	// Fall back to legacy separate entry (migration path)
	item, err := kr.Get(profile + "-credentials")
	if err != nil {
		return nil, err
	}

	var creds federation.AWSCredentials
	if err := json.Unmarshal(item.Data, &creds); err != nil {
		return nil, err
	}

	// Migrate: save to unified format for next time
	_ = writeUnifiedSession(kr, profile, &UnifiedSession{Credentials: &creds})

	return &creds, nil
}

// SaveToKeyring saves AWS credentials to the OS keyring using the unified session.
func SaveToKeyring(creds *federation.AWSCredentials, profile string) error {
	kr, err := openKeyring()
	if err != nil {
		return err
	}

	if runtime.GOOS == "windows" {
		return saveToKeyringWindows(kr, creds, profile)
	}

	// Read existing unified session to preserve monitoring token
	session, err := readUnifiedSession(kr, profile)
	if err != nil || session == nil {
		session = &UnifiedSession{}
	}
	session.Credentials = creds

	return writeUnifiedSession(kr, profile, session)
}

// ClearKeyring replaces credentials with an expired dummy to maintain keychain permissions.
func ClearKeyring(profile string) error {
	expired := &federation.AWSCredentials{
		Version:         1,
		AccessKeyID:     "EXPIRED",
		SecretAccessKey: "EXPIRED",
		SessionToken:    "EXPIRED",
		Expiration:      "2000-01-01T00:00:00Z",
	}
	return SaveToKeyring(expired, profile)
}

// ReadClientSecret reads an Azure confidential-client secret from the OS keyring.
// Entry name matches what the Python ccwb init wizard writes: "{profile}-client-secret"
// under service "claude-code-with-bedrock". Returns empty string with no error
// when the entry is absent -- the caller decides whether that is fatal based on
// azure_auth_mode.
func ReadClientSecret(profile string) (string, error) {
	kr, err := openKeyring()
	if err != nil {
		return "", err
	}
	item, err := kr.Get(profile + "-client-secret")
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return "", nil
		}
		return "", err
	}
	return string(item.Data), nil
}

// ReadMonitoringTokenFromKeyring reads the monitoring token from keyring.
// Tries unified session first, falls back to legacy separate entry.
func ReadMonitoringTokenFromKeyring(profile string) (*MonitoringTokenData, error) {
	kr, err := openKeyring()
	if err != nil {
		return nil, err
	}

	// Try unified session first
	session, err := readUnifiedSession(kr, profile)
	if err == nil && session.Monitoring != nil {
		return session.Monitoring, nil
	}

	// Fall back to legacy separate entry
	item, err := kr.Get(profile + "-monitoring")
	if err != nil {
		return nil, err
	}

	var data MonitoringTokenData
	if err := json.Unmarshal(item.Data, &data); err != nil {
		return nil, err
	}

	// Migrate: merge into unified session
	if session == nil {
		session = &UnifiedSession{}
	}
	session.Monitoring = &data
	_ = writeUnifiedSession(kr, profile, session)

	return &data, nil
}

// SaveMonitoringTokenToKeyring saves a monitoring token to keyring using the unified session.
func SaveMonitoringTokenToKeyring(data *MonitoringTokenData, profile string) error {
	kr, err := openKeyring()
	if err != nil {
		return err
	}

	// Read existing unified session to preserve credentials
	session, err := readUnifiedSession(kr, profile)
	if err != nil || session == nil {
		session = &UnifiedSession{}
	}
	session.Monitoring = data

	return writeUnifiedSession(kr, profile, session)
}

// MonitoringTokenData represents the monitoring token stored in keyring or file.
type MonitoringTokenData struct {
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
	Email   string `json:"email"`
	Profile string `json:"profile"`
}
