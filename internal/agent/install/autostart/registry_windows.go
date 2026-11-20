//go:build windows

package autostart

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows/registry"
)

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// RegistryRunKey is the real HKCU Run key. Writing under HKCU needs no
// admin rights.
type RegistryRunKey struct{}

// Get implements RunKey.
func (RegistryRunKey) Get(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("opening Run key: %w", err)
	}
	defer func() { _ = k.Close() }() // read-only handle; nothing to flush
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading Run value: %w", err)
	}
	return v, true, nil
}

// Set implements RunKey.
func (RegistryRunKey) Set(name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening Run key: %w", err)
	}
	if err := k.SetStringValue(name, value); err != nil {
		_ = k.Close()
		return fmt.Errorf("writing Run value: %w", err)
	}
	if err := k.Close(); err != nil {
		return fmt.Errorf("closing Run key: %w", err)
	}
	return nil
}

// Delete implements RunKey.
func (RegistryRunKey) Delete(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening Run key: %w", err)
	}
	defer func() { _ = k.Close() }() // the delete has already happened or failed
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("deleting Run value: %w", err)
	}
	return nil
}
