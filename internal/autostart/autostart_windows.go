package autostart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "agent-shift"
)

func install(exe, _ string) (string, error) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	// `start` spawns the daemon with no console window and returns immediately; logon
	// sessions already carry the user's full PATH.
	command := fmt.Sprintf(`"%s" start --quiet`, exe)
	if err := k.SetStringValue(valueName, command); err != nil {
		return "", err
	}
	return `HKCU\` + runKey + `\` + valueName, nil
}

func uninstall() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if err := k.DeleteValue(valueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func get() Status {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return Status{}
	}
	defer k.Close()
	v, _, err := k.GetStringValue(valueName)
	if err != nil {
		return Status{}
	}
	return Status{Installed: true, Detail: v}
}

func managed() bool       { return false }
func startManaged() error { return errors.New("not supported") }
func stopManaged() error  { return nil }
