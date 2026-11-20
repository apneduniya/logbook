//go:build !windows

package claudecode

// registryPolicies has nothing to read outside Windows.
func registryPolicies() []managedDoc { return nil }
