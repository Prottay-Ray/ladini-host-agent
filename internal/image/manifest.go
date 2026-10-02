// Package image builds and clones the golden guest disk.
//
// The golden image is built once: fetch a pinned Ubuntu cloud image, convert
// qcow2 -> raw in pure Go, grow it, then boot it once with a cloud-init seed
// that installs Docker and the guest agent and hardens the guest. Thereafter
// every VM is an APFS copy-on-write clone of that file, which is instant and
// costs no extra bytes until written.
package image

// Manifest pins exactly which bytes we build from. Never track a moving
// "release" or "daily" symlink: the serial is part of the supply chain.
type Manifest struct {
	Distro   string
	Release  string
	Serial   string
	Arch     string
	URL      string
	SHA256   string
	Size     int64
	DiskSize int64 // the raw disk we grow to
}

// Ubuntu2404ARM64 was verified against cloud-images.ubuntu.com on 2026-10-02.
//
// The checksum is the one published in SHA256SUMS for this serial. Fetch
// verifies against it and refuses to proceed on a mismatch.
var Ubuntu2404ARM64 = Manifest{
	Distro:  "ubuntu",
	Release: "24.04",
	Serial:  "release-20260926",
	Arch:    "arm64",
	URL:     "https://cloud-images.ubuntu.com/releases/noble/release-20260926/ubuntu-24.04-server-cloudimg-arm64.img",
	SHA256:  "1d6bffe64b848468ac97f821d369a4846d983de1800ccf6b5ec8853e85cefc55",
	Size:    620224512,
	// 40 GiB. Virtualization.framework requires the image be a multiple of
	// 512 bytes and never grows it at runtime; cloud-init's growpart expands
	// the root partition into this space on first boot.
	DiskSize: 40 << 30,
}

// GoldenName is the filename of the built golden image for this manifest.
func (m Manifest) GoldenName() string {
	return m.Distro + "-" + m.Release + "-" + m.Arch + "-docker-" + m.Serial + ".raw"
}

// CacheName is the filename of the downloaded upstream qcow2.
func (m Manifest) CacheName() string {
	return m.Distro + "-" + m.Release + "-server-cloudimg-" + m.Arch + ".img"
}
