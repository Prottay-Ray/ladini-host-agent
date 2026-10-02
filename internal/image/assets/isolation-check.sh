#!/bin/sh
# Isolation checks, run INSIDE the guest.
#
# Run it twice -- the answers legitimately differ between the two contexts:
#
#   as guest root:   sh /path/to/isolation-check.sh
#   in a container:  docker run --rm -v "$PWD:/t:ro" alpine:3 sh /t/isolation-check.sh
#
# Each check prints "PASS <id>", "FAIL <id>: <observed>", or "DEFER <id>".
# DEFER marks the network checks: under --dev-unsafe-nat the guest-side
# nftables policy may well block these, but guest root can flush that policy,
# so they are not a guarantee until the host-side netstack lands (M5).
#
# set -u but deliberately NOT set -e: for most checks here, a command that
# fails IS the passing condition.

set -u

CTX="${CTX:-guest}"

# Results are appended to a file rather than counted in shell variables.
# Most checks here run inside a pipeline (`... | expect_empty Fn`), and a
# pipeline stage is a subshell: a counter incremented there is lost when it
# exits. Counting in variables would therefore drop exactly the FAILs raised
# by the majority of the checks, and the script would exit 0 while reporting
# failures on screen. File appends survive subshells; variables do not.
RESULTS=$(mktemp /tmp/ladini-isolation.XXXXXX)
trap 'rm -f "$RESULTS"' EXIT

ok()    { echo "PASS  $1" | tee -a "$RESULTS"; }
no()    { echo "FAIL  $1: $2" | tee -a "$RESULTS"; }
defer() { echo "DEFER $1  $2" | tee -a "$RESULTS"; }
skip()  { echo "SKIP  $1  $2" | tee -a "$RESULTS"; }

# expect_empty <id> <description> -- passes when stdin is empty
expect_empty() {
	_id="$1"; _out=$(cat)
	if [ -z "$_out" ]; then ok "$_id"; else no "$_id" "$(echo "$_out" | head -3 | tr '\n' ' ')"; fi
}

echo "ladini isolation check (context: $CTX)"
echo

# ---------------------------------------------------------------- filesystem
echo "-- host filesystem --"
findmnt -t virtiofs,9p,fuse -n 2>/dev/null | expect_empty F1

# F2/F3: the virtio device inventory IS the device contract, read back from
# the guest. Permitted ids: 1 net, 2 blk, 3 console, 4 rng, 5 balloon, 19 vsock.
# Forbidden: 9 (9p) and 26 (virtiofs).
if [ -d /sys/bus/virtio/devices ]; then
	unexpected=""
	for d in /sys/bus/virtio/devices/*; do
		[ -e "$d/device" ] || continue
		id=$(cat "$d/device" 2>/dev/null)
		case "$id" in
			0x0001|0x0002|0x0003|0x0004|0x0005|0x0013|1|2|3|4|5|19) ;;
			*) unexpected="$unexpected $(basename "$d"):$id" ;;
		esac
	done
	[ -z "$unexpected" ] && ok F3 || no F3 "unexpected virtio devices:$unexpected"
else
	skip F3 "no /sys/bus/virtio (container context)"
fi

{ findmnt 2>/dev/null | grep -i rosetta; ls /mnt/rosetta 2>/dev/null; } | expect_empty F4

for p in /Users "/System/Volumes" /host /mnt/host /Volumes; do
	[ -e "$p" ] && echo "$p exists"
done | expect_empty F5

find / -xdev \( -name 'id_ed25519*' -o -name 'id_rsa*' -o -name 'known_hosts' \
	-o -name '*.keychain-db' \) 2>/dev/null | expect_empty F6

# ------------------------------------------------------------- seed / secrets
echo
echo "-- seed and credential leakage --"
# The cidata seed remains attached for the life of the VM: Virtualization.
# framework has no hot-unplug for storage, so it cannot be removed after
# cloud-init consumes it. That is acceptable only because of what is on it --
# a hostname, an instance id, and a single-boot vsock token that is consumed
# and unlinked at startup (F8b). The real assertions are F8 and F8b; the
# presence of the device is reported, not failed. See docs/ISOLATION.md.
if command -v blkid >/dev/null 2>&1; then
	if blkid 2>/dev/null | grep -qi cidata; then
		echo "INFO  F7  the cidata seed is still attached (expected; F8/F8b are the assertions)"
	else
		ok F7
	fi
else
	skip F7 "blkid unavailable"
fi

# The per-boot vsock token legitimately travelled on the seed and is expected
# to be gone by now; nothing else may be there at all.
grep -rIl -e 'AKIA' -e 'ASIA' -e 'BEGIN .*PRIVATE KEY' -e 'ghp_' -e 'dkr.ecr' \
	/var/lib/cloud /etc/ladini 2>/dev/null | expect_empty F8
[ -e /etc/ladini/boot-token ] && echo "boot token still on disk" | expect_empty F8b || ok F8b

grep -ciE 'token|secret|passw|_key=' /proc/cmdline 2>/dev/null | grep -qv '^0$' \
	&& no F9 "secrets on the kernel command line" || ok F9

# ------------------------------------------------------------ host processes
echo
echo "-- host processes and environment --"
ps -eo comm= 2>/dev/null | grep -iE '^(Finder|WindowServer|loginwindow|com\.docker)' | expect_empty F10
env | grep -iE 'AWS_|GITHUB_|_TOKEN|_SECRET|KEYCHAIN|DOCKER_HOST' | expect_empty F11

# ------------------------------------------------------------ container runtime
echo
echo "-- container runtime --"
if [ "$CTX" = "container" ]; then
	if [ -S /var/run/docker.sock ]; then
		no F12 "the container can see a container runtime socket"
	else
		ok F12
	fi
else
	if [ -S /var/run/docker.sock ]; then
		perms=$(stat -c '%U %G %a' /var/run/docker.sock 2>/dev/null)
		case "$perms" in
			"root docker 660"|"root root 660") ok F13 ;;
			*) no F13 "socket perms are '$perms', expected root docker 660" ;;
		esac
		getent group docker 2>/dev/null | cut -d: -f4 | tr ',' '\n' | grep -v '^$' | expect_empty F13b
	else
		skip F13 "no runtime socket"
	fi
fi

# ----------------------------------------------------------- clipboard / devices
echo
echo "-- clipboard, usb, audio, passthrough --"
cat /sys/class/virtio-ports/*/name 2>/dev/null | grep -i spice | expect_empty F15
{ ls /sys/bus/usb/devices 2>/dev/null; lsusb 2>/dev/null; } | expect_empty F16
# /dev/snd/seq and /dev/snd/timer are created by the ALSA core whether or not
# any sound hardware exists, so their presence proves nothing. A real audio
# device would show a PCM node, a control node, or a card in aplay.
{ ls /dev/snd/pcm* /dev/snd/controlC* 2>/dev/null; aplay -l 2>/dev/null | grep '^card'; } | expect_empty F17
# A PCI root bus exists in any VZ guest and /dev/vfio/vfio is just the VFIO
# container node that ships with the module. Passthrough would show up as a
# bound IOMMU group or a numbered vfio group device.
{ ls /dev/vfio/[0-9]* 2>/dev/null; ls /sys/kernel/iommu_groups 2>/dev/null; } | expect_empty F18

# Every PCI endpoint must be a virtio device (vendor 0x1af4). The one
# exception is the emulated PCI host bridge at class 0x060000, which
# Virtualization.framework presents with Apple's vendor id (0x106b) -- it is
# the root complex, not a passed-through device. Anything else would mean
# real host hardware is visible in the guest.
nonvirtio=""
for d in /sys/bus/pci/devices/*; do
	[ -e "$d/vendor" ] || continue
	v=$(cat "$d/vendor" 2>/dev/null)
	c=$(cat "$d/class" 2>/dev/null)
	[ "$c" = "0x060000" ] && continue
	[ "$v" = "0x1af4" ] || nonvirtio="$nonvirtio $(basename "$d"):vendor=$v,class=$c"
done
if [ -z "$nonvirtio" ]; then ok F18b; else no F18b "non-virtio PCI endpoints:$nonvirtio"; fi

# The virtio device inventory IS the omit-list, observed from inside the
# guest. Permitted: net (0x1041), block (0x1042), console (0x1043),
# rng (0x1044), balloon (0x1045), vsock (0x1053). A GPU, a sound card, a
# USB controller or a filesystem device would each show up here.
forbidden_pci=""
for d in /sys/bus/pci/devices/*; do
	[ -e "$d/device" ] || continue
	v=$(cat "$d/vendor" 2>/dev/null); [ "$v" = "0x1af4" ] || continue
	id=$(cat "$d/device" 2>/dev/null)
	case "$id" in
		0x1041|0x1042|0x1043|0x1044|0x1045|0x1053) ;;
		*) forbidden_pci="$forbidden_pci $(basename "$d"):$id" ;;
	esac
done
if [ -z "$forbidden_pci" ]; then ok F18c; else no F18c "unexpected virtio devices:$forbidden_pci"; fi

# ------------------------------------------------------------------- network
echo
echo "-- network (DEFERRED under --dev-unsafe-nat; see docs/ISOLATION.md) --"
gw=$(ip route 2>/dev/null | awk '/^default/ {print $3; exit}')
probe_tcp() { # host port -> prints "host:port" if reachable
	if command -v nc >/dev/null 2>&1; then
		nc -z -w2 "$1" "$2" >/dev/null 2>&1 && echo "$1:$2"
	fi
}

if [ -n "${gw:-}" ]; then
	open=""
	for p in 22 80 443 445 2375 2376 5000 7000 8083 9000 9092 15611 58254; do
		r=$(probe_tcp "$gw" "$p"); [ -n "$r" ] && open="$open $r"
	done
	[ -z "$open" ] && defer F19 "no host service reachable on the gateway $gw" \
	                || defer F19 "REACHABLE on host gateway:$open"
else
	skip F19 "no default route"
fi

if ping -c1 -W2 "${gw:-127.0.0.1}" >/dev/null 2>&1; then
	defer F21 "gateway $gw answers ping"
else
	defer F21 "gateway unreachable"
fi

if command -v curl >/dev/null 2>&1; then
	if curl -s --max-time 3 -o /dev/null http://169.254.169.254/latest/meta-data/ 2>/dev/null; then
		defer F24 "link-local metadata endpoint REACHABLE"
	else
		defer F24 "link-local metadata unreachable"
	fi
	# Negative control: egress to a public registry MUST work, or image
	# pulls are broken and every other result here is meaningless.
	code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 https://registry-1.docker.io/v2/ 2>/dev/null)
	case "$code" in
		200|401) ok F26 ;;
		*) no F26 "public egress is broken (got '$code'); image pulls will fail" ;;
	esac
else
	skip F26 "curl unavailable"
fi

# ---------------------------------------------------------------------- vsock
echo
echo "-- vsock control surface --"
# AF_VSOCK is not isolated by network namespaces, so this is the one place
# the container boundary is load-bearing. The host must have no listener.
if command -v python3 >/dev/null 2>&1; then
	python3 - <<'PY' | tee -a "$RESULTS"
import socket
AF_VSOCK = getattr(socket, "AF_VSOCK", 40)
HOST_CID = 2
reached = []
blocked = None
for port in (1, 22, 80, 443, 1024, 1025, 2222, 5000, 9999):
    try:
        s = socket.socket(AF_VSOCK, socket.SOCK_STREAM)
    except OSError as e:
        blocked = e
        break
    s.settimeout(2)
    try:
        s.connect((HOST_CID, port))
        reached.append(port)
    except OSError:
        pass
    finally:
        s.close()
if blocked is not None:
    print("PASS  F28  socket(AF_VSOCK) blocked: %s" % blocked)
elif reached:
    print("FAIL  F27: host vsock listener reachable on ports %s" % reached)
else:
    print("PASS  F27")
PY
else
	skip F27 "python3 unavailable"
fi

# ----------------------------------------------------------- container posture
if [ "$CTX" = "container" ]; then
	echo
	echo "-- container privilege posture --"
	grep -E '^(Seccomp|NoNewPrivs):' /proc/self/status 2>/dev/null | while read -r k v; do
		case "$k$v" in
			"Seccomp:2") echo "PASS  F30a" ;;
			"NoNewPrivs:1") echo "PASS  F30b" ;;
			"Seccomp:"*) echo "FAIL  F30a: seccomp not filtering (=$v)" ;;
			"NoNewPrivs:"*) echo "FAIL  F30b: no_new_privs not set (=$v)" ;;
		esac
	done
	# Inside a container pid 1 IS the container's own init, so /proc/1/root
	# being readable is normal and proves nothing. What must not be visible
	# is the GUEST's filesystem -- the guest agent binary is the marker.
	if [ -e /proc/1/root/usr/local/bin/ladini-guestd ] || [ -e /proc/1/root/etc/ladini ]; then
		no F31 "the container can see the guest root filesystem"
	else
		ok F31
	fi
	# No mount may come from the guest's own disk. A container legitimately
	# has overlay, proc, sysfs, tmpfs, devpts, mqueue and cgroup mounts; a
	# real on-disk filesystem here would mean guest storage was bind-mounted
	# in. (Checking for "root mounted at /" instead would match the
	# container's own overlay rootfs, which every container has.)
	# The container runtime always injects three files from the guest's own
	# filesystem -- /etc/hostname, /etc/hosts and /etc/resolv.conf -- so an
	# on-disk mount at those paths is expected. An on-disk mount ANYWHERE
	# ELSE would mean guest storage had been bind-mounted in, which is what
	# this check exists to catch.
	diskmounts=$(awk '{mp=$5; for(i=1;i<=NF;i++) if($i=="-"){print $(i+1)" "mp; break}}' \
		/proc/self/mountinfo 2>/dev/null \
		| grep -vE '^(overlay|proc|sysfs|tmpfs|devpts|mqueue|cgroup|cgroup2|securityfs|bpf|pstore|debugfs|tracefs|configfs|fusectl|binfmt_misc|nsfs|ramfs|autofs|hugetlbfs|devtmpfs) ' \
		| grep -vE ' /etc/(hostname|hosts|resolv\.conf)$' \
		| sort -u)
	if [ -z "$diskmounts" ]; then
		ok F31b
	else
		no F31b "unexpected on-disk mounts in the container: $(echo "$diskmounts" | tr '\n' ';')"
	fi
fi

# -------------------------------------------------------- identity / firewall
echo
echo "-- host identity leakage and guest firewall --"
# VZ's EFI firmware reports a synthetic "Virtualization-<uuid>" serial. That
# is fine; what would not be fine is an Apple serial (10-12 alphanumerics) or
# anything resembling the host's own identity. The host-side counterpart in
# scripts/host-isolation-check.sh compares against the Mac's real serial.
serial=$(cat /sys/class/dmi/id/product_serial 2>/dev/null || echo "")
case "$serial" in
	""|"None"|"0"|"Not Specified") ok F32 ;;
	Virtualization-*) ok F32 ;;
	*) no F32 "dmi product_serial is '$serial', which is not a synthetic VZ identifier" ;;
esac

if command -v nft >/dev/null 2>&1; then
	if nft list ruleset 2>/dev/null | grep -q 'ip daddr @private drop'; then
		ok F33
	else
		no F33 "the guest egress policy is not loaded"
	fi
else
	skip F33 "nft unavailable in this context"
fi

pass=$(grep -c '^PASS ' "$RESULTS" 2>/dev/null || echo 0)
fail=$(grep -c '^FAIL ' "$RESULTS" 2>/dev/null || echo 0)
defer=$(grep -c '^DEFER ' "$RESULTS" 2>/dev/null || echo 0)
skip=$(grep -c '^SKIP ' "$RESULTS" 2>/dev/null || echo 0)

echo
echo "-------------------------------------------"
echo "pass $pass   fail $fail   deferred $defer   skipped $skip"
echo "deferred checks are network reachability; they become guarantees"
echo "when the host-side userspace netstack replaces NAT (milestone M5)."
if [ "$fail" -ne 0 ]; then
	echo
	grep '^FAIL ' "$RESULTS"
	exit 1
fi
