// Package config resolves the agent's on-disk layout.
//
// MVP: everything lives under the invoking user's Application Support
// directory. At M6 this moves to /var/db/ladini owned by a dedicated
// non-admin service account; keep all path construction in here so that
// migration is a single-file change.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Paths is the resolved on-disk layout. Every directory it names is created
// with 0700 and every file written beneath it with 0600, except the golden
// image, which is sealed 0444 once built.
type Paths struct {
	Root   string // ~/Library/Application Support/Ladini
	Images string // <Root>/images
	Cache  string // <Root>/images/cache
	VMs    string // <Root>/vms
	StateD string // <Root>/state.db
}

// Resolve returns the layout, creating any missing directories.
func Resolve() (*Paths, error) {
	base, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	root := filepath.Join(base, "Library", "Application Support", "Ladini")
	p := &Paths{
		Root:   root,
		Images: filepath.Join(root, "images"),
		Cache:  filepath.Join(root, "images", "cache"),
		VMs:    filepath.Join(root, "vms"),
		StateD: filepath.Join(root, "state.db"),
	}
	for _, d := range []string{p.Root, p.Images, p.Cache, p.VMs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	return p, nil
}

// VMDir is the per-VM bundle directory: disk, nvram, machine identifier,
// seed ISO, console log and the VMM child's control socket.
func (p *Paths) VMDir(id string) string { return filepath.Join(p.VMs, id) }
