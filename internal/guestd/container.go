//go:build linux

// Package guestd is the agent that runs INSIDE the Linux VM. It is the only
// place in this repository that drives a container runtime.
//
// It is excluded from the host's "never touch the host container runtime"
// guard in internal/vmm/config_test.go, because it only ever compiles for
// linux/arm64 and only ever runs inside the guest.
package guestd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/guestapi"
)

const runtimeBin = "docker"

// containerName is prefixed so that a workload can never collide with, or
// address, anything else running in the guest.
func containerName(id string) string { return "ladini-" + id }

// Runner drives the container runtime inside the guest.
type Runner struct{}

// Run pulls and starts one workload.
//
// The argv is built here, field by field, from the request. Nothing the
// caller sends is passed through as a flag. The hardening flags below are
// unconditional: a workload cannot opt out of them.
func (r *Runner) Run(ctx context.Context, req guestapi.RunWorkload) (*guestapi.Workload, error) {
	w := &guestapi.Workload{ID: req.ID, Image: req.Image, State: guestapi.StatePulling}

	ref := req.Image
	if req.Digest != "" {
		// Pinning by digest is both the reproducibility and the supply-chain
		// control: the scheduler decides exactly which bytes run.
		ref = strings.SplitN(req.Image, ":", 2)[0] + "@" + req.Digest
	}
	if out, err := run(ctx, runtimeBin, "pull", ref); err != nil {
		w.State, w.Error = guestapi.StateFailed, fmt.Sprintf("pull %s: %v: %s", ref, err, out)
		return w, fmt.Errorf("%s", w.Error)
	}

	args := []string{
		"run", "--detach",
		"--name", containerName(req.ID),
		// Unconditional hardening.
		//
		// The load-bearing part is what CANNOT be requested: this argv is
		// built from typed fields, so there is no --privileged, no
		// --security-opt (so the default seccomp profile stays on, and with
		// it the block on socket(AF_VSOCK) that keeps a container away from
		// the host control channel), no --cap-add, no --device, no
		// --pid/--ipc/--network/--userns=host, and no bind mount.
		"--security-opt", "no-new-privileges",
		"--pids-limit", "512",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=256m",
	}
	// Opt-in hardening: both of these break common stock images, and the
	// boundary that matters here is the VM. See guestapi.RunWorkload.
	if req.ReadOnlyRootfs {
		args = append(args, "--read-only")
	}
	if req.DropAllCapabilities {
		args = append(args, "--cap-drop=ALL")
	}
	if req.MemLimitBytes > 0 {
		args = append(args, "--memory", strconv.FormatInt(req.MemLimitBytes, 10))
	}
	if req.CPUShares > 0 {
		args = append(args, "--cpu-shares", strconv.FormatInt(req.CPUShares, 10))
	}
	for _, p := range req.Ports {
		args = append(args, "--publish", fmt.Sprintf("%d:%d", p.Host, p.Container))
	}

	// Environment goes in a file, not on argv: argv is visible in the guest's
	// process table to every other container's escape attempt and to any
	// other workload. The file lives on tmpfs and is removed immediately.
	if len(req.Env) > 0 {
		envFile, cleanup, err := writeEnvFile(req.Env)
		if err != nil {
			w.State, w.Error = guestapi.StateFailed, err.Error()
			return w, err
		}
		defer cleanup()
		args = append(args, "--env-file", envFile)
	}
	args = append(args, ref)
	// The command is the workload. Restricting it would buy nothing: the
	// image already is arbitrary code. What must not pass through is runtime
	// *flags*, and none do.
	args = append(args, req.Command...)

	out, err := run(ctx, runtimeBin, args...)
	if err != nil {
		w.State, w.Error = guestapi.StateFailed, fmt.Sprintf("run: %v: %s", err, out)
		return w, fmt.Errorf("%s", w.Error)
	}
	w.ContainerID = strings.TrimSpace(out)
	w.State = guestapi.StateRunning
	w.StartedAt = time.Now().UTC()
	return w, nil
}

// Inspect reports the current state of a workload's container.
func (r *Runner) Inspect(ctx context.Context, id string) (*guestapi.Workload, error) {
	out, err := run(ctx, runtimeBin, "inspect", "--format", "{{json .}}", containerName(id))
	if err != nil {
		return nil, fmt.Errorf("no such workload %q", id)
	}
	var raw struct {
		ID    string `json:"Id"`
		State struct {
			Status    string `json:"Status"`
			ExitCode  int    `json:"ExitCode"`
			OOMKilled bool   `json:"OOMKilled"`
			StartedAt string `json:"StartedAt"`
			Error     string `json:"Error"`
		} `json:"State"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse inspect output: %w", err)
	}

	w := &guestapi.Workload{
		ID:          id,
		Image:       raw.Config.Image,
		ContainerID: raw.ID,
		ExitCode:    raw.State.ExitCode,
		OOMKilled:   raw.State.OOMKilled,
		Error:       raw.State.Error,
	}
	switch raw.State.Status {
	case "running":
		w.State = guestapi.StateRunning
	case "created", "restarting":
		w.State = guestapi.StatePending
	case "exited", "dead", "removing":
		w.State = guestapi.StateExited
	default:
		w.State = guestapi.StateFailed
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.State.StartedAt); err == nil {
		w.StartedAt = t
	}
	return w, nil
}

// List returns every workload this agent is managing.
func (r *Runner) List(ctx context.Context) ([]guestapi.Workload, error) {
	out, err := run(ctx, runtimeBin, "ps", "--all",
		"--filter", "name=^ladini-", "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var workloads []guestapi.Workload
	for _, name := range strings.Fields(out) {
		id := strings.TrimPrefix(name, "ladini-")
		w, err := r.Inspect(ctx, id)
		if err != nil {
			continue
		}
		workloads = append(workloads, *w)
	}
	return workloads, nil
}

// Stop stops and removes a workload's container.
func (r *Runner) Stop(ctx context.Context, id string, graceSec int) error {
	if graceSec <= 0 {
		graceSec = 10
	}
	if out, err := run(ctx, runtimeBin, "stop", "--timeout", strconv.Itoa(graceSec), containerName(id)); err != nil {
		return fmt.Errorf("stop %s: %v: %s", id, err, out)
	}
	if out, err := run(ctx, runtimeBin, "rm", "--force", containerName(id)); err != nil {
		return fmt.Errorf("remove %s: %v: %s", id, err, out)
	}
	return nil
}

// Logs returns the tail of a workload's output.
func (r *Runner) Logs(ctx context.Context, id string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	out, err := run(ctx, runtimeBin, "logs", "--tail", strconv.Itoa(tail), containerName(id))
	if err != nil {
		return "", fmt.Errorf("logs %s: %v: %s", id, err, out)
	}
	return out, nil
}

// Version reports the container runtime version, and whether it is usable.
func (r *Runner) Version(ctx context.Context) (string, bool) {
	out, err := run(ctx, runtimeBin, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func writeEnvFile(env map[string]string) (string, func(), error) {
	f, err := os.CreateTemp("/tmp", "ladini-env-")
	if err != nil {
		return "", nil, fmt.Errorf("create env file: %w", err)
	}
	cleanup := func() { os.Remove(f.Name()) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("chmod env file: %w", err)
	}
	for k, v := range env {
		if strings.ContainsAny(k, "=\n") {
			f.Close()
			cleanup()
			return "", nil, fmt.Errorf("invalid environment variable name %q", k)
		}
		if _, err := fmt.Fprintf(f, "%s=%s\n", k, v); err != nil {
			f.Close()
			cleanup()
			return "", nil, fmt.Errorf("write env file: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close env file: %w", err)
	}
	return f.Name(), cleanup, nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
