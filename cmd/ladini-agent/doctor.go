package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/Prottay-Ray/ladini-host-agent/internal/config"
	"github.com/Prottay-Ray/ladini-host-agent/internal/vmm"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this host can run VMs, and that the binary is correctly signed",
		RunE:  runDoctor,
	}
}

type check struct {
	name   string
	value  string
	failed bool
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	var checks []check
	add := func(name, value string) { checks = append(checks, check{name, value, false}) }
	fail := func(name, value string) { checks = append(checks, check{name, value, true}) }

	add("macos", macOSVersion())
	add("arch", runtime.GOARCH)
	if runtime.GOARCH != "arm64" {
		fail("arch", runtime.GOARCH+" (Apple Silicon required)")
	}

	if vm, err := mem.VirtualMemory(); err == nil {
		add("host memory", fmt.Sprintf("%.1f GiB total, %.1f GiB available",
			float64(vm.Total)/(1<<30), float64(vm.Available)/(1<<30)))
	}

	paths, err := config.Resolve()
	if err != nil {
		fail("state dir", err.Error())
	} else {
		add("state dir", paths.Root)
		if u, err := disk.Usage(paths.Root); err == nil {
			add("free disk", fmt.Sprintf("%.1f GiB", float64(u.Free)/(1<<30)))
		}
	}

	// The real test of both framework availability and the entitlement: build
	// a minimal configuration and let Virtualization.framework validate it.
	// Without the entitlement this fails, which is exactly what we want
	// `go run` to do.
	if err := probeVirtualization(paths); err != nil {
		fail("virtualization", err.Error())
	} else {
		add("virtualization", "supported, entitlement present")
	}

	add("entitlements", entitlementSummary())

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	anyFailed := false
	for _, c := range checks {
		status := "ok"
		if c.failed {
			status, anyFailed = "FAIL", true
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", status, c.name, c.value)
	}
	w.Flush()

	if anyFailed {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

// probeVirtualization builds and validates a throwaway VM configuration in a
// temporary directory. It creates no disks and starts nothing.
func probeVirtualization(paths *config.Paths) error {
	dir, err := os.MkdirTemp("", "ladini-doctor-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// Build() attaches a root disk, so give it an empty, correctly sized file:
	// Virtualization.framework requires disk images be a multiple of 512 bytes.
	stub := dir + "/root.raw"
	if err := os.WriteFile(stub, nil, 0o600); err != nil {
		return fmt.Errorf("stub disk: %w", err)
	}
	if err := os.Truncate(stub, 1<<20); err != nil {
		return fmt.Errorf("size stub disk: %w", err)
	}

	spec := &vmm.Spec{
		ID:          "doctor",
		CPUCount:    1,
		MemoryBytes: 1 << 30,
		Dir:         dir,
		Network:     vmm.NetworkNone,
	}
	if _, err := vmm.Build(spec); err != nil {
		return err
	}
	return nil
}

// entitlementSummary reports what this binary is actually signed with, so a
// stale or unsigned build is visible rather than mysterious.
func entitlementSummary() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown: " + err.Error()
	}
	out, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", exe).CombinedOutput()
	if err != nil {
		return "unsigned or unreadable (build with `make build`)"
	}
	s := string(out)
	var found []string
	for _, key := range []string{
		"com.apple.security.virtualization",
		"com.apple.vm.networking",
		"com.apple.security.hypervisor",
		"com.apple.security.app-sandbox",
	} {
		if strings.Contains(s, key) {
			found = append(found, key)
		}
	}
	if len(found) == 0 {
		return "none (build with `make build`)"
	}
	return strings.Join(found, ", ")
}

func macOSVersion() string {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
