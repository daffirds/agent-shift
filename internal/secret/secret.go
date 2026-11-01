// Package secret stores Slack tokens in the OS keychain (macOS Keychain, Windows Credential
// Manager), falling back to a private JSON file when no keychain is available.
package secret

import (
	"encoding/json"
	"errors"
	"os"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
)

var ErrNotFound = errors.New("secret not found")

const service = app.Name

// Token kinds stored per workspace.
const (
	UserToken = "user_token" // xoxp-… acts as the user
	AppToken  = "app_token"  // xapp-… opens the Socket Mode connection
)

func key(teamID, kind string) string { return teamID + "/" + kind }

var mu sync.Mutex

// Get returns a workspace token.
func Get(teamID, kind string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	v, err := keyring.Get(service, key(teamID, kind))
	if err == nil {
		return v, nil
	}
	if file, ferr := readFile(); ferr == nil {
		if v, ok := file[key(teamID, kind)]; ok {
			return v, nil
		}
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return "", err
}

// Set stores a workspace token, preferring the OS keychain.
func Set(teamID, kind, value string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := keyring.Set(service, key(teamID, kind), value); err == nil {
		// Make sure a stale fallback copy doesn't shadow the keychain value later.
		_ = deleteFromFile(key(teamID, kind))
		return nil
	}
	file, _ := readFile()
	if file == nil {
		file = map[string]string{}
	}
	file[key(teamID, kind)] = value
	return writeFile(file)
}

// Delete removes all tokens of a workspace.
func Delete(teamID string) error {
	mu.Lock()
	defer mu.Unlock()
	var errs []error
	for _, kind := range []string{UserToken, AppToken} {
		if err := keyring.Delete(service, key(teamID, kind)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, err)
		}
		if err := deleteFromFile(key(teamID, kind)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func readFile() (map[string]string, error) {
	data, err := os.ReadFile(app.CredentialsPath())
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	return m, json.Unmarshal(data, &m)
}

func writeFile(m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(app.CredentialsPath(), data, 0o600)
}

func deleteFromFile(k string) error {
	m, err := readFile()
	if err != nil {
		return nil
	}
	if _, ok := m[k]; !ok {
		return nil
	}
	delete(m, k)
	if len(m) == 0 {
		return os.Remove(app.CredentialsPath())
	}
	return writeFile(m)
}
