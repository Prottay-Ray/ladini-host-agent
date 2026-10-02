# ladini-host-agent

Turn an idle Mac into a node that hosts sandbox workloads inside an isolated
Linux VM, so an org can run its sandbox environments on hardware it already
owns instead of renting them.

Containers run **inside the VM**, never on macOS. The agent configures the VM
and talks to an agent inside it; it never executes third-party code on the
host.

## Build it first — `go run` will not work

This binary links Virtualization.framework, which requires the
`com.apple.security.virtualization` entitlement on a **signed** binary.
`go run` and `go test` execute an unsigned temporary binary, so anything that
touches a VM fails with:

```
VZErrorDomain Code=2 "The process doesn't have the
 com.apple.security.virtualization entitlement."
```

Always build through the Makefile, which signs as its final step:

```sh
make build     # host agent  -> bin/ladini-agent  (built, then codesigned)
make guestd    # guest agent -> bin/ladini-guestd (linux/arm64, static)
```

Ad-hoc signing is enough for local development; Developer ID and notarization
only matter at packaging time.

## Requirements

Apple Silicon, macOS 13 or later, Go 1.25+, and the Command Line Tools —
Virtualization.framework is in the CLT SDK, so full Xcode is not needed.

## Use it

```sh
make build && make guestd

./bin/ladini-agent doctor          # host, signing and entitlement check
./bin/ladini-agent image build     # fetch, convert, bake and seal the golden image

./bin/ladini-agent vm up --dev-unsafe-nat
./bin/ladini-agent run --image nginx:alpine --port 8080:80
curl -s "http://$(./bin/ladini-agent vm status --ip):8080"

./bin/ladini-agent ps
./bin/ladini-agent logs <workload-id>
./bin/ladini-agent stop <workload-id>
./bin/ladini-agent vm down         # stops the VM and destroys its disk
```

For a step-by-step walkthrough with the output you should expect at each
stage — including a side-by-side against a plain `docker run` — see
[`TESTING.md`](TESTING.md).

`image build` takes several minutes the first time: it downloads a pinned
Ubuntu cloud image, converts qcow2 to raw in pure Go (no qemu dependency),
and boots the guest once to install Docker and the guest agent.

## Isolation: what this does and does not protect

Read [`docs/ISOLATION.md`](docs/ISOLATION.md) — it is the normative contract,
and `internal/vmm/config_test.go` enforces it on every build.

**We claim.** The VM gets CPU, RAM, a virtual disk and a network interface,
and nothing else. No directory sharing, no clipboard, no USB, no audio or
microphone, no graphics, no device passthrough, no Rosetta share. The guest
cannot read the host filesystem, see host processes, reach host credentials
or SSH keys, or touch the host's container runtime. The host registers no
vsock listener, so a container in the guest cannot reach the control channel.

**`--dev-unsafe-nat` suspends part of that claim.** It puts the guest on the
host's vmnet network, where it can reach services bound to `0.0.0.0` on this
Mac and scan the local network. It exists for bring-up. The isolation checks
report those results as `DEFER`, never `PASS`, until the host-side userspace
netstack replaces NAT.

**We do not claim that your workload is confidential from the node owner.**
The VM's memory is a region of the agent process's address space and its disk
is a file on the operator's laptop, and they are root on their own machine.
Apple Silicon has no encrypted-memory or attested-VM primitive, so this is a
property of the platform rather than a gap to be fixed. Do not schedule
customer data, production secrets or key material to employee nodes.

> This protects the employee's Mac from the workload. It does not protect the
> workload from the employee.

## Verify it

```sh
make verify-entitlements      # exactly one entitlement, and it is the right one
go test ./...                 # includes the omit-list guards
./scripts/host-isolation-check.sh

# the in-guest matrix, run from both vantage points (needs a running VM):
./bin/ladini-agent selftest
```

## Layout

```
cmd/ladini-agent     host CLI and supervisor; re-execs itself as the VM host
cmd/ladini-guestd    guest agent (linux/arm64), baked into the golden image
internal/vmm         the VZ device contract, the VM runner, supervisor + child
internal/image       fetch, qcow2->raw, cidata seed, APFS clone, golden build
internal/guestapi    the host/guest wire contract, and the host-side client
internal/guestd      the guest-side server and container runner (linux only)
internal/image/assets/isolation-check.sh
                     the in-guest test matrix, embedded and baked into images
docs/ISOLATION.md    the normative security contract
```
