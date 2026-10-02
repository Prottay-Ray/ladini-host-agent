package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/config"
	"github.com/Prottay-Ray/ladini-host-agent/internal/image"
	"github.com/Prottay-Ray/ladini-host-agent/internal/vmm"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// defaultVMID keeps the MVP to a single VM per node. Multi-VM is N children
// of the supervisor and needs no change to the VMM.
const defaultVMID = "default"

func newVMCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "vm", Short: "Manage the node's Linux VM"}
	cmd.AddCommand(newVMUpCmd(), newVMDownCmd(), newVMStatusCmd(), newVMConsoleCmd())
	return cmd
}

func newVMUpCmd() *cobra.Command {
	var (
		id        string
		cpus      uint
		memoryGiB float64
		unsafeNAT bool
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create and boot the VM",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVMUp(cmd.Context(), id, cpus, memoryGiB, unsafeNAT)
		},
	}
	cmd.Flags().StringVar(&id, "id", defaultVMID, "VM identifier")
	cmd.Flags().UintVar(&cpus, "cpus", 2, "virtual CPUs")
	cmd.Flags().Float64Var(&memoryGiB, "memory", 4, "memory in GiB")
	cmd.Flags().BoolVar(&unsafeNAT, "dev-unsafe-nat", false,
		"give the guest NAT networking (REQUIRED for image pulls, but see docs/ISOLATION.md)")
	return cmd
}

func runVMUp(ctx context.Context, id string, cpus uint, memoryGiB float64, unsafeNAT bool) error {
	paths, err := config.Resolve()
	if err != nil {
		return err
	}

	m := image.Ubuntu2404ARM64
	golden := filepath.Join(paths.Images, m.GoldenName())
	if _, err := os.Stat(golden); err != nil {
		return fmt.Errorf("no golden image: run `ladini-agent image build` first")
	}
	// The base disk for every VM on this node is worth re-checking.
	if err := image.Verify(golden); err != nil {
		return err
	}

	dir := paths.VMDir(id)
	if rec, err := vmm.LoadRecord(dir); err == nil && rec.Alive() {
		return fmt.Errorf("vm %q is already running (pid %d); use `vm down` first", id, rec.PID)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clear vm directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create vm directory: %w", err)
	}

	network := vmm.NetworkNone
	if unsafeNAT {
		network = vmm.NetworkNAT
		printNATWarning()
	} else {
		fmt.Println("note: starting with no network device. The guest cannot pull images.")
		fmt.Println("      pass --dev-unsafe-nat for bring-up networking.")
	}

	spec := &vmm.Spec{
		ID:          id,
		CPUCount:    cpus,
		MemoryBytes: uint64(memoryGiB * (1 << 30)),
		Dir:         dir,
		Network:     network,
	}

	fmt.Printf("cloning golden image\n")
	if err := image.Clone(golden, spec.RootDisk()); err != nil {
		return err
	}

	token, err := vmm.NewToken()
	if err != nil {
		return err
	}
	if err := image.RuntimeSeed(dir, "ladini-"+shortID(id), uuid.NewString(), token); err != nil {
		return err
	}

	bootCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	rec, err := vmm.Spawn(bootCtx, spec, token, func(msg string) { fmt.Printf("  %s\n", msg) })
	if err != nil {
		return err
	}

	fmt.Printf("\nvm %q is ready\n", rec.ID)
	fmt.Printf("  address  %s\n", rec.IPv4)
	fmt.Printf("  cpus     %d\n", rec.CPUCount)
	fmt.Printf("  memory   %.0f GiB\n", rec.MemoryGiB)
	// The clone shares its blocks with the golden image, so report that
	// rather than an allocation figure that looks like real consumption.
	fmt.Printf("  disk     copy-on-write clone of the golden image (0 bytes until written)\n")
	return nil
}

func printNATWarning() {
	fmt.Println("")
	fmt.Println("  !! --dev-unsafe-nat: the guest is on the host's vmnet network.")
	fmt.Println("  !! It can reach services bound to 0.0.0.0 on this Mac, and your LAN.")
	fmt.Println("  !! Bring-up only. Do not run untrusted workloads. See docs/ISOLATION.md.")
	fmt.Println("")
}

func newVMDownCmd() *cobra.Command {
	var id string
	var keep bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the VM and destroy its disk",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			dir := paths.VMDir(id)
			rec, err := vmm.LoadRecord(dir)
			if err != nil {
				return fmt.Errorf("no such vm %q", id)
			}
			if rec.Alive() {
				fmt.Printf("stopping vm %q (pid %d)\n", id, rec.PID)
				if err := rec.Shutdown(cmd.Context()); err != nil {
					return err
				}
			}
			if keep {
				fmt.Println("vm stopped; disk kept")
				return nil
			}
			// Ephemerality is the cross-tenant control: the clone and
			// everything written into it goes away with the VM.
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("destroy vm directory: %w", err)
			}
			fmt.Println("vm stopped and destroyed")
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", defaultVMID, "VM identifier")
	cmd.Flags().BoolVar(&keep, "keep-disk", false, "keep the VM directory for inspection")
	return cmd
}

func newVMStatusCmd() *cobra.Command {
	var id string
	var ipOnly bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the VM's state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			dir := paths.VMDir(id)
			rec, err := vmm.LoadRecord(dir)
			if err != nil {
				return fmt.Errorf("vm %q is not running", id)
			}
			if !rec.Alive() {
				return fmt.Errorf("vm %q is not running (stale record, pid %d)", id, rec.PID)
			}
			if ipOnly {
				fmt.Println(rec.IPv4)
				return nil
			}
			health, err := vmm.ConnectLocal(dir).Health(cmd.Context())
			if err != nil {
				return fmt.Errorf("vm %q is running but the guest is unreachable: %w", id, err)
			}
			fmt.Printf("id         %s\n", rec.ID)
			fmt.Printf("state      ready\n")
			fmt.Printf("address    %s\n", health.IPv4)
			fmt.Printf("pid        %d\n", rec.PID)
			fmt.Printf("uptime     %s\n", (time.Duration(health.UptimeSec) * time.Second).String())
			fmt.Printf("guestd     %s\n", health.GuestdVersion)
			fmt.Printf("docker     %s\n", health.DockerVersion)
			fmt.Printf("guest mem  %.1f / %.1f GiB free\n",
				float64(health.MemAvailBytes)/(1<<30), float64(health.MemTotalBytes)/(1<<30))
			fmt.Printf("guest disk %.1f / %.1f GiB free\n",
				float64(health.DiskFree)/(1<<30), float64(health.DiskTotal)/(1<<30))
			fmt.Printf("workloads  %d\n", health.Workloads)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", defaultVMID, "VM identifier")
	cmd.Flags().BoolVar(&ipOnly, "ip", false, "print only the guest's IPv4 address")
	return cmd
}

func newVMConsoleCmd() *cobra.Command {
	var id string
	var lines int
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Show the guest's serial console log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			// The console is guest-to-host only and is untrusted output.
			b, err := os.ReadFile(filepath.Join(paths.VMDir(id), "console.log"))
			if err != nil {
				return fmt.Errorf("no console log for vm %q", id)
			}
			fmt.Print(tailLines(string(b), lines))
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", defaultVMID, "VM identifier")
	cmd.Flags().IntVar(&lines, "lines", 50, "number of trailing lines to show")
	return cmd
}

func newVMMCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "vmm",
		Short:  "Internal: run the VM host process",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
			return vmm.RunChild(log)
		},
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func tailLines(s string, n int) string {
	if n <= 0 {
		return s
	}
	count, idx := 0, len(s)
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			count++
			if count > n {
				idx = i + 1
				break
			}
		}
	}
	return s[idx:]
}
