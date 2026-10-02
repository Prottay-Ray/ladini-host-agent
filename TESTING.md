# Testing the MVP by hand

Every command below is copy-pasteable, and each one shows the output actually
observed on an M1 Pro / macOS 15.6.1. If what you see differs materially from
what is printed here, that is a finding — not a step to skip.

Budget about 15 minutes, or 20 if the golden image still needs building.

---

## Prerequisites

Apple Silicon, macOS 13+, Go 1.25+, Command Line Tools. Full Xcode is **not**
needed — Virtualization.framework ships in the CLT SDK.

```sh
cd path/to/ladini-host-agent
```

---

## Step 0 — Build, and see the signing trap on purpose

```sh
make build && make guestd
```

Now run the thing that *should* fail, so it is never a mystery later:

```sh
go run ./cmd/ladini-agent doctor
```

Expected — a failure, and this exact one:

```
FAIL  virtualization  validate configuration: Error Domain=VZErrorDomain Code=2
      Description="Invalid virtual machine configuration. The process doesn't
      have the "com.apple.security.virtualization" entitlement."
ok  entitlements  none (build with `make build`)
```

`go run` and `go test` execute an *unsigned* temporary binary. The entitlement
is a property of the signed Mach-O, so anything touching a VM must go through
`make build`, which signs as its final step. This is why there is a Makefile.

---

## Step 1 — `doctor`

```sh
./bin/ladini-agent doctor
```

```
ok  macos           15.6.1
ok  arch            arm64
ok  host memory     32.0 GiB total, 9.3 GiB available
ok  state dir       /Users/<you>/Library/Application Support/Ladini
ok  free disk       182.1 GiB
ok  virtualization  supported, entitlement present
ok  entitlements    com.apple.security.virtualization
```

The `virtualization` line is not a version check — it builds a real
`VZVirtualMachineConfiguration` and asks the framework to validate it. The
`entitlements` line must list that one entitlement **and nothing else**.

---

## Step 2 — The golden image

If one already exists, just verify it:

```sh
./bin/ladini-agent image show
```

```
path       .../images/ubuntu-24.04-arm64-docker-release-20260926.raw
source     ubuntu 24.04 release-20260926
sha256     0e56ae861ca3472272dd554626127f529b0766bdf58f06396a0365cb26fc31d9
size       40 GiB virtual, 2.17 GiB on disk
built      2026-10-02T12:31:05Z
integrity  verified
```

To build from scratch — about 6 minutes cold, 90 seconds with the download
cached:

```sh
./bin/ladini-agent image build --force
```

It downloads a **pinned** Ubuntu 24.04 arm64 cloud image, verifies its
SHA-256, converts qcow2 → raw **in pure Go** (no qemu is installed or used),
grows it to 40 GiB sparse, then boots it once to install Docker and the guest
agent, and seals the result read-only (`0444`).

Worth noticing: 40 GiB virtual, 2.17 GiB actually on disk.

---

## Step 3 — Boot the VM

```sh
./bin/ladini-agent vm up --dev-unsafe-nat
```

```
  !! --dev-unsafe-nat: the guest is on the host's vmnet network.
  !! It can reach services bound to 0.0.0.0 on this Mac, and your LAN.
  !! Bring-up only. Do not run untrusted workloads. See docs/ISOLATION.md.

cloning golden image
  vmm child started, waiting for guest

vm "default" is ready
  address  192.168.64.11
  cpus     2
  memory   4 GiB
  disk     copy-on-write clone of the golden image (0 bytes until written)
```

Roughly 35–40 seconds. The warning is real and deliberate — see *Reading the
results* below.

### Optional: prove the clone is free

```sh
G=$(ls ~/Library/Application\ Support/Ladini/images/*.raw | head -1)
df -k ~ | tail -1 | awk '{print "free before: "$4" KiB"}'
time cp -c "$G" /tmp/clone-test.raw
df -k ~ | tail -1 | awk '{print "free after:  "$4" KiB"}'
rm /tmp/clone-test.raw
```

Observed: **0.002 s, and the two free-space figures are identical** — a 40 GiB
disk cloned for zero bytes. APFS copy-on-write is what makes a
fresh VM per job affordable. (Use `df`, not `du` — `du` counts shared clone
blocks against every file that shares them.)

---

## Step 4 — Run a container inside the VM and reach it from macOS

```sh
./bin/ladini-agent run --image nginx:alpine --port 8080:80
```

```
pulling and starting nginx:alpine in vm "default"
workload ee085807 is running
  http://192.168.64.11:8080 -> container port 80
```

```sh
curl -s "http://$(./bin/ladini-agent vm status --ip):8080" | grep -E '<title>|<h1>'
```

```
<title>Welcome to nginx!</title>
<h1>Welcome to nginx!</h1>
```

That page was served by a container, inside a Linux VM, by a Docker daemon
that the agent never touched directly — the request path is
`curl → vmnet → guest → docker → nginx`, and the control path that started it
was `CLI → unix socket → vsock → guest agent → docker`.

---

## Step 5 — How this differs from a normal `docker run`

This is the whole argument for the project, so test it rather than take it
on faith.

### 5a. Plain Docker on your Mac

```sh
docker run --rm -v "$HOME/.ssh:/stolen:ro" alpine:3 \
  sh -c 'find /stolen -maxdepth 1; echo; head -1 /stolen/id_ed25519'
```

Observed on this machine:

```
/stolen
/stolen/id_ed25519
/stolen/known_hosts
/stolen/config
...
-----BEGIN OPENSSH PRIVATE KEY-----
```

A container read your private key. Not because Docker is broken — because
Docker Desktop shares your home directory into its VM by default, and `-v` is
one flag. The same works for `~/.aws` and `~/.docker/config.json`.

> If you have turned Docker Desktop's file sharing off, this step shows
> nothing. That is a setting you chose, and one that can be switched back at
> any time by you or by anything running as you.

### 5b. The same attempt against ladini

First, look for the flag:

```sh
./bin/ladini-agent run --help | grep -iE 'volume|mount|-v,'
```

Nothing. There is no `-v`. And this is not just a missing CLI flag — the wire
protocol has no field that could carry a host path:

```sh
sed -n '/^type RunWorkload/,/^}/p' internal/guestapi/types.go
```

The guest builds the `docker` argv itself from those typed fields, so a bind
mount is **not expressible**, rather than merely rejected. Same for
`--privileged`, `--device`, `--pid=host` and `--security-opt`.

Now prove there is nothing to mount even if you could:

```sh
./bin/ladini-agent run --id probe --image alpine:3 -- sh -c '
  echo "--- host home dir? ---";        ls /Users 2>&1
  echo "--- any ssh keys at all? ---";  find / -xdev -name "id_*" -o -xdev -name "known_hosts" 2>/dev/null | head -5
  echo "--- host docker socket? ---";   ls -la /var/run/docker.sock 2>&1
  echo "--- host filesystem mounts? ---"; findmnt -t virtiofs,9p -n 2>/dev/null'
sleep 5 && ./bin/ladini-agent logs probe && ./bin/ladini-agent stop probe
```

Observed:

```
--- host home dir? ---
ls: /Users: No such file or directory
--- any ssh keys at all? ---
--- host docker socket? ---
ls: /var/run/docker.sock: No such file or directory
--- host filesystem mounts? ---
```

Three of those are empty because there is nothing to find.

### What a container can reach, side by side

| | `docker run` on your Mac | `ladini-agent run` |
|---|---|---|
| Your home directory | yes, with `-v` (shared by default) | no — no host filesystem is attached at all |
| `~/.ssh`, `~/.aws`, registry creds | yes, with `-v` | no |
| Host Docker socket | yes, with `-v /var/run/docker.sock` | no — and the agent never opens it either |
| Clipboard | yes (host pasteboard via Docker Desktop) | no — the Spice agent port is not attached |
| USB devices | yes, with `--device` | no — the guest has no USB bus |
| Request a bind mount at all | yes | **not expressible in the API** |
| Blast radius of a container escape | your laptop | a disposable VM, destroyed on `vm down` |

The difference is not that ladini forbids more flags. It is that the VM has
nothing of yours in it to reach.

---

## Step 6 — The isolation matrix

```sh
./bin/ladini-agent selftest
```

```
========== vantage: guest ==========
pass 22   fail 0
========== vantage: container ==========
pass 22   fail 0
```

Runs twice, because several answers legitimately differ: once as guest root,
once inside an unprivileged container. There is no SSH in the guest, so the
script is baked into the golden image and this endpoint runs exactly that
script — it takes a vantage point, never a command.

Two checks are worth reading in full output (`./bin/ladini-agent selftest | less`):

- **F18c** — the virtio device inventory, observed from inside the guest. It
  is exactly net, console, two block devices, vsock, rng and balloon, plus the
  emulated PCI host bridge. No GPU, no audio, no USB, no filesystem device.
  This is the device contract, read back from the other side.
- **F28** — `socket(AF_VSOCK)` inside a container fails with `EPERM`. That is
  the seccomp profile, and it is the one place the *container* boundary, not
  the VM boundary, is what stands between a workload and the host control
  channel. Verified, not assumed.

---

## Step 7 — Host-side checks

```sh
./scripts/host-isolation-check.sh
```

```
pass 10   fail 0
```

These verify what the guest cannot see for itself: that the binary carries
exactly one entitlement and has the hardened runtime, that the state directory
is `0700` with no file looser than `0600`, that the golden image is sealed
`0444`, that the VM host process opens no TCP listener and holds no handle
into your home directory, and that the guest cannot see your Mac's serial.

---

## Step 8 — Lifecycle

```sh
./bin/ladini-agent ps
./bin/ladini-agent logs <workload-id> --tail 5
./bin/ladini-agent stop <workload-id>
```

The nginx access log will contain your `curl` from step 4 — useful proof that
the request really traversed the whole path.

---

## Step 9 — Teardown

```sh
./bin/ladini-agent vm down
ls -A ~/Library/Application\ Support/Ladini/vms/ | wc -l    # must print 0
```

```
stopping vm "default" (pid 87779)
vm stopped and destroyed
       0
```

The VM directory and its disk are destroyed. Ephemerality is the control that
keeps one workload from seeing another's leftovers.

---

## Reading the results

| Marker | Meaning |
|---|---|
| `PASS` | Verified now. |
| `DEFER` | **Not a guarantee yet.** Network reachability only. |
| `INFO` | Expected condition, documented, with the real assertion elsewhere. |
| `SKIP` | The tool for that check is absent in that vantage point. |

**Why the network checks say `DEFER` and not `PASS`.** Under
`--dev-unsafe-nat` the guest sits on your Mac's vmnet network. The guest image
loads an nftables policy that currently *does* block your host's services and
your LAN — you will see `DEFER F19  no host service reachable` — but guest
root can run `nft flush ruleset` and undo it. A control the adversary can
switch off is not a guarantee, so it is reported honestly as deferred rather
than counted as a pass.

That closes at **M5**, which replaces NAT with a userspace network stack
inside the agent, putting the policy on the host side of the boundary where
the guest cannot reach it. Until then, `--dev-unsafe-nat` is required for
image pulls and suspends part of the isolation claim. Don't run anything
genuinely untrusted yet.

---

## What this does not do yet

- **No remote control plane.** You cannot bring containers up or down from a
  web panel. Everything here is local CLI. That is M7; the design decision is
  settled (the agent polls a control plane every ~15s and converges on desired
  state, so no inbound port is needed on the laptop) but none of it is built.
- **No daemon.** Nothing survives logout or reboot; there is no launchd job
  and no service account. That is M6.
- **No packaging.** No `.pkg`, no notarization, no Developer ID. That is M8.
- **Workloads are not confidential from you.** The VM's memory is a region of
  the agent process and its disk is a file on this Mac, and you are root here.
  That is a property of Apple Silicon, not a gap to be closed. See
  `docs/ISOLATION.md`.

---

## Troubleshooting

**`VZErrorDomain Code=2 ... entitlement`** — you ran an unsigned binary. Use
`make build`, never `go run`.

**Changed `cmd/ladini-guestd` and nothing happened** — the guest agent is
baked into the golden image. Rebuild it and the image:
`make guestd && ./bin/ladini-agent image build --force`.

**`vm up` says it is already running** — a previous VM is still up or left a
stale record. `./bin/ladini-agent vm down` first.

**`vm up` times out waiting for the guest** — the failure message already
includes the tail of the VM host process's log. For more, keep the disk and
read it directly:

```sh
./bin/ladini-agent vm down --keep-disk
cat ~/Library/Application\ Support/Ladini/vms/default/vmm.log
```

```json
{"level":"INFO","msg":"starting vm","id":"default","cpu":2,"memory_gib":4,"network":"nat"}
{"level":"INFO","msg":"guest agent reachable over vsock"}
{"level":"INFO","msg":"guest ready","ipv4":"192.168.64.11","docker":"29.8.2"}
```

> **Known gap:** `vm console` is wired up and a per-VM `console.log` is
> captured write-only, but it currently comes out **0 bytes**. The Ubuntu
> cloud image does not put `console=hvc0` on its kernel command line, and the
> golden image masks `getty` on `hvc0` deliberately, so nothing is written to
> it. That means the serial console is empty exactly when you would want it —
> during a boot failure. Fixing it is a two-line addition to the cloud-init
> (`console=hvc0` via GRUB) plus a rebake; `vmm.log` is the working
> diagnostic until then.

**A container exits immediately** — check `./bin/ladini-agent logs <id>`.
Images that need a writable root or `chown` work by default; `--read-only` and
`--drop-all-caps` are opt-in precisely because they break such images.
