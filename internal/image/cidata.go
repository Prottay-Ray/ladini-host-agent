package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WriteSeedISO builds a cloud-init NoCloud seed: an ISO labelled "cidata"
// containing the given files.
//
// Implementation note, and a deviation from the original plan. The plan called
// for github.com/kdomanski/iso9660, but that writer supports neither Joliet
// nor Rock Ridge, so "user-data" would be mangled to an ISO9660-legal name
// (d-characters are A-Z 0-9 _ only, so the hyphen cannot survive) and
// cloud-init would not find it. macOS ships hdiutil, which writes a Joliet
// tree that preserves the names exactly. Verified: the ISO volume label is
// CIDATA, the Joliet label is cidata, and both filenames round-trip.
//
// Nothing secret belongs on this ISO. It is attached read-only and its
// contents are readable by anything in the guest; see TestSeedCarriesNoSecrets.
func WriteSeedISO(dst string, files map[string]string) error {
	staging, err := os.MkdirTemp("", "ladini-cidata-")
	if err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	for name, content := range files {
		if strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("seed file %q must be a bare filename", name)
		}
		if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
			return fmt.Errorf("write seed file %s: %w", name, err)
		}
	}

	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing seed: %w", err)
	}
	// -default-volume-name cidata is what makes cloud-init's NoCloud
	// datasource pick this device up.
	out, err := exec.Command("hdiutil", "makehybrid",
		"-iso", "-joliet",
		"-default-volume-name", "cidata",
		"-o", dst, staging).CombinedOutput()
	if err != nil {
		return fmt.Errorf("hdiutil makehybrid: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// hdiutil creates the image 0644 regardless of umask. The enclosing VM
	// directory is 0700 so there is no traversal path to it, but the seed
	// carries the per-boot handshake token and the stated invariant is that
	// every file under the state directory is 0600 -- an invariant is worth
	// nothing if it has a quiet exception.
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("restrict seed permissions: %w", err)
	}

	// Virtualization.framework rejects disk images whose size is not a
	// multiple of 512 bytes. hdiutil emits 2048-byte sectors, so this should
	// never trigger -- but a silent rejection at VM start is a miserable
	// thing to debug, so check here where the error can name the cause.
	fi, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("stat seed: %w", err)
	}
	if fi.Size()%512 != 0 {
		return fmt.Errorf("seed ISO is %d bytes, not a multiple of 512", fi.Size())
	}
	return nil
}
