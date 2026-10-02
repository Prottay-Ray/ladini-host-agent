# The isolation contract

This document is normative. `internal/vmm/config.go` implements it,
`internal/vmm/config_test.go` enforces it on the source tree, and
`scripts/isolation-check.sh` verifies it from inside a running guest.

> The VM gets CPU, RAM, a virtual disk and a network interface.
> Nothing else from the host.

## Why the VM is the boundary, and the agent is not

The node is an employee's laptop. It holds their SSH keys, their login
keychain, corporate registry credentials, source code, and — on the machine
this was developed against — live `kubectl port-forward` tunnels into a
running cluster. If a workload can reach any of that, the product has no
reason to exist.

So the host agent never executes third-party code on macOS. It configures a
VM and talks to an agent inside it. Containers run only in the guest.

## The contract is a list of omissions

In Virtualization.framework, almost every way a guest could reach the host is
an opt-in device. The framework defaults every device array to empty, so the
contract is honoured by *not calling the setter*.

That makes the contract invisible in the code, which is why it is written
here and enforced by a test that greps for the class names.

### Attached, and why each is safe

| Device | Why it is safe |
|---|---|
| `VZEFIBootLoader` + `VZEFIVariableStore` | Boots the guest's own GRUB. The NVRAM store is a host file in the 0700 VM directory. |
| `VZGenericPlatformConfiguration` + persisted machine identifier | Synthetic, stable hardware identity. Not the Mac's. |
| `VZVirtioBlockDeviceConfiguration` (root) | A copy-on-write clone of the golden image. Destroyed with the VM. |
| `VZVirtioBlockDeviceConfiguration` (seed, read-only) | Carries a hostname, an instance id and the per-boot vsock token. Nothing else may travel here; `TestSeedCarriesNoSecrets` enforces that. It stays attached for the life of the VM — Virtualization.framework has no hot-unplug for storage — so check F7 reports its presence as expected and F8/F8b carry the real assertions. Removing the token from the seed entirely, by having the host present it on the first vsock connection, is the post-MVP improvement. |
| `VZVirtioNetworkDeviceConfiguration` | See **The network problem** below. This is the weak one. |
| `VZVirtioEntropyDeviceConfiguration` | Host → guest only. |
| `VZVirtioSocketDeviceConfiguration` | The control channel. See below. |
| `VZVirtioTraditionalMemoryBalloonDeviceConfiguration` | Host-initiated reclaim. No guest → host data path. |
| `VZVirtioConsoleDeviceSerialPortConfiguration` | Write-only: the read fd is `/dev/null`, so the host cannot inject bytes into the guest console, and the guest image masks `getty` on `hvc0`. The resulting log is untrusted guest output. |

### Never attached

| Class | What attaching it would grant |
|---|---|
| `VZVirtioFileSystemDeviceConfiguration`, `VZDirectorySharingDeviceConfiguration`, `VZSharedDirectory`, `VZSingleDirectoryShare`, `VZMultipleDirectoryShare` | virtiofs access to host directories. The single biggest violation. |
| `VZLinuxRosettaDirectoryShare` | Rosetta is delivered *as a directory share* and is a host binary-translation path. Consequence: this node runs **arm64 images only**. |
| `VZSpiceAgentPortAttachment` | The host↔guest clipboard. |
| `VZUSBControllerConfiguration`, `VZXHCIControllerConfiguration`, `VZUSBMassStorageDeviceConfiguration`, `VZUSBDeviceConfiguration` | A USB bus, and with it hot-plug and mass storage. The guest has no USB bus at all. |
| `VZVirtioSoundDeviceConfiguration`, `VZHostAudioInputStreamSource` | Microphone access. |
| `VZVirtioGraphicsDeviceConfiguration`, `VZMacGraphicsDeviceConfiguration`, keyboard and pointing devices | A framebuffer. Headless needs none, and graphics plus Spice is a clipboard path. |
| `VZBridgedNetworkDeviceAttachment` | Puts the guest directly on the operator's LAN with its own MAC. Also needs the restricted `com.apple.vm.networking` entitlement. |
| `VZNetworkBlockDeviceStorageDeviceAttachment` | Guest storage reaching out over the network. |
| `VZDiskBlockDeviceStorageDeviceAttachment` | Raw host block device passthrough. |
| `VZNVMExpressControllerDeviceConfiguration` | No functional need. Fewer devices, fewer CVEs. |

### Obligations that are not VZ configuration

- The VMM child is exec'd with a scrubbed environment (`PATH`, `HOME` only).
- The VM spec travels on **file descriptor 3**, never argv — argv is
  world-readable through `ps`, and the spec carries the handshake token.
- The agent never opens or references the host's container runtime.
  `TestNeverShellsOutToHostDocker` enforces this.
- The guest IP is read from the guest's own health response, never from
  `/var/db/dhcpd_leases`, which other VMs on the host also write to.

## The control channel

`AF_VSOCK` is **not** isolated by network namespaces. An unprivileged
container inside the guest can call `socket(AF_VSOCK, SOCK_STREAM, 0)` and
`connect((VMADDR_CID_HOST=2, port))` with no privilege at all.

So the host registers **no vsock listener**. The guest agent listens; the
host dials out to it. A guest-side connect to the host CID then fails
unconditionally — a structural property, not a filter.

Three further layers sit behind it:

1. The guest agent requires a per-boot token on every request. The token
   arrives on the read-only seed, is read into memory at boot, and the file
   is unlinked. A failed handshake is logged as a warning.
2. The guest agent builds the container argv itself from typed fields. The
   load-bearing property is what **cannot be asked for**: there is no
   `--privileged`, no `--security-opt` (so the runtime's default seccomp
   profile stays on, and with it the block on `socket(AF_VSOCK)`), no
   `--cap-add`, no `--device`, no `--pid`/`--ipc`/`--network`/`--userns=host`,
   and no bind mount. Every container also gets `no-new-privileges`, a pid
   limit and a tmpfs `/tmp`.

   `--cap-drop=ALL` and a read-only root are **opt-in**, not default. That is
   a deliberate call: stock images routinely write to their own filesystem
   and call `chown` — `nginx:alpine` does both — so defaulting them on would
   break a large share of real workloads in order to harden a boundary that
   is not the one doing the work. The container lives inside a disposable VM
   that holds no host filesystem, no host credential and no host socket.
3. Workload environment goes to a tmpfs file passed with `--env-file`, never
   on argv, which is visible in the guest's process table.

## The network problem

**This is the weakest part of the contract, and under `--dev-unsafe-nat` it
is not satisfied.**

`VZNATNetworkDeviceAttachment` puts the guest on the host's vmnet shared
segment. From there it can reach anything the host has bound to `0.0.0.0`,
the operator's LAN, link-local addresses, and other VMs on the same segment.
On the development machine this meant a Kafka Connect REST API on
`0.0.0.0:8083` — an endpoint that accepts connector configurations, which is
effectively code execution against the host's container runtime.

The guest image loads an nftables policy that drops egress to RFC1918,
link-local and CGNAT ranges, allowing only DNS (the resolver under NAT *is*
the vmnet gateway) and public egress. That is **defence in depth, not the
control**: guest root can flush it.

### How the checks are run

There is no SSH in the guest and no shell access by design, so the matrix is
**baked into the golden image** and exposed through one endpoint that runs
exactly that script: `ladini-agent selftest`. It takes a vantage point, never
a command. The script runs twice, because several answers legitimately differ:
as guest root, and inside an unprivileged container.

Two findings from building this are worth recording, because both are the
kind of thing that reads as a pass:

- The virtio device inventory observed from inside the guest is exactly the
  attached list and nothing more: net, console, two block devices, vsock, rng
  and balloon, plus the emulated PCI host bridge. No GPU, no audio, no USB,
  no filesystem device.
- `socket(AF_VSOCK)` from inside a container fails with `EPERM`, confirmed
  rather than assumed. That is the seccomp profile doing the work, and it is
  the one place the container boundary — not the VM boundary — is what stands
  between a workload and the host control channel.

The real fix is milestone M5: replace the NAT attachment with
`VZFileHandleNetworkDeviceAttachment` backed by a userspace TCP/IP stack in
the agent, so the policy lives on the host side of the boundary and there is
no bridge for VM-to-VM traffic to traverse. Until then, the network checks in
`scripts/isolation-check.sh` report `DEFER`, never `PASS`.

## What we do not claim

**Your workload is not confidential from the node owner.** The VM's memory is
a region of the agent process's address space and its disk is a file on the
operator's laptop, and they are root on their own machine. They can dump
guest memory, read or modify the disk offline, or run a patched agent. Apple
Silicon offers no encrypted-memory or attested-VM primitive, so this is a
property of the platform and not a gap that can be closed here.

The consequence is a scheduling rule, and it has to live in the control
plane, because nothing the agent does can substitute for it: every workload
carries a `confidentiality_class`, and nothing above
`internal-nonconfidential` is placed on an employee laptop. Fail closed when
the label is absent.

We also do not claim:

- **Verified results.** A malicious node can return wrong output.
- **Defence against a Virtualization.framework escape.** We contain the blast
  radius; a hypervisor zero-day is out of scope.
- **Anything about side channels.** Spectre/Meltdown-class and
  microarchitectural leakage across the VM boundary are not mitigated.
- **Node attestation.** We cannot prove a node runs unmodified software.

> In one line: this protects the employee's Mac from the workload. It does
> not protect the workload from the employee.
