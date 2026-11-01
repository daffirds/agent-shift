//go:build !darwin && !windows

package autostart

import "errors"

var errUnsupported = errors.New("autostart is only supported on macOS and Windows")

func install(string, string) (string, error) { return "", errUnsupported }
func uninstall() error                       { return errUnsupported }
func get() Status                            { return Status{} }
func managed() bool                          { return false }
func startManaged() error                    { return errUnsupported }
func stopManaged() error                     { return nil }
