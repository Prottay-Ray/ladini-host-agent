package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/guestapi"
)

// RunChild is the body of the hidden `ladini-agent vmm` subcommand.
//
// This process owns the VZVirtualMachine. If Virtualization.framework takes
// it down, the supervisor survives and can report and restart -- which is the
// whole reason the VM lives in a child rather than in the agent.
func RunChild(log *slog.Logger) error {
	req, err := readChildRequest()
	if err != nil {
		return err
	}
	spec := req.Spec

	log.Info("starting vm", "id", spec.ID, "cpu", spec.CPUCount,
		"memory_gib", float64(spec.MemoryBytes)/(1<<30), "network", spec.Network)

	runner, err := Start(&spec)
	if err != nil {
		return fmt.Errorf("start vm: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	health, err := runner.WaitReady(readyCtx, req.Token, func(msg string) { log.Info(msg) })
	cancel()
	if err != nil {
		runner.Stop(context.Background())
		return err
	}
	log.Info("guest ready", "ipv4", health.IPv4, "docker", health.DockerVersion)

	srv, listener, err := controlServer(spec.Dir, runner, req.Token)
	if err != nil {
		runner.Stop(context.Background())
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Error("control socket failed", "err", err)
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelShutdown()
	srv.Shutdown(shutdownCtx)
	os.Remove(ControlSocket(spec.Dir))

	if err := runner.Stop(shutdownCtx); err != nil {
		log.Error("vm did not stop cleanly", "err", err)
		return err
	}
	log.Info("vm stopped")
	return nil
}

// controlServer exposes the guest control API on a unix socket, so that other
// invocations of the CLI can reach the guest without holding the vsock device
// or the handshake token.
//
// Access control is filesystem permissions: the socket is created inside the
// 0700 VM directory and chmod'ed 0600.
func controlServer(dir string, runner *Runner, token string) (*http.Server, net.Listener, error) {
	sock := ControlSocket(dir)
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("clear stale control socket: %w", err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on control socket: %w", err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return nil, nil, fmt.Errorf("restrict control socket: %w", err)
	}

	target, _ := url.Parse("http://guest")
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The token is injected here and nowhere else, so it exists only
			// in this process's memory and on the read-only seed the guest
			// already consumed.
			pr.Out.Header.Set(guestapi.TokenHeader, token)
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return runner.Dial(ctx)
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 15 * time.Minute, // image pulls
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(guestapi.Error{Message: "guest unreachable: " + err.Error()})
		},
	}

	return &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}, l, nil
}

// readChildRequest reads the spec from inherited file descriptor 3.
func readChildRequest() (*ChildRequest, error) {
	f := os.NewFile(3, "spec")
	if f == nil {
		return nil, fmt.Errorf("no spec on fd 3 (the vmm subcommand is not meant to be run by hand)")
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read spec from fd 3: %w", err)
	}
	var req ChildRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	if req.Token == "" {
		return nil, fmt.Errorf("spec carries no handshake token")
	}
	return &req, nil
}
