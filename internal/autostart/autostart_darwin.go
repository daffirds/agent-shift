package autostart

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/daffirds/agent-shift/internal/app"
)

const label = "com.github.daffirds.agent-shift"

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func install(exe, pathEnv string) (string, error) {
	home, _ := os.UserHomeDir()
	user := os.Getenv("USER")
	logFile := filepath.Join(app.LogsDir(), "launchd.log")
	if err := os.MkdirAll(app.LogsDir(), 0o700); err != nil {
		return "", err
	}
	env := map[string]string{"PATH": pathEnv, "HOME": home, "USER": user, "LANG": "en_US.UTF-8"}
	if h := os.Getenv(app.HomeEnv); h != "" {
		env[app.HomeEnv] = h
	}
	var envXML bytes.Buffer
	for k, v := range env {
		if v != "" {
			fmt.Fprintf(&envXML, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(k), xmlEscape(v))
		}
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
		<string>--service</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
%s	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, label, xmlEscape(exe), envXML.String(), xmlEscape(logFile), xmlEscape(logFile))
	path := plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "bootout", domain()+"/"+label).Run()
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return "", err
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain(), path).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl bootstrap: %v: %s", err, bytes.TrimSpace(out))
	}
	return path, nil
}

func uninstall() error {
	_ = exec.Command("launchctl", "bootout", domain()+"/"+label).Run()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func get() Status {
	if _, err := os.Stat(plistPath()); err != nil {
		return Status{}
	}
	return Status{Installed: true, Detail: plistPath()}
}

func managed() bool { return get().Installed }

func startManaged() error {
	out, err := exec.Command("launchctl", "kickstart", domain()+"/"+label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// stopManaged lets the caller stop the daemon through its control API; with
// KeepAlive.SuccessfulExit=false launchd won't restart a clean exit.
func stopManaged() error { return nil }
