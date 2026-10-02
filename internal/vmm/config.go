// Package vmm builds and supervises the Linux VM.
//
// THE SECURITY CONTRACT LIVES IN THIS FILE.
//
//	The VM gets CPU, RAM, a virtual disk and a network interface.
//	Nothing else from the host.
//
// In Virtualization.framework that contract is expressed almost entirely as
// devices we deliberately do NOT attach. The framework defaults every device
// array to empty, so the contract is honoured by never calling the
// corresponding setter. Because an omission is invisible, the normative list
// lives in docs/ISOLATION.md and is enforced mechanically by TestOmitList in
// config_test.go, which greps the whole module for the forbidden class names.
//
// Before adding any device here, read docs/ISOLATION.md.
package vmm

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/Code-Hex/vz/v3"
)

// NetworkMode selects the guest's network attachment.
type NetworkMode string

const (
	// NetworkNAT uses vmnet shared mode. The guest gets 192.168.64.x and
	// outbound internet, which it needs for `docker pull`.
	//
	// WARNING: NAT does NOT satisfy the isolation contract. The guest can
	// reach host services bound to 0.0.0.0, the host's LAN, link-local
	// addresses, and other VMs on the same vmnet segment. It is gated behind
	// --dev-unsafe-nat and is for bring-up only. See docs/ISOLATION.md and
	// the M5 milestone (isolated userspace netstack).
	NetworkNAT NetworkMode = "nat"

	// NetworkNone attaches no network device at all.
	NetworkNone NetworkMode = "none"
)

// VsockPort is the guest-side port ladini-guestd listens on.
//
// The host NEVER registers a vsock listener of its own: AF_VSOCK is not
// isolated by network namespaces, so a host-side listener would be reachable
// by any unprivileged container inside the guest. The host dials out to this
// port instead. See docs/ISOLATION.md §control-channel.
const VsockPort uint32 = 1024

// Spec is everything needed to realise one VM. It is passed to the VMM child
// process as JSON on fd 3 -- never on argv, which is world-readable via ps.
type Spec struct {
	ID          string      `json:"id"`
	CPUCount    uint        `json:"cpu_count"`
	MemoryBytes uint64      `json:"memory_bytes"`
	Dir         string      `json:"dir"`
	Network     NetworkMode `json:"network"`
	MACAddress  string      `json:"mac_address"`
}

// Bundle paths within the VM directory.
func (s *Spec) RootDisk() string  { return filepath.Join(s.Dir, "root.raw") }
func (s *Spec) SeedISO() string   { return filepath.Join(s.Dir, "seed.iso") }
func (s *Spec) NVRAM() string     { return filepath.Join(s.Dir, "nvram") }
func (s *Spec) MachineID() string { return filepath.Join(s.Dir, "machine-id.bin") }
func (s *Spec) Console() string   { return filepath.Join(s.Dir, "console.log") }

// Validate checks the spec against the framework's own limits before we try
// to realise it, so misconfiguration fails with a readable error.
func (s *Spec) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("spec: empty id")
	}
	if s.Dir == "" {
		return fmt.Errorf("spec: empty dir")
	}
	if min, max := uint(1), vz.VirtualMachineConfigurationMaximumAllowedCPUCount(); s.CPUCount < min || s.CPUCount > max {
		return fmt.Errorf("spec: cpu_count %d outside allowed range %d..%d", s.CPUCount, min, max)
	}
	min := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()
	max := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	if s.MemoryBytes < min || s.MemoryBytes > max {
		return fmt.Errorf("spec: memory_bytes %d outside allowed range %d..%d", s.MemoryBytes, min, max)
	}
	return nil
}

// Build realises the Spec as a validated VZVirtualMachineConfiguration.
//
// Every device attached below is listed in docs/ISOLATION.md as permitted.
// Everything absent is absent on purpose.
func Build(s *Spec) (*vz.VirtualMachineConfiguration, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	// --- Boot: EFI, so the guest boots its own GRUB and kernel.
	// VZLinuxBootLoader would force us to re-extract kernel+initrd from the
	// image on every guest kernel upgrade; EFI makes `apt upgrade` just work.
	nvram, err := efiVariableStore(s.NVRAM())
	if err != nil {
		return nil, err
	}
	boot, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(nvram))
	if err != nil {
		return nil, fmt.Errorf("efi bootloader: %w", err)
	}

	cfg, err := vz.NewVirtualMachineConfiguration(boot, s.CPUCount, s.MemoryBytes)
	if err != nil {
		return nil, fmt.Errorf("vm configuration: %w", err)
	}

	// --- Platform: a stable machine identifier, so the guest sees consistent
	// hardware across restarts (and does not see the Mac's real identity).
	machineID, err := machineIdentifier(s.MachineID())
	if err != nil {
		return nil, err
	}
	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(machineID))
	if err != nil {
		return nil, fmt.Errorf("platform configuration: %w", err)
	}
	cfg.SetPlatformVirtualMachineConfiguration(platform)

	// --- Storage: the cloned root disk, plus the read-only cidata seed.
	storage, err := storageDevices(s)
	if err != nil {
		return nil, err
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration(storage)

	// --- Network.
	net, err := networkDevices(s)
	if err != nil {
		return nil, err
	}
	if len(net) > 0 {
		cfg.SetNetworkDevicesVirtualMachineConfiguration(net)
	}

	// --- Entropy: seeds the guest CSPRNG. Host -> guest only.
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("entropy device: %w", err)
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	// --- Memory balloon: host-initiated reclaim. No guest -> host data path.
	balloon, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("memory balloon: %w", err)
	}
	cfg.SetMemoryBalloonDevicesVirtualMachineConfiguration([]vz.MemoryBalloonDeviceConfiguration{balloon})

	// --- Control channel: the guest listens, the host dials out. See VsockPort.
	vsock, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("vsock device: %w", err)
	}
	cfg.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{vsock})

	// --- Serial console: WRITE-ONLY, guest -> host.
	// The read side is /dev/null so the host can never inject bytes into the
	// guest console, and the guest image disables getty on hvc0 so it is not
	// an inbound login path either. Treat console.log as untrusted input.
	serial, err := serialPort(s.Console())
	if err != nil {
		return nil, err
	}
	cfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serial})

	// NOT CONFIGURED, each one deliberately. See docs/ISOLATION.md:
	//   directory sharing / virtiofs ... host filesystem access
	//   Rosetta share ................ host binary translation, and is a dir share
	//   Spice agent port ............. host<->guest clipboard
	//   USB controllers .............. passthrough and hot-plug surface
	//   audio devices ................ microphone access
	//   graphics / keyboard / pointer  headless; graphics+spice is a clipboard path
	//   bridged networking ........... puts the guest on the corporate LAN
	//   network block device ......... guest storage reaching over the network
	//   raw block device ............. host block device passthrough
	//   NVMe controller .............. no need; virtio-blk suffices

	ok, err := cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("validate configuration: rejected by Virtualization.framework")
	}
	return cfg, nil
}

func storageDevices(s *Spec) ([]vz.StorageDeviceConfiguration, error) {
	rootAttach, err := vz.NewDiskImageStorageDeviceAttachmentWithCacheAndSync(
		s.RootDisk(), false, vz.DiskImageCachingModeAutomatic, vz.DiskImageSynchronizationModeFsync)
	if err != nil {
		return nil, fmt.Errorf("root disk attachment: %w", err)
	}
	root, err := vz.NewVirtioBlockDeviceConfiguration(rootAttach)
	if err != nil {
		return nil, fmt.Errorf("root disk device: %w", err)
	}
	devices := []vz.StorageDeviceConfiguration{root}

	// The cidata seed is attached READ-ONLY and carries no credential: only
	// the hostname, instance-id and the per-boot vsock handshake token.
	if _, err := os.Stat(s.SeedISO()); err == nil {
		seedAttach, err := vz.NewDiskImageStorageDeviceAttachment(s.SeedISO(), true)
		if err != nil {
			return nil, fmt.Errorf("seed attachment: %w", err)
		}
		seed, err := vz.NewVirtioBlockDeviceConfiguration(seedAttach)
		if err != nil {
			return nil, fmt.Errorf("seed device: %w", err)
		}
		devices = append(devices, seed)
	}
	return devices, nil
}

func networkDevices(s *Spec) ([]*vz.VirtioNetworkDeviceConfiguration, error) {
	if s.Network == NetworkNone {
		return nil, nil
	}
	if s.Network != NetworkNAT {
		return nil, fmt.Errorf("unsupported network mode %q", s.Network)
	}
	attach, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, fmt.Errorf("nat attachment: %w", err)
	}
	dev, err := vz.NewVirtioNetworkDeviceConfiguration(attach)
	if err != nil {
		return nil, fmt.Errorf("network device: %w", err)
	}
	// A pinned locally-administered MAC keeps the guest's DHCP lease stable
	// across restarts without exposing the host's own hardware address.
	if s.MACAddress != "" {
		hw, err := net.ParseMAC(s.MACAddress)
		if err != nil {
			return nil, fmt.Errorf("parse mac %q: %w", s.MACAddress, err)
		}
		mac, err := vz.NewMACAddress(hw)
		if err != nil {
			return nil, fmt.Errorf("mac address: %w", err)
		}
		dev.SetMACAddress(mac)
	}
	return []*vz.VirtioNetworkDeviceConfiguration{dev}, nil
}

// serialPort wires the guest console to a log file, write-only.
func serialPort(logPath string) (*vz.VirtioConsoleDeviceSerialPortConfiguration, error) {
	w, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open console log: %w", err)
	}
	r, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("open devnull: %w", err)
	}
	attach, err := vz.NewFileHandleSerialPortAttachment(r, w)
	if err != nil {
		return nil, fmt.Errorf("serial attachment: %w", err)
	}
	port, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(attach)
	if err != nil {
		return nil, fmt.Errorf("serial port: %w", err)
	}
	return port, nil
}

func efiVariableStore(path string) (*vz.EFIVariableStore, error) {
	if _, err := os.Stat(path); err == nil {
		store, err := vz.NewEFIVariableStore(path)
		if err != nil {
			return nil, fmt.Errorf("open nvram: %w", err)
		}
		return store, nil
	}
	store, err := vz.NewEFIVariableStore(path, vz.WithCreatingEFIVariableStore())
	if err != nil {
		return nil, fmt.Errorf("create nvram: %w", err)
	}
	return store, nil
}

func machineIdentifier(path string) (*vz.GenericMachineIdentifier, error) {
	if _, err := os.Stat(path); err == nil {
		id, err := vz.NewGenericMachineIdentifierWithDataPath(path)
		if err != nil {
			return nil, fmt.Errorf("open machine identifier: %w", err)
		}
		return id, nil
	}
	id, err := vz.NewGenericMachineIdentifier()
	if err != nil {
		return nil, fmt.Errorf("create machine identifier: %w", err)
	}
	if err := os.WriteFile(path, id.DataRepresentation(), 0o600); err != nil {
		return nil, fmt.Errorf("persist machine identifier: %w", err)
	}
	return id, nil
}
