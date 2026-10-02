#!/bin/bash
# Isolation checks, run on the macOS HOST.
#
# These verify the properties the guest cannot observe for itself: how the
# binary is signed, what the agent process can reach, and that the omit-list
# is still honoured by the source tree.

set -u

BIN="${BIN:-bin/ladini-agent}"
pass=0; fail=0

ok() { pass=$((pass+1)); echo "PASS  $1"; }
no() { fail=$((fail+1)); echo "FAIL  $1: $2"; }
skip() { echo "SKIP  $1  $2"; }

echo "ladini host isolation check"
echo

echo "-- code signing --"
if [ ! -x "$BIN" ]; then
	no H1 "$BIN not built; run make build"
else
	ents=$(codesign -d --entitlements - --xml "$BIN" 2>/dev/null | plutil -convert xml1 -o - - 2>/dev/null)
	if echo "$ents" | grep -q 'com.apple.security.virtualization'; then ok H1a
	else no H1a "the virtualization entitlement is missing"; fi

	# Each of these would either widen the VM's reach or weaken the agent's
	# own hardening.
	forbidden=$(echo "$ents" | grep -oE 'com\.apple\.(vm\.networking|security\.(hypervisor|app-sandbox|cs\.[a-z-]+))' | sort -u)
	if [ -z "$forbidden" ]; then ok H1b
	else no H1b "forbidden entitlements present: $(echo "$forbidden" | tr '\n' ' ')"; fi

	if codesign -d -v "$BIN" 2>&1 | grep -q 'flags=.*runtime'; then ok H2
	else no H2 "the hardened runtime is not enabled"; fi
fi

echo
echo "-- state on disk --"
STATE="$HOME/Library/Application Support/Ladini"
if [ -d "$STATE" ]; then
	mode=$(stat -f '%Lp' "$STATE")
	[ "$mode" = "700" ] && ok H5a || no H5a "state dir mode is $mode, expected 700"

	loose=$(find "$STATE" -type f ! -name '*.raw' ! -name '*.img' \
		-perm +077 2>/dev/null | head -5)
	[ -z "$loose" ] && ok H5b || no H5b "group/other-readable files: $(echo "$loose" | tr '\n' ' ')"

	golden=$(ls "$STATE"/images/*.raw 2>/dev/null | head -1)
	if [ -n "$golden" ]; then
		gmode=$(stat -f '%Lp' "$golden")
		[ "$gmode" = "444" ] && ok H6 || no H6 "golden image mode is $gmode, expected 444 (sealed)"
	else
		skip H6 "no golden image built yet"
	fi
else
	skip H5a "no state directory yet"
fi

echo
echo "-- the running agent --"
PID=$(pgrep -f 'ladini-agent vmm' | head -1)
if [ -n "${PID:-}" ]; then
	# H9: the agent must not open a listening socket of its own. Its control
	# surface is a unix socket inside the 0700 VM directory.
	listeners=$(lsof -nP -iTCP -sTCP:LISTEN -a -p "$PID" 2>/dev/null | tail -n +2)
	[ -z "$listeners" ] && ok H9 || no H9 "the vmm child is listening on TCP: $(echo "$listeners" | head -2)"

	# H10: the VM host process must hold no handle into the user's data.
	handles=$(lsof -p "$PID" 2>/dev/null | grep -cE '\.ssh|keychain|/Volumes/|\.docker/config')
	[ "$handles" = "0" ] && ok H10 || no H10 "$handles open handles into user data"
else
	skip H9 "no vmm child running"
fi

echo
echo "-- vmnet exposure --"
# H8 is the M5 gate. Under --dev-unsafe-nat a vmnet bridge exists by design;
# once the host-side userspace netstack lands there must be no bridge at all.
bridges=$(ifconfig 2>/dev/null | grep -cE '^bridge1[0-9][0-9]:')
if [ -n "${PID:-}" ]; then
	if [ "$bridges" = "0" ]; then ok H8
	else echo "DEFER H8  a vmnet bridge is up (expected under --dev-unsafe-nat; must be 0 after M5)"; fi
else
	skip H8 "no vm running"
fi

echo
echo "-- host identity --"
# The guest-side check can only confirm its serial looks synthetic; only the
# host can confirm it is not the Mac's.
MACSER=$(ioreg -l 2>/dev/null | awk -F'"' '/IOPlatformSerialNumber/ {print $4; exit}')
MACUUID=$(ioreg -l 2>/dev/null | awk -F'"' '/IOPlatformUUID/ {print $4; exit}')
if [ -n "${PID:-}" ] && [ -n "$MACSER" ]; then
	GUESTSER=$(./bin/ladini-agent selftest --context guest 2>/dev/null | grep -i 'product_serial' | head -1)
	if echo "$GUESTSER" | grep -qF "$MACSER" || echo "$GUESTSER" | grep -qF "$MACUUID"; then
		no H16 "the guest can see the Mac's serial or hardware UUID"
	else
		ok H16
	fi
else
	skip H16 "no vm running, or the host serial is unreadable"
fi

echo
echo "-- the omit-list, enforced on the source tree --"
if go test ./internal/vmm/ -run 'TestOmitList|TestHostRegistersNoVsockListener|TestNeverShellsOutToHostDocker' >/dev/null 2>&1; then
	ok "H12/H13"
else
	no "H12/H13" "the omit-list guards fail; run: go test ./internal/vmm/ -v"
fi

echo
echo "-- teardown --"
stale=$(ls -d "$STATE"/vms/*/ 2>/dev/null | wc -l | tr -d ' ')
echo "INFO  H14  $stale vm directories present (vm down must leave 0)"

echo
echo "-------------------------------------------"
echo "pass $pass   fail $fail"
[ "$fail" -eq 0 ] || exit 1
