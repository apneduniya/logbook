//go:build windows

package claudecode

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// policyKey is where MDM and Group Policy put Claude Code's managed settings
// (a JSON string value named Settings).
const policyKey = `SOFTWARE\Policies\ClaudeCode`

// registryPolicies reads the Settings value under HKLM and HKCU.
func registryPolicies() []managedDoc {
	var out []managedDoc
	for _, root := range []struct {
		key  registry.Key
		name string
	}{{registry.LOCAL_MACHINE, `HKLM\`}, {registry.CURRENT_USER, `HKCU\`}} {
		source := root.name + policyKey + `\Settings`
		k, err := registry.OpenKey(root.key, policyKey, registry.QUERY_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			continue
		}
		if err != nil {
			out = append(out, managedDoc{source: source, err: err})
			continue
		}
		s, _, err := k.GetStringValue("Settings") // REG_SZ or REG_EXPAND_SZ
		_ = k.Close()                             // read-only handle; nothing to flush
		switch {
		case errors.Is(err, registry.ErrNotExist):
		case err != nil:
			out = append(out, managedDoc{source: source, err: err})
		default:
			out = append(out, managedDoc{source: source, data: []byte(s)})
		}
	}
	return out
}
