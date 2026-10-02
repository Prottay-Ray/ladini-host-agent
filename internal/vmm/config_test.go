package vmm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenFragments are the Virtualization.framework classes that would each
// punch a hole in the isolation contract. They are assembled from fragments at
// runtime so that this test file does not itself contain the literals it
// searches for -- otherwise it would always match itself.
//
// The normative list, with the reason for each, is docs/ISOLATION.md.
func forbiddenFragments() map[string]string {
	vz := "VZ"
	return map[string]string{
		vz + "VirtioFileSystemDevice":      "virtiofs host directory access",
		vz + "DirectorySharingDevice":      "host directory sharing",
		vz + "SharedDirectory":             "host directory sharing",
		vz + "SingleDirectoryShare":        "host directory sharing",
		vz + "MultipleDirectoryShare":      "host directory sharing",
		vz + "LinuxRosettaDirectoryShare":  "Rosetta is a host directory share and translation path",
		vz + "SpiceAgentPortAttachment":    "host<->guest clipboard",
		vz + "USBControllerConfiguration":  "USB passthrough surface",
		vz + "XHCIControllerConfiguration": "USB passthrough surface",
		vz + "USBMassStorageDevice":        "hot-plugged mass storage",
		vz + "VirtioSoundDevice":           "microphone access",
		vz + "HostAudioInputStreamSource":  "microphone access",
		vz + "VirtioGraphicsDevice":        "framebuffer; with spice, a clipboard path",
		vz + "MacGraphicsDevice":           "framebuffer",
		vz + "BridgedNetworkDevice":        "puts the guest directly on the host LAN",
		vz + "NetworkBlockDeviceStorage":   "guest storage reaching over the network",
		vz + "DiskBlockDeviceStorage":      "raw host block device passthrough",
	}
}

// TestOmitList is the mechanical enforcement of the isolation contract: an
// omitted device is invisible in the code, so the only way to keep it omitted
// is to fail the build when its name appears.
//
// This test runs under plain `go test` -- it needs no entitlement and no VM.
func TestOmitList(t *testing.T) {
	root := moduleRoot(t)
	forbidden := forbiddenFragments()

	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, src []byte) {
			if strings.HasSuffix(path, "config_test.go") {
				return // this file names them on purpose
			}
			for name, why := range forbidden {
				if strings.Contains(string(src), name) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s references %s, which would grant: %s\nSee docs/ISOLATION.md before adding it.", rel, name, why)
				}
			}
		})
	}
}

// TestHostRegistersNoVsockListener guards the structural fix for the one hole
// the VM boundary does not close on its own: AF_VSOCK is not isolated by
// network namespaces, so a host-side vsock listener would be reachable from
// any unprivileged container inside the guest. The host must only ever dial.
func TestHostRegistersNoVsockListener(t *testing.T) {
	root := moduleRoot(t)
	needle := "SetSocket" + "Listener"
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, src []byte) {
			if strings.HasSuffix(path, "config_test.go") {
				return
			}
			if strings.Contains(string(src), needle) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s registers a host-side vsock listener; a container in the guest could reach it. The host must dial out instead.", rel)
			}
		})
	}
}

// TestNeverShellsOutToHostDocker catches an easy and dangerous mistake: the
// developer's default docker context is Docker Desktop on the host. All
// container work must happen inside the guest, via ladini-guestd.
func TestNeverShellsOutToHostDocker(t *testing.T) {
	root := moduleRoot(t)
	for _, dir := range []string{"internal", "cmd/ladini-agent"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, src []byte) {
			if strings.HasSuffix(path, "_test.go") {
				return
			}
			s := string(src)
			for _, needle := range []string{`"docker"`, "/var/run/docker.sock"} {
				if strings.Contains(s, needle) {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s references %s; the host agent must never touch the host Docker. Container work belongs in cmd/ladini-guestd.", rel, needle)
				}
			}
		})
	}
}

func walkGoFiles(t *testing.T, dir string, fn func(path string, src []byte)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			walkGoFiles(t, p, fn)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		fn(p, src)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate module root")
		}
		dir = parent
	}
}
