//go:build linux

// Command ladini-guestd runs inside the Linux VM. It accepts control
// requests from the host over virtio-vsock and drives the container runtime.
//
// It is built for linux/arm64 with CGO disabled and baked into the golden
// image; see `make guestd`.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/guestd"
)

// tokenPath is written by cloud-init from the read-only cidata seed, root-only.
const tokenPath = "/etc/ladini/boot-token"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token, err := waitForToken(tokenPath, 180*time.Second)
	if err != nil {
		log.Error("no boot token; refusing to serve", "err", err)
		os.Exit(1)
	}
	// The token lives in memory from here on. Removing the file keeps it out
	// of reach of anything that later gains access to the guest filesystem.
	if err := os.Remove(tokenPath); err != nil {
		log.Warn("could not remove boot token file", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := guestd.NewServer(token, log).ListenAndServe(ctx); err != nil {
		log.Error("guest agent stopped", "err", err)
		os.Exit(1)
	}
}

// waitForToken polls for the token file. cloud-init writes it during its
// config stage, which can land after this unit starts, so starting up is a
// race we wait out rather than lose.
func waitForToken(path string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			token := strings.TrimSpace(string(b))
			if token != "" {
				return token, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("token not present at %s after %s", path, timeout)
		}
		time.Sleep(time.Second)
	}
}
