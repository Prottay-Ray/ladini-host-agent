package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeedCarriesNoSecrets is a standing guard, not a test of today's code.
// The seed ISO is attached to the VM read-only and anything in the guest can
// read it, so it must never become a convenient place to pass a credential.
// The per-boot vsock handshake token is the one value that legitimately
// travels this way; it is scoped to a single boot and grants nothing off-host.
func TestSeedCarriesNoSecrets(t *testing.T) {
	forbidden := []string{
		"AKIA", "ASIA", // AWS access key ids
		"BEGIN OPENSSH PRIVATE KEY", "BEGIN RSA PRIVATE KEY", "BEGIN EC PRIVATE KEY",
		"ghp_", "github_pat_", // GitHub tokens
		"dkr.ecr",              // registry credentials
		"ladini_control_token", // control-plane token, when that lands
		"ssh-rsa", "ssh-ed25519",
	}

	seed := map[string]string{
		"user-data": GoldenUserData,
		"meta-data": "instance-id: test\nlocal-hostname: ladini-test\n",
	}
	for name, content := range seed {
		for _, needle := range forbidden {
			if strings.Contains(content, needle) {
				t.Errorf("seed file %q contains %q; nothing secret may travel on the cidata ISO", name, needle)
			}
		}
	}
}

func TestWriteSeedISOProducesMountableCidata(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "seed.iso")
	if err := WriteSeedISO(dst, map[string]string{
		"user-data": "#cloud-config\nhostname: ladini-test\n",
		"meta-data": "instance-id: iid-test\n",
	}); err != nil {
		t.Fatalf("WriteSeedISO: %v", err)
	}

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if len(raw)%512 != 0 {
		t.Errorf("seed size %d is not a multiple of 512; Virtualization.framework will reject it", len(raw))
	}
	// cloud-init finds the seed by volume label, and reads the filenames from
	// the Joliet tree, where they are UTF-16BE.
	if !strings.Contains(string(raw), "CIDATA") {
		t.Error("seed is missing the CIDATA volume label; cloud-init will not find it")
	}
	for _, name := range []string{"user-data", "meta-data"} {
		utf16be := make([]byte, 0, len(name)*2)
		for _, r := range name {
			utf16be = append(utf16be, 0, byte(r))
		}
		if !strings.Contains(string(raw), string(utf16be)) {
			t.Errorf("seed is missing a Joliet entry for %q; cloud-init will not read it", name)
		}
	}
}
