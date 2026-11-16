// Package secret stores agent-shift's secrets in the OS keychain (macOS Keychain, Windows
// Credential Manager), falling back to a private JSON file when no keychain is available.
// The secrets are Slack user tokens (xoxp-…), one per workspace.
package secret

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"

	"github.com/daffirds/agent-shift/internal/app"
	"github.com/daffirds/agent-shift/internal/config"
)

var ErrNotFound = errors.New("secret not found")

const service = app.Name

// SlackToken names a workspace's user token. The name matches what earlier versions stored.
func SlackToken(teamID string) string { return teamID + "/user_token" }

// legacyAppToken is the Socket Mode app token earlier versions stored; it is no longer used.
func legacyAppToken(teamID string) string { return teamID + "/app_token" }

// DeleteWorkspace removes every secret of a workspace.
func DeleteWorkspace(teamID string) error {
	return errors.Join(Delete(SlackToken(teamID)), Delete(legacyAppToken(teamID)))
}

var mu sync.Mutex

// Get returns a secret.
func Get(name string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	v, err := keyring.Get(service, name)
	if err == nil {
		return v, nil
	}
	if file, ferr := readFile(); ferr == nil {
		if v, ok := file[name]; ok {
			return v, nil
		}
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return "", err
}

// Set stores a secret, preferring the OS keychain.
func Set(name, value string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := keyring.Set(service, name, value); err == nil {
		// Make sure a stale fallback copy doesn't shadow the keychain value later.
		_ = deleteFromFile(name)
		return nil
	}
	file, _ := readFile()
	if file == nil {
		file = map[string]string{}
	}
	file[name] = value
	return writeFile(file)
}

// Delete removes a secret.
func Delete(name string) error {
	mu.Lock()
	defer mu.Unlock()
	var errs []error
	if err := keyring.Delete(service, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, err)
	}
	if err := deleteFromFile(name); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Hint is a safe-to-show fragment of a secret ("xoxp-…3f9c").
func Hint(v string) string {
	if len(v) <= 8 {
		return "…"
	}
	pre := v[:3]
	if i := strings.IndexAny(v, "_-"); i > 0 && i < 6 {
		pre = v[:i+1]
	}
	return pre + "…" + v[len(v)-4:]
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
