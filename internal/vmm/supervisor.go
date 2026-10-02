package vmm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/guestapi"
)

// ChildRequest is handed to the VMM child on file descriptor 3.
//
// It travels on an inherited fd rather than argv because argv is world
// readable through ps, and this carries the per-boot handshake token.
type ChildRequest struct {
	Spec  Spec   `json:"spec"`
	Token string `json:"token"`
}

// Record is the persisted, redacted view of a running VM. Other CLI
// invocations read it to find the control socket.
//
// It deliberately does not contain the handshake token: only the child that
// owns the VM needs it, and vm.json is an ordinary file.
type Record struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	Dir       string    `json:"dir"`
	Socket    string    `json:"socket"`
	IPv4      string    `json:"ipv4"`
	CPUCount  uint      `json:"cpu_count"`
	MemoryGiB float64   `json:"memory_gib"`
	Network   string    `json:"network"`
	StartedAt time.Time `json:"started_at"`
}

func recordPath(dir string) string { return filepath.Join(dir, "vm.json") }

// ControlSocket is the unix socket the VMM child serves. It proxies the guest
// control API, injecting the handshake token, so that no other process needs
// to hold the token or the VZVirtioSocketDevice.
func ControlSocket(dir string) string { return filepath.Join(dir, "vmm.sock") }

// NewToken mints a per-boot handshake token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Spawn starts the VMM child for spec and waits until the guest is ready.
//
// The child is a re-exec of this same binary, which is what gives us crash
// isolation without a second language or a second signing target: the child
// inherits the entitlement because the entitlement is a property of the
// Mach-O, not of the invocation.
func Spawn(ctx context.Context, spec *Spec, token string, progress func(string)) (*Record, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate own binary: %w", err)
	}

	payload, err := json.Marshal(ChildRequest{Spec: *spec, Token: token})
	if err != nil {
		return nil, fmt.Errorf("marshal child request: %w", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create spec pipe: %w", err)
	}
	defer r.Close()

	logFile, err := os.OpenFile(filepath.Join(spec.Dir, "vmm.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open vmm log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "vmm")
	// A scrubbed environment: the child must not inherit AWS_*, GITHUB_TOKEN,
	// DOCKER_HOST or anything else from the operator's shell.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + spec.Dir}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{r} // becomes fd 3 in the child
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		w.Close()
		return nil, fmt.Errorf("start vmm child: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		w.Close()
		return nil, fmt.Errorf("send spec to child: %w", err)
	}
	w.Close()
	// The child outlives this process; reap nothing, but do not leave a
	// zombie if we exit first -- Setsid already detached it.
	go cmd.Wait()

	progress("vmm child started, waiting for guest")
	health, err := waitForControlSocket(ctx, spec.Dir)
	if err != nil {
		// Surface what the child logged; a bare timeout is useless to debug.
		if tail := tailFile(filepath.Join(spec.Dir, "vmm.log"), 2048); tail != "" {
			return nil, fmt.Errorf("%w\n--- vmm.log ---\n%s", err, tail)
		}
		return nil, err
	}

	rec := &Record{
		ID:        spec.ID,
		PID:       cmd.Process.Pid,
		Dir:       spec.Dir,
		Socket:    ControlSocket(spec.Dir),
		IPv4:      health.IPv4,
		CPUCount:  spec.CPUCount,
		MemoryGiB: float64(spec.MemoryBytes) / (1 << 30),
		Network:   string(spec.Network),
		StartedAt: time.Now().UTC(),
	}
	if err := writeRecord(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func waitForControlSocket(ctx context.Context, dir string) (*guestapi.GuestHealth, error) {
	client := ConnectLocal(dir)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("vm did not become ready: %w", lastErr)
			}
			return nil, fmt.Errorf("vm did not become ready: %w", ctx.Err())
		case <-ticker.C:
			healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			health, err := client.Health(healthCtx)
			cancel()
			if err == nil {
				return health, nil
			}
			lastErr = err
		}
	}
}

// ConnectLocal returns a client for an already-running VM, going through the
// child's unix control socket. The caller needs no token: the child holds it.
func ConnectLocal(dir string) *guestapi.Client {
	sock := ControlSocket(dir)
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}
	return guestapi.NewClient(dial, "")
}

// LoadRecord reads the persisted record for a VM directory.
func LoadRecord(dir string) (*Record, error) {
	b, err := os.ReadFile(recordPath(dir))
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", recordPath(dir), err)
	}
	return &rec, nil
}

func writeRecord(rec *Record) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	return os.WriteFile(recordPath(rec.Dir), append(b, '\n'), 0o600)
}

// Alive reports whether the VMM child for this record is still running.
func (r *Record) Alive() bool {
	if r.PID <= 0 {
		return false
	}
	return syscall.Kill(r.PID, 0) == nil
}

// Shutdown asks the VMM child to stop the VM and exit.
func (r *Record) Shutdown(ctx context.Context) error {
	if !r.Alive() {
		return nil
	}
	if err := syscall.Kill(r.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal vmm child: %w", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !r.Alive() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	// The guest ignored ACPI or the child is wedged. SIGKILL releases the
	// VM's memory; the disk is a clone we are about to delete anyway.
	return syscall.Kill(r.PID, syscall.SIGKILL)
}

func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := fi.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return ""
	}
	return string(buf)
}
