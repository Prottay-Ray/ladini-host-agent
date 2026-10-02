package image

import (
	"fmt"
	"path/filepath"
)

// RuntimeSeed writes the per-VM cidata seed.
//
// It carries three things and nothing else: a synthetic hostname (never the
// Mac's), a fresh instance id so cloud-init re-runs its per-instance modules
// on every boot, and the per-boot vsock handshake token.
//
// The token is the one value that legitimately travels on the seed. It is
// scoped to a single boot of a single VM and grants nothing off-host; the
// structural protection against an unwanted control connection is that the
// host registers no vsock listener at all.
func RuntimeSeed(vmDir, hostname, instanceID, token string) error {
	if token == "" {
		return fmt.Errorf("runtime seed: empty handshake token")
	}
	userData := `#cloud-config
preserve_hostname: false
write_files:
  - path: /etc/ladini/boot-token
    permissions: '0600'
    owner: 'root:root'
    content: ` + token + `
`
	metaData := "instance-id: " + instanceID + "\nlocal-hostname: " + hostname + "\n"

	return WriteSeedISO(filepath.Join(vmDir, "seed.iso"), map[string]string{
		"user-data": userData,
		"meta-data": metaData,
	})
}
