package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/config"
	"github.com/Prottay-Ray/ladini-host-agent/internal/guestapi"
	"github.com/Prottay-Ray/ladini-host-agent/internal/vmm"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// guestClient resolves the running VM and returns a client for its guest
// agent, going through the VMM child's unix control socket.
func guestClient(id string) (*guestapi.Client, *vmm.Record, error) {
	paths, err := config.Resolve()
	if err != nil {
		return nil, nil, err
	}
	dir := paths.VMDir(id)
	rec, err := vmm.LoadRecord(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("vm %q is not running: start it with `ladini-agent vm up`", id)
	}
	if !rec.Alive() {
		return nil, nil, fmt.Errorf("vm %q is not running (stale record)", id)
	}
	return vmm.ConnectLocal(dir), rec, nil
}

func newRunCmd() *cobra.Command {
	var (
		vmID     string
		id       string
		img      string
		digest   string
		ports    []string
		envs     []string
		memory   string
		cpuShar  int64
		readOnly bool
		dropCaps bool
	)
	cmd := &cobra.Command{
		Use:   "run [--image IMAGE] [-- COMMAND...]",
		Short: "Run a container workload inside the VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			var command []string
			if img == "" && len(args) > 0 {
				img, args = args[0], args[1:]
			}
			command = args
			if img == "" {
				return fmt.Errorf("an image is required: ladini-agent run --image nginx:alpine")
			}
			client, rec, err := guestClient(vmID)
			if err != nil {
				return err
			}
			if id == "" {
				id = strings.SplitN(uuid.NewString(), "-", 2)[0]
			}

			req := guestapi.RunWorkload{
				ID: id, Image: img, Digest: digest, Command: command,
				CPUShares: cpuShar, ReadOnlyRootfs: readOnly, DropAllCapabilities: dropCaps,
			}
			if req.Ports, err = parsePorts(ports); err != nil {
				return err
			}
			if req.Env, err = parseEnv(envs); err != nil {
				return err
			}
			if memory != "" {
				if req.MemLimitBytes, err = parseSize(memory); err != nil {
					return err
				}
			}

			fmt.Printf("pulling and starting %s in vm %q\n", img, rec.ID)
			w, err := client.Run(cmd.Context(), req)
			if err != nil {
				return err
			}
			fmt.Printf("workload %s is %s\n", w.ID, w.State)
			for _, p := range req.Ports {
				fmt.Printf("  http://%s:%d -> container port %d\n", rec.IPv4, p.Host, p.Container)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&vmID, "vm", defaultVMID, "VM identifier")
	cmd.Flags().StringVar(&id, "id", "", "workload identifier (generated if omitted)")
	cmd.Flags().StringVar(&img, "image", "", "container image")
	cmd.Flags().StringVar(&digest, "digest", "", "pin the image to this sha256 digest")
	cmd.Flags().StringArrayVarP(&ports, "port", "p", nil, "publish a port, host:container")
	cmd.Flags().StringArrayVarP(&envs, "env", "e", nil, "environment variable, KEY=VALUE")
	cmd.Flags().StringVar(&memory, "memory", "", "memory limit, e.g. 512m or 2g")
	cmd.Flags().Int64Var(&cpuShar, "cpu-shares", 0, "relative CPU weight")
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "make the container root filesystem immutable")
	cmd.Flags().BoolVar(&dropCaps, "drop-all-caps", false, "drop all Linux capabilities (breaks images that chown at startup)")
	return cmd
}

func newPSCmd() *cobra.Command {
	var vmID string
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List workloads running in the VM",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := guestClient(vmID)
			if err != nil {
				return err
			}
			workloads, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			if len(workloads) == 0 {
				fmt.Println("no workloads")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tIMAGE\tSTATE\tUPTIME")
			for _, wl := range workloads {
				uptime := "-"
				if !wl.StartedAt.IsZero() && wl.State == guestapi.StateRunning {
					uptime = time.Since(wl.StartedAt).Truncate(time.Second).String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", wl.ID, wl.Image, wl.State, uptime)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&vmID, "vm", defaultVMID, "VM identifier")
	return cmd
}

func newLogsCmd() *cobra.Command {
	var vmID string
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <workload-id>",
		Short: "Show a workload's output",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := guestClient(vmID)
			if err != nil {
				return err
			}
			out, err := client.Logs(cmd.Context(), args[0], tail)
			if err != nil {
				return err
			}
			fmt.Print(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&vmID, "vm", defaultVMID, "VM identifier")
	cmd.Flags().IntVar(&tail, "tail", 200, "number of trailing lines")
	return cmd
}

func newStopCmd() *cobra.Command {
	var vmID string
	var grace int
	cmd := &cobra.Command{
		Use:   "stop <workload-id>",
		Short: "Stop and remove a workload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := guestClient(vmID)
			if err != nil {
				return err
			}
			if err := client.Stop(cmd.Context(), args[0], grace); err != nil {
				return err
			}
			fmt.Printf("workload %s stopped\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&vmID, "vm", defaultVMID, "VM identifier")
	cmd.Flags().IntVar(&grace, "grace", 10, "seconds to wait before killing")
	return cmd
}

func parsePorts(specs []string) ([]guestapi.PortMap, error) {
	var out []guestapi.PortMap
	for _, s := range specs {
		host, container, found := strings.Cut(s, ":")
		if !found {
			return nil, fmt.Errorf("port %q must be host:container", s)
		}
		h, err := strconv.Atoi(host)
		if err != nil {
			return nil, fmt.Errorf("port %q: bad host port: %w", s, err)
		}
		c, err := strconv.Atoi(container)
		if err != nil {
			return nil, fmt.Errorf("port %q: bad container port: %w", s, err)
		}
		out = append(out, guestapi.PortMap{Host: h, Container: c})
	}
	return out, nil
}

func parseEnv(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(specs))
	for _, s := range specs {
		k, v, found := strings.Cut(s, "=")
		if !found || k == "" {
			return nil, fmt.Errorf("environment variable %q must be KEY=VALUE", s)
		}
		out[k] = v
	}
	return out, nil
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "g"), strings.HasSuffix(s, "G"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "M"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "K"):
		mult, s = 1<<10, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size: %w", err)
	}
	return n * mult, nil
}
