package vmm

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/Prottay-Ray/ladini-host-agent/internal/guestapi"
)

// Runner owns one running virtual machine.
type Runner struct {
	spec *Spec
	vm   *vz.VirtualMachine
}

// Start realises the spec and boots the VM. It returns as soon as the VM is
// running; use WaitReady to wait for the guest agent.
func Start(spec *Spec) (*Runner, error) {
	cfg, err := Build(spec)
	if err != nil {
		return nil, err
	}
	vm, err := vz.NewVirtualMachine(cfg)
	if err != nil {
		return nil, fmt.Errorf("create virtual machine: %w", err)
	}
	if err := vm.Start(); err != nil {
		return nil, fmt.Errorf("start virtual machine: %w", err)
	}
	return &Runner{spec: spec, vm: vm}, nil
}

// State reports the framework's view of the VM.
func (r *Runner) State() vz.VirtualMachineState { return r.vm.State() }

// Dial opens a control connection to the guest agent.
//
// The host dials; it never listens. See the note on VsockPort.
func (r *Runner) Dial(ctx context.Context) (net.Conn, error) {
	devices := r.vm.SocketDevices()
	if len(devices) == 0 {
		return nil, fmt.Errorf("vm has no vsock device")
	}
	conn, err := devices[0].Connect(VsockPort)
	if err != nil {
		return nil, fmt.Errorf("dial guest vsock port %d: %w", VsockPort, err)
	}
	return conn, nil
}

// Client returns a control client for the guest agent.
func (r *Runner) Client(token string) *guestapi.Client {
	return guestapi.NewClient(r.Dial, token)
}

// WaitReady blocks until the guest agent answers a health check with a
// working container runtime, or the context expires.
//
// This -- not "the VM is running", and not "the VM has an IP" -- is the
// readiness signal. Virtualization.framework reports Running the instant the
// vCPUs start, which is a long way from a guest that can accept work.
func (r *Runner) WaitReady(ctx context.Context, token string, progress func(string)) (*guestapi.GuestHealth, error) {
	client := r.Client(token)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastErr error
	announced := false
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("guest did not become ready: %w", lastErr)
			}
			return nil, fmt.Errorf("guest did not become ready: %w", ctx.Err())
		case <-ticker.C:
			if state := r.vm.State(); state == vz.VirtualMachineStateError || state == vz.VirtualMachineStateStopped {
				return nil, fmt.Errorf("vm entered state %s before the guest became ready (see console.log)", state)
			}
			healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			health, err := client.Health(healthCtx)
			cancel()
			if err != nil {
				lastErr = err
				continue
			}
			if !announced {
				progress("guest agent reachable over vsock")
				announced = true
			}
			if !health.DockerOK {
				lastErr = fmt.Errorf("container runtime not ready in guest")
				continue
			}
			return health, nil
		}
	}
}

// WaitStopped blocks until the VM powers itself off. The golden image bake
// relies on this: cloud-init ends with `power_state: poweroff`, so a clean
// shutdown is how we know provisioning finished.
func (r *Runner) WaitStopped(ctx context.Context) error {
	if r.vm.State() == vz.VirtualMachineStateStopped {
		return nil
	}
	states := r.vm.StateChangedNotify()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("vm did not stop: %w", ctx.Err())
		case state := <-states:
			switch state {
			case vz.VirtualMachineStateStopped:
				return nil
			case vz.VirtualMachineStateError:
				return fmt.Errorf("vm entered the error state (see console.log)")
			}
		}
	}
}

// Stop shuts the VM down, asking politely before insisting.
func (r *Runner) Stop(ctx context.Context) error {
	if r.vm.State() == vz.VirtualMachineStateStopped {
		return nil
	}
	if r.vm.CanRequestStop() {
		if _, err := r.vm.RequestStop(); err == nil {
			graceful, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := r.WaitStopped(graceful); err == nil {
				return nil
			}
		}
	}
	if r.vm.CanStop() {
		if err := r.vm.Stop(); err != nil {
			return fmt.Errorf("force stop: %w", err)
		}
		return r.WaitStopped(ctx)
	}
	return fmt.Errorf("vm cannot be stopped from state %s", r.vm.State())
}
