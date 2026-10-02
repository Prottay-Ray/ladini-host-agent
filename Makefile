# The agent links Virtualization.framework, which requires the
# com.apple.security.virtualization entitlement on a SIGNED binary.
# `go run` and `go test` execute an unsigned temporary binary and will fail.
# Always build through this Makefile.

BIN       := bin/ladini-agent
GUEST_BIN := bin/ladini-guestd
ENTS      := build/ladini.entitlements
SIGN_ID   ?= -

.PHONY: all build guestd sign verify-entitlements verify-isolation clean

all: build

build: $(BIN)

$(BIN): $(shell find cmd/ladini-agent internal -name '*.go' 2>/dev/null) go.mod
	CGO_ENABLED=1 go build -o $(BIN) ./cmd/ladini-agent
	codesign --force --sign $(SIGN_ID) --entitlements $(ENTS) --options runtime $(BIN)

guestd:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o $(GUEST_BIN) ./cmd/ladini-guestd

# H1: exactly one entitlement, and it is the virtualization one.
verify-entitlements: build
	@codesign -d --entitlements - --xml $(BIN) 2>/dev/null | \
		plutil -convert xml1 -o - - | \
		grep -c 'com.apple.security.virtualization' | grep -q '^1$$' \
		&& echo "PASS H1: virtualization entitlement present"
	@codesign -d --entitlements - --xml $(BIN) 2>/dev/null | \
		plutil -convert xml1 -o - - | \
		grep -E 'com\.apple\.(vm\.networking|security\.(hypervisor|app-sandbox|cs\.))' \
		&& { echo "FAIL H1: forbidden entitlement present"; exit 1; } \
		|| echo "PASS H1: no forbidden entitlements"

# H12/H13: the omit-list, enforced on the source tree.
verify-isolation: build
	@./scripts/host-isolation-check.sh

clean:
	rm -rf bin
