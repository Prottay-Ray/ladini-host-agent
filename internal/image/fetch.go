package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Fetch downloads the manifest's image into cacheDir if it is not already
// there and verified. It returns the path to the verified qcow2.
//
// A cached file is re-verified rather than trusted: a truncated or tampered
// download must never silently become the base for every VM on the node.
func Fetch(m Manifest, cacheDir string, progress func(string)) (string, error) {
	dst := filepath.Join(cacheDir, m.CacheName())

	if sum, err := sha256File(dst); err == nil {
		if sum == m.SHA256 {
			progress(fmt.Sprintf("cached image verified (%s)", shortSum(sum)))
			return dst, nil
		}
		progress("cached image failed checksum, re-downloading")
		if err := os.Remove(dst); err != nil {
			return "", fmt.Errorf("remove stale cache: %w", err)
		}
	}

	progress(fmt.Sprintf("downloading %s (%.0f MiB)", m.URL, float64(m.Size)/(1<<20)))
	if err := download(m.URL, dst); err != nil {
		return "", err
	}

	sum, err := sha256File(dst)
	if err != nil {
		return "", err
	}
	if sum != m.SHA256 {
		// Leave nothing unverified on disk.
		os.Remove(dst)
		return "", fmt.Errorf("checksum mismatch for %s:\n  want %s\n  got  %s", m.CacheName(), m.SHA256, sum)
	}
	progress(fmt.Sprintf("download verified (%s)", shortSum(sum)))
	return dst, nil
}

func download(url, dst string) error {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("get %s: %s", url, resp.Status)
	}

	// Download to a temporary name and rename on success, so an interrupted
	// run never leaves a partial file that looks like a cached image.
	tmp := dst + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("download body: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	return os.Rename(tmp, dst)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
