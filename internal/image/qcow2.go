package image

import (
	"fmt"
	"os"

	"github.com/lima-vm/go-qcow2reader"
	"github.com/lima-vm/go-qcow2reader/convert"
)

// ConvertToRaw converts a qcow2 image to a raw disk of exactly diskSize bytes.
//
// Virtualization.framework only accepts raw images, and will not grow one at
// runtime, so the disk is sized here and the guest's cloud-init growpart
// expands the root partition into the space on first boot.
//
// This is pure Go on purpose: the obvious implementation shells out to
// `qemu-img convert`, which would make qemu a dependency on every employee's
// Mac for one operation we can do ourselves. The convert package skips
// unallocated and zero extents, so the output stays sparse on APFS -- a 40 GiB
// disk occupies a couple of GiB until the guest actually writes.
func ConvertToRaw(src, dst string, diskSize int64, progress func(string)) error {
	if diskSize%512 != 0 {
		return fmt.Errorf("disk size %d is not a multiple of 512 bytes", diskSize)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open qcow2: %w", err)
	}
	defer in.Close()

	img, err := qcow2reader.Open(in)
	if err != nil {
		return fmt.Errorf("parse qcow2 %s: %w", src, err)
	}
	defer img.Close()

	if img.Size() > diskSize {
		return fmt.Errorf("source image is %d bytes, larger than the target disk size %d", img.Size(), diskSize)
	}
	progress(fmt.Sprintf("converting qcow2 (%.1f GiB virtual) to raw", float64(img.Size())/(1<<30)))

	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	// Size the file up front: Convert skips zero ranges rather than writing
	// them, so it never extends the file itself.
	if err := out.Truncate(diskSize); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("size raw disk: %w", err)
	}
	if err := convert.Convert(out, img, convert.Options{}); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("convert to raw: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close raw disk: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename raw disk: %w", err)
	}

	progress(fmt.Sprintf("raw disk written: %s (%.0f GiB, sparse)", dst, float64(diskSize)/(1<<30)))
	return nil
}
