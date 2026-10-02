package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/config"
	"github.com/Prottay-Ray/ladini-host-agent/internal/image"
	"github.com/Prottay-Ray/ladini-host-agent/internal/vmm"
	"github.com/spf13/cobra"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Manage the golden guest image"}
	cmd.AddCommand(newImageBuildCmd(), newImageShowCmd())
	return cmd
}

func newImageBuildCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the golden guest image (fetch, convert, bake, seal)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runImageBuild(cmd.Context(), force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "rebuild even if a golden image already exists")
	return cmd
}

func runImageBuild(ctx context.Context, force bool) error {
	paths, err := config.Resolve()
	if err != nil {
		return err
	}
	m := image.Ubuntu2404ARM64
	golden := filepath.Join(paths.Images, m.GoldenName())

	if !force {
		if _, err := os.Stat(golden); err == nil {
			fmt.Printf("golden image already present: %s\n", golden)
			fmt.Println("re-run with --force to rebuild")
			return nil
		}
	}

	guestd := "bin/ladini-guestd"
	if _, err := os.Stat(guestd); err != nil {
		return fmt.Errorf("guest agent not built at %s: run `make guestd` first", guestd)
	}

	work := filepath.Join(paths.Images, ".build")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	defer os.RemoveAll(work)

	step := func(msg string) { fmt.Printf("  %s\n", msg) }

	fmt.Println("building golden image")
	fmt.Printf("  source: %s (%s)\n", m.URL, m.Serial)
	prep, err := image.Prepare(m, paths.Images, paths.Cache, work, guestd, step)
	if err != nil {
		return err
	}

	// The bake boot runs in this process rather than a VMM child: it is a
	// single blocking operation with a clear end condition (the guest powers
	// itself off), so there is nothing to supervise.
	fmt.Println("  booting guest to install Docker and the guest agent (several minutes)")
	spec := &vmm.Spec{
		ID:          "golden-bake",
		CPUCount:    2,
		MemoryBytes: 4 << 30,
		Dir:         work,
		Network:     vmm.NetworkNAT,
	}
	runner, err := vmm.Start(spec)
	if err != nil {
		return fmt.Errorf("boot bake vm: %w", err)
	}

	bakeCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := runner.WaitStopped(bakeCtx); err != nil {
		runner.Stop(context.Background())
		return fmt.Errorf("%w\nconsole log: %s", err, spec.Console())
	}
	step("bake complete, guest powered off")

	if err := image.Seal(prep, m, step); err != nil {
		return err
	}

	fi, err := os.Stat(prep.GoldenPath)
	if err != nil {
		return err
	}
	fmt.Printf("\ngolden image ready\n  %s\n  %.0f GiB virtual, %s on disk\n",
		prep.GoldenPath, float64(fi.Size())/(1<<30), onDiskSize(prep.GoldenPath))
	return nil
}

func newImageShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the golden image record and verify its checksum",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.Resolve()
			if err != nil {
				return err
			}
			m := image.Ubuntu2404ARM64
			golden := filepath.Join(paths.Images, m.GoldenName())
			sc, err := image.ReadSidecar(golden)
			if err != nil {
				return fmt.Errorf("no golden image: run `ladini-agent image build`")
			}
			fmt.Printf("path       %s\n", golden)
			fmt.Printf("source     %s %s %s\n", sc.Manifest.Distro, sc.Manifest.Release, sc.Manifest.Serial)
			fmt.Printf("sha256     %s\n", sc.SHA256)
			fmt.Printf("size       %.0f GiB virtual, %s on disk\n",
				float64(sc.SizeBytes)/(1<<30), onDiskSize(golden))
			fmt.Printf("built      %s\n", sc.BuiltAt.Format(time.RFC3339))
			if err := image.Verify(golden); err != nil {
				fmt.Printf("integrity  FAIL: %v\n", err)
				return err
			}
			fmt.Printf("integrity  verified\n")
			return nil
		},
	}
}

// onDiskSize reports actual allocated blocks, which is what makes the sparse
// file and the APFS clone visible.
func onDiskSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	if st, ok := fi.Sys().(*syscallStat); ok {
		return fmt.Sprintf("%.2f GiB", float64(st.Blocks)*512/(1<<30))
	}
	return "unknown"
}
