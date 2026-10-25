// Package docker is a minimal, read-only Docker Engine API client used to
// look up container names, images, and labels.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"gpumon/agent/internal/clock"
)

// ErrNotFound means the daemon does not know the container.
var ErrNotFound = errors.New("container not found")

// Container is the subset of "docker inspect" that the agent uses.
type Container struct {
	ID     string
	Name   string
	Image  string
	Labels map[string]string
}

// Client looks up containers.
type Client interface {
	Inspect(ctx context.Context, id string) (*Container, error)
}

// HTTPClient talks to the Docker Engine API. It only ever issues GET requests.
type HTTPClient struct {
	hc *http.Client
}

// NewUnixClient returns a client for the daemon listening on socket.
func NewUnixClient(socket string) *HTTPClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	return NewClientWithTransport(tr)
}

// NewClientWithTransport returns a client using rt (tests use a fake).
func NewClientWithTransport(rt http.RoundTripper) *HTTPClient {
	return &HTTPClient{hc: &http.Client{Transport: rt, Timeout: 2 * time.Second}}
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// get performs a read-only GET request. No other HTTP method is used by this package.
func (c *HTTPClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("docker API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker API returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// Inspect returns the container with the given ID.
func (c *HTTPClient) Inspect(ctx context.Context, id string) (*Container, error) {
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid container id")
	}
	var raw struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := c.get(ctx, "/containers/"+id+"/json", &raw); err != nil {
		return nil, err
	}
	return &Container{
		ID:     raw.ID,
		Name:   strings.TrimPrefix(raw.Name, "/"),
		Image:  raw.Config.Image,
		Labels: raw.Config.Labels,
	}, nil
}

// Cached wraps a Client with a small time-based cache. Failures are cached
// for a shorter time so that an unreachable daemon is not hammered once per
// process.
type Cached struct {
	Inner   Client
	Clock   clock.Clock
	TTL     time.Duration
	FailTTL time.Duration

	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	c   *Container
	err error
	at  time.Time
}

// NewCached returns a caching client.
func NewCached(inner Client, c clock.Clock) *Cached {
	return &Cached{Inner: inner, Clock: c, TTL: 5 * time.Minute, FailTTL: 20 * time.Second, m: map[string]cacheEntry{}}
}

// Inspect returns a cached or fresh lookup.
func (c *Cached) Inspect(ctx context.Context, id string) (*Container, error) {
	now := c.Clock.Now()
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok {
		ttl := c.TTL
		if e.err != nil {
			ttl = c.FailTTL
		}
		if now.Sub(e.at) < ttl {
			return e.c, e.err
		}
	}
	ct, err := c.Inner.Inspect(ctx, id)
	c.mu.Lock()
	if len(c.m) > 4096 {
		c.m = map[string]cacheEntry{}
	}
	c.m[id] = cacheEntry{c: ct, err: err, at: now}
	c.mu.Unlock()
	return ct, err
}

// Fake is an in-memory Client for tests and simulation.
type Fake struct {
	Containers map[string]*Container
	Err        error // when set, every call fails (daemon unreachable)
}

// Inspect looks the container up in the map. IDs may be given in full or
// abbreviated form.
func (f *Fake) Inspect(_ context.Context, id string) (*Container, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	for k, c := range f.Containers {
		if k == id || strings.HasPrefix(k, id) {
			return c, nil
		}
	}
	return nil, ErrNotFound
}
