//go:build linux

package guestd

import "os"

// mustReadScript loads the baked isolation script. If it is missing the
// container-context run produces an explicit failure rather than a silent
// pass, because a self test that cannot run must never look like a clean one.
func mustReadScript() []byte {
	b, err := os.ReadFile(isolationScript)
	if err != nil {
		return []byte("echo 'FAIL  selftest: the isolation script is missing from this image'; exit 1\n")
	}
	return b
}
