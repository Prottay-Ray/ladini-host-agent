package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newSelfTestCmd() *cobra.Command {
	var vmID string
	var vantage string
	cmd := &cobra.Command{
		Use:   "selftest",
		Short: "Run the isolation matrix inside the running VM",
		Long: "Runs the checks baked into the golden image, from both vantage " +
			"points: as guest root, and inside an unprivileged container. " +
			"Network checks report DEFER rather than PASS while the guest is " +
			"on NAT; see docs/ISOLATION.md.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := guestClient(vmID)
			if err != nil {
				return err
			}
			vantages := []string{"guest", "container"}
			if vantage != "" && vantage != "both" {
				vantages = []string{vantage}
			}

			failed := false
			for _, v := range vantages {
				fmt.Printf("\n========== vantage: %s ==========\n", v)
				out, err := client.SelfTest(cmd.Context(), v)
				if err != nil {
					return err
				}
				fmt.Print(out)
				if strings.Contains(out, "FAIL ") {
					failed = true
				}
			}
			if failed {
				return fmt.Errorf("one or more isolation checks failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&vmID, "vm", defaultVMID, "VM identifier")
	cmd.Flags().StringVar(&vantage, "context", "both", "vantage point: guest, container, or both")
	return cmd
}
