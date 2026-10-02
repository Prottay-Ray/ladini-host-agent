package guestapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Client speaks the control protocol to a guest agent over an already
// established connection factory.
//
// The host always dials; it never listens. Dial is supplied by the VMM, which
// owns the VZVirtioSocketDevice for this VM, so possession of a Client is
// structurally scoped to one VM.
type Client struct {
	http  *http.Client
	token string
}

// NewClient wraps a vsock dialer in an HTTP client.
func NewClient(dial func(ctx context.Context) (net.Conn, error), token string) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 15 * time.Minute, // image pulls are slow
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dial(ctx)
				},
				// One connection per request keeps the lifetime of a pull
				// from interfering with a concurrent health poll.
				DisableKeepAlives: true,
			},
		},
		token: token,
	}
}

// Health returns the guest's self-report. The host uses this both as the
// readiness gate after boot and as the source of the guest's IP address.
func (c *Client) Health(ctx context.Context) (*GuestHealth, error) {
	var h GuestHealth
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Run starts a workload in the guest.
func (c *Client) Run(ctx context.Context, req RunWorkload) (*Workload, error) {
	var w Workload
	if err := c.do(ctx, http.MethodPost, "/v1/workloads", req, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// List returns every workload the guest is managing.
func (c *Client) List(ctx context.Context) ([]Workload, error) {
	var wl []Workload
	if err := c.do(ctx, http.MethodGet, "/v1/workloads", nil, &wl); err != nil {
		return nil, err
	}
	return wl, nil
}

// Get returns one workload.
func (c *Client) Get(ctx context.Context, id string) (*Workload, error) {
	var w Workload
	if err := c.do(ctx, http.MethodGet, "/v1/workloads/"+id, nil, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// Stop stops and removes a workload.
func (c *Client) Stop(ctx context.Context, id string, graceSec int) error {
	return c.do(ctx, http.MethodDelete, "/v1/workloads/"+id+"?grace="+strconv.Itoa(graceSec), nil, nil)
}

// Logs returns the tail of a workload's output.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/workloads/"+id+"/logs?tail="+strconv.Itoa(tail), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("guest returned %s: %s", resp.Status, body)
	}
	return string(body), nil
}

// SelfTest runs the isolation matrix inside the guest and returns its report.
func (c *Client) SelfTest(ctx context.Context, vantage string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/selftest?context="+vantage, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("guest returned %s: %s", resp.Status, body)
	}
	return string(body), nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e Error
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			return fmt.Errorf("guest: %s", e.Message)
		}
		return fmt.Errorf("guest returned %s", resp.Status)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	// The host is the only name that matters; the connection is already
	// bound to one VM by the dialer.
	req, err := http.NewRequestWithContext(ctx, method, "http://guest"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(TokenHeader, c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
