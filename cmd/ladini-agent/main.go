// Command ladini-agent turns this Mac into a node that can host sandbox
// workloads inside an isolated Linux VM.
//
// BUILD NOTE: this binary links Virtualization.framework, which requires the
// com.apple.security.virtualization entitlement on a *signed* binary. `go run`
// and `go test` execute an unsigned temporary binary and will fail at VM
// creation. Always build with `make build`.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:          "ladini-agent",
		Short:        "Host sandbox workloads in an isolated Linux VM",
		SilenceUsage: true,
	}
	root.AddCommand(newDoctorCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
