package image

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Clone makes a copy-on-write clone of the golden image.
//
// On APFS clonefile(2) is effectively instant and costs no extra blocks until
// the clone is written to, which is what makes "fresh VM per job" cheap. The
// golden image is never opened for writing, so a bug here cannot corrupt the
// base that every future VM derives from.
func Clone(golden, dst string) error {
	if _, err := os.Stat(golden); err != nil {
		return fmt.Errorf("golden image: %w", err)
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing clone: %w", err)
	}
	if err := unix.Clonefile(golden, dst, 0); err != nil {
		return fmt.Errorf("clonefile %s -> %s: %w (is the state directory on APFS?)", golden, dst, err)
	}
	// The clone inherits the golden image's 0444; the VM needs to write to it.
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("chmod clone: %w", err)
	}
	return nil
}
