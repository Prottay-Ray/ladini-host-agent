//go:build linux

package guestd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Prottay-Ray/ladini-host-agent/internal/guestapi"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// Version is stamped into health responses so the host can tell which guest
// agent a golden image carries.
const Version = "0.1.0"

// Server is the guest-side control endpoint.
type Server struct {
	runner *Runner
	token  string
	log    *slog.Logger
	start  time.Time
}

// NewServer reads the per-boot handshake token and returns a ready server.
func NewServer(token string, log *slog.Logger) *Server {
	return &Server{runner: &Runner{}, token: token, log: log, start: time.Now()}
}

// ListenAndServe serves the control API on the vsock port.
//
// The guest listens and the host dials in. The reverse -- a listener on the
// host -- would be reachable from any unprivileged container in this guest,
// because AF_VSOCK is not isolated by network namespaces.
func (s *Server) ListenAndServe(ctx context.Context) error {
	l, err := vsock.Listen(guestapi.Port, nil)
	if err != nil {
		return fmt.Errorf("listen on vsock port %d: %w", guestapi.Port, err)
	}
	defer l.Close()

	srv := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	s.log.Info("guest agent listening", "vsock_port", guestapi.Port, "version", Version)
	if err := srv.Serve(l); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("POST /v1/workloads", s.createWorkload)
	mux.HandleFunc("GET /v1/workloads", s.listWorkloads)
	mux.HandleFunc("GET /v1/workloads/{id}", s.getWorkload)
	mux.HandleFunc("DELETE /v1/workloads/{id}", s.deleteWorkload)
	mux.HandleFunc("GET /v1/workloads/{id}/logs", s.workloadLogs)
	mux.HandleFunc("GET /v1/selftest", s.selfTest)
	return s.authenticate(mux)
}

// authenticate rejects any request without the per-boot token.
//
// This is the backstop, not the control: the structural protection is that
// the host registers no vsock listener, so there should be nothing here for
// a container to reach in the first place. A failed handshake therefore means
// something is wrong, and is logged loudly.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || r.Header.Get(guestapi.TokenHeader) != s.token {
			s.log.Warn("rejected unauthenticated control request",
				"remote", r.RemoteAddr, "path", r.URL.Path)
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h := guestapi.GuestHealth{
		GuestdVersion: Version,
		UptimeSec:     int64(time.Since(s.start).Seconds()),
		IPv4:          primaryIPv4(),
	}
	h.DockerVersion, h.DockerOK = s.runner.Version(r.Context())
	if wl, err := s.runner.List(r.Context()); err == nil {
		h.Workloads = len(wl)
	}
	if la, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Sscanf(string(la), "%f", &h.LoadAvg1)
	}
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err == nil {
		unitSize := uint64(si.Unit)
		h.MemTotalBytes = uint64(si.Totalram) * unitSize
		h.MemAvailBytes = uint64(si.Freeram) * unitSize
	}
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err == nil {
		h.DiskTotal = st.Blocks * uint64(st.Bsize)
		h.DiskFree = st.Bavail * uint64(st.Bsize)
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) createWorkload(w http.ResponseWriter, r *http.Request) {
	var req guestapi.RunWorkload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return
	}
	if req.ID == "" || req.Image == "" {
		writeError(w, http.StatusBadRequest, "id and image are required")
		return
	}
	if !validID(req.ID) {
		writeError(w, http.StatusBadRequest, "id must be alphanumeric, dash or underscore")
		return
	}
	s.log.Info("starting workload", "id", req.ID, "image", req.Image)
	wl, err := s.runner.Run(r.Context(), req)
	if err != nil {
		s.log.Error("workload failed to start", "id", req.ID, "err", err)
		writeJSON(w, http.StatusInternalServerError, wl)
		return
	}
	writeJSON(w, http.StatusCreated, wl)
}

func (s *Server) listWorkloads(w http.ResponseWriter, r *http.Request) {
	wl, err := s.runner.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wl == nil {
		wl = []guestapi.Workload{}
	}
	writeJSON(w, http.StatusOK, wl)
}

func (s *Server) getWorkload(w http.ResponseWriter, r *http.Request) {
	wl, err := s.runner.Inspect(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wl)
}

func (s *Server) deleteWorkload(w http.ResponseWriter, r *http.Request) {
	grace, _ := strconv.Atoi(r.URL.Query().Get("grace"))
	if err := s.runner.Stop(r.Context(), r.PathValue("id"), grace); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workloadLogs(w http.ResponseWriter, r *http.Request) {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	out, err := s.runner.Logs(r.Context(), r.PathValue("id"), tail)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(out))
}

// selfTest runs the baked isolation matrix. It takes a vantage point, not a
// command: there is no way to ask this endpoint to run arbitrary code.
func (s *Server) selfTest(w http.ResponseWriter, r *http.Request) {
	out, err := s.runner.SelfTest(r.Context(), r.URL.Query().Get("context"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(out))
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// primaryIPv4 reports the guest's address on its network interface. The host
// reads this from /v1/health rather than scraping the vmnet lease file, which
// other VMs on the host also write to.
func primaryIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "docker") || strings.HasPrefix(iface.Name, "br-") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil {
					return v4.String()
				}
			}
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, guestapi.Error{Message: msg})
}
