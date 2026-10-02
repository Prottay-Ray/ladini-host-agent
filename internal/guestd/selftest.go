//go:build linux

package guestd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// isolationScript is the baked-in test matrix. The path is fixed at image
// build time; guestd does not accept a script from the caller, so this
// endpoint runs exactly one known program and is not a command channel.
const isolationScript = "/usr/local/lib/ladini/isolation-check.sh"

// probeImage is used for the container-context run. It needs python3, which
// is how the AF_VSOCK reachability check is performed -- and that check is
// the single most important one in the whole matrix, because it is the one
// place the container boundary, not the VM boundary, is load-bearing.
const probeImage = "python:3-alpine"

// SelfTest runs the isolation matrix inside the guest and returns its output.
//
// ctx selects the vantage point:
//
//	"guest"     as guest root, where most of the VM-boundary checks live
//	"container" inside an unprivileged container, where the container-
//	            boundary checks (docker socket, AF_VSOCK, seccomp) live
//
// Several checks legitimately give different answers in the two contexts,
// which is why both are run.
func (r *Runner) SelfTest(parent context.Context, vantage string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	switch vantage {
	case "", "guest":
		cmd = exec.CommandContext(ctx, "sh", isolationScript)
		cmd.Env = []string{"CTX=guest", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

	case "container":
		// The script is piped in on stdin rather than bind-mounted: the
		// container gets no mount from the guest filesystem at all, which
		// keeps this diagnostic consistent with what it is testing.
		cmd = exec.CommandContext(ctx, runtimeBin, "run", "--rm", "--interactive",
			"--name", "ladini-selftest",
			"--env", "CTX=container",
			"--security-opt", "no-new-privileges",
			"--pids-limit", "256",
			probeImage, "sh", "-s")
		cmd.Stdin = bytes.NewReader(mustReadScript())

	default:
		return "", fmt.Errorf("unknown vantage point %q (want guest or container)", vantage)
	}

	out, err := cmd.CombinedOutput()
	// A non-zero exit means checks failed, which is a result rather than an
	// error: the caller wants the report either way.
	if err != nil && len(out) == 0 {
		return "", fmt.Errorf("run self test (%s): %w", vantage, err)
	}
	return string(out), nil
}
