// Package guestapi is the wire contract between the host agent and the guest
// agent. Both binaries import it, so the types cannot drift.
//
// The transport is HTTP/1.1 + JSON over a single virtio-vsock connection.
// That is deliberately boring: gRPC would buy schema evolution we do not need
// across a one-hop socket, at the cost of a protoc toolchain in the build.
package guestapi

import "time"

// Port is the guest-side port the guest agent listens on. The host dials out
// to it; the host never listens. See internal/vmm.VsockPort.
const Port uint32 = 1024

// TokenHeader carries the per-boot handshake token. The token is delivered to
// the guest on the read-only cidata seed and grants nothing off-host; it
// exists so that a connection which should be impossible is also unauthorised.
const TokenHeader = "X-Ladini-Token"

// WorkloadState is the lifecycle of one container inside the guest.
type WorkloadState string

const (
	StatePending WorkloadState = "pending"
	StatePulling WorkloadState = "pulling"
	StateRunning WorkloadState = "running"
	StateExited  WorkloadState = "exited"
	StateFailed  WorkloadState = "failed"
)

// PortMap publishes a container port on the guest's network interface.
type PortMap struct {
	Host      int `json:"host"`
	Container int `json:"container"`
}

// RunWorkload asks the guest to pull and start one container.
//
// Note what is NOT here: there is no field for arbitrary docker flags, no
// field for a host path to mount, and no field for a long-lived credential.
// The guest agent builds the docker argv itself from these fields, so a
// compromised control plane cannot ask for --privileged or a host bind mount.
type RunWorkload struct {
	ID            string            `json:"id"`
	Image         string            `json:"image"`
	Digest        string            `json:"digest,omitempty"`
	Command       []string          `json:"command,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Ports         []PortMap         `json:"ports,omitempty"`
	CPUShares     int64             `json:"cpu_shares,omitempty"`
	MemLimitBytes int64             `json:"mem_limit_bytes,omitempty"`
	TimeoutSec    int               `json:"timeout_sec,omitempty"`

	// ReadOnlyRootfs and DropAllCapabilities are extra container hardening,
	// and both are OPT-IN rather than default.
	//
	// That is a deliberate call, not a concession. The security boundary in
	// this design is the VM, not the container: a workload lives inside a
	// disposable guest that holds no host filesystem, no host credentials and
	// no host socket, and that is destroyed when the job ends. Stock images
	// routinely need to write to their own filesystem and to call chown --
	// nginx:alpine does both -- so turning these on by default would break a
	// large share of real workloads to harden a boundary that is not the one
	// doing the work.
	//
	// What is NOT optional is listed in internal/guestd/container.go: no
	// privileged mode, no host namespaces, no device or bind mounts, and the
	// runtime's default seccomp profile, which is what blocks
	// socket(AF_VSOCK) and therefore keeps a container away from the host
	// control channel. None of those have a field here, and the argv is built
	// from typed fields, so there is no way to ask for them.
	ReadOnlyRootfs      bool `json:"read_only_rootfs,omitempty"`
	DropAllCapabilities bool `json:"drop_all_capabilities,omitempty"`
}

// Workload is the guest's view of one container.
type Workload struct {
	ID          string        `json:"id"`
	Image       string        `json:"image"`
	ContainerID string        `json:"container_id,omitempty"`
	State       WorkloadState `json:"state"`
	ExitCode    int           `json:"exit_code,omitempty"`
	OOMKilled   bool          `json:"oom_killed,omitempty"`
	Error       string        `json:"error,omitempty"`
	StartedAt   time.Time     `json:"started_at,omitempty"`
}

// GuestHealth is what the host polls to decide the VM is ready, and later
// what it folds into the node heartbeat.
type GuestHealth struct {
	GuestdVersion string  `json:"guestd_version"`
	UptimeSec     int64   `json:"uptime_sec"`
	LoadAvg1      float64 `json:"load_avg_1"`
	MemTotalBytes uint64  `json:"mem_total_bytes"`
	MemAvailBytes uint64  `json:"mem_avail_bytes"`
	DiskTotal     uint64  `json:"disk_total_bytes"`
	DiskFree      uint64  `json:"disk_free_bytes"`
	DockerVersion string  `json:"docker_version"`
	DockerOK      bool    `json:"docker_ok"`
	IPv4          string  `json:"ipv4"`
	Workloads     int     `json:"workloads"`
}

// Error is the body of any non-2xx response.
type Error struct {
	Message string `json:"message"`
}
