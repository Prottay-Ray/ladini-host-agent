package image

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Sidecar records what a golden image actually is, so a node can prove which
// bytes it is running rather than inferring it from a filename.
type Sidecar struct {
	Manifest  Manifest  `json:"manifest"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	BuiltAt   time.Time `json:"built_at"`
}

func sidecarPath(golden string) string {
	return strings.TrimSuffix(golden, ".raw") + ".json"
}

func writeSidecar(golden string, m Manifest) error {
	sum, err := sha256File(golden)
	if err != nil {
		return fmt.Errorf("hash golden image: %w", err)
	}
	fi, err := os.Stat(golden)
	if err != nil {
		return fmt.Errorf("stat golden image: %w", err)
	}
	b, err := json.MarshalIndent(Sidecar{
		Manifest:  m,
		SHA256:    sum,
		SizeBytes: fi.Size(),
		BuiltAt:   time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal sidecar: %w", err)
	}
	return os.WriteFile(sidecarPath(golden), append(b, '\n'), 0o600)
}

// ReadSidecar loads the record for a golden image.
func ReadSidecar(golden string) (*Sidecar, error) {
	b, err := os.ReadFile(sidecarPath(golden))
	if err != nil {
		return nil, err
	}
	var s Sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse sidecar: %w", err)
	}
	return &s, nil
}

// Verify re-hashes a golden image against its sidecar. The agent calls this
// before every boot: the base disk for every VM on the node is worth checking.
func Verify(golden string) error {
	s, err := ReadSidecar(golden)
	if err != nil {
		return fmt.Errorf("read sidecar: %w", err)
	}
	sum, err := sha256File(golden)
	if err != nil {
		return err
	}
	if sum != s.SHA256 {
		return fmt.Errorf("golden image checksum mismatch:\n  want %s\n  got  %s", s.SHA256, sum)
	}
	return nil
}
