// Package client talks to semd's API. It has no dependency on the daemon's
// internals (no cgo, no engine), so the UI can use it; the wire types
// mirror the engine's, which internal/api's tests check.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
)

// SearchRequest is the body of POST /v1/search.
type SearchRequest struct {
	Q     string   `json:"q"`
	K     int      `json:"k,omitempty"` // default 10, max 100
	Roots []string `json:"roots,omitempty"`
	Kinds []string `json:"kinds,omitempty"`
}

// SearchResponse is the reply to POST /v1/search.
type SearchResponse struct {
	Hits   []Hit `json:"hits"`
	TookMS int64 `json:"took_ms"`
}

// Hit is one search result.
type Hit struct {
	Path    string       `json:"path"`
	Kind    string       `json:"kind"`
	Locator kind.Locator `json:"locator"`
	Score   float32      `json:"score"`
	Snippet string       `json:"snippet,omitempty"`
	Stale   bool         `json:"stale,omitempty"` // file changed since it was indexed
}

// RootsRequest is the body of PUT /v1/roots. A nil Excludes keeps the current ones.
type RootsRequest struct {
	Roots    []string `json:"roots"`
	Excludes []string `json:"excludes"`
}

// RootsResponse is the reply to GET/PUT /v1/roots.
type RootsResponse struct {
	Roots           []string `json:"roots"`
	Excludes        []string `json:"excludes"`
	DefaultExcludes []string `json:"default_excludes"`
}

// Settings is the body and reply of GET/PUT /v1/settings.
type Settings struct {
	PauseOnBattery bool `json:"pause_on_battery"`
}

// Status is the reply to GET /v1/status.
type Status struct {
	Indexed    int          `json:"indexed"`
	Pending    int          `json:"pending"`
	Failed     int          `json:"failed"`
	Scanning   bool         `json:"scanning"`
	Watch      string       `json:"watch"`
	Paused     []string     `json:"paused,omitempty"`
	Embedder   EmbedStatus  `json:"embedder"`
	Store      *StoreStats  `json:"store,omitempty"`
	StoreError string       `json:"store_error,omitempty"`
	Roots      []RootStatus `json:"roots"`
}

type EmbedStatus struct {
	Model     string `json:"model"`
	Precision string `json:"precision"`
	Dims      int    `json:"dims"`
}

type StoreStats struct {
	Segments int `json:"segments"`
	Records  int `json:"records"`
	Dead     int `json:"dead"`
	Files    int `json:"files"`
}

type RootStatus struct {
	Path  string `json:"path"`
	Error string `json:"error,omitempty"`
}

// Failure is a file or directory that could not be indexed.
type Failure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Error is the body of every non-2xx reply.
type Error struct {
	Error string `json:"error"`
}

// Client talks to semd over its Unix socket.
type Client struct {
	hc *http.Client
}

// NewClient returns a client for the socket at path.
func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// As in semd: connect reports only EINVAL for an overlong path.
			if len(socket) >= 104 {
				return nil, fmt.Errorf("socket path is %d bytes, the limit is 103: %s", len(socket), socket)
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{hc: &http.Client{Transport: tr}}
}

// HTTP returns the underlying client, for requests the typed methods cannot
// make (tests send malformed bodies with it). Hosts are ignored.
func (c *Client) HTTP() *http.Client { return c.hc }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://semd"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("semd unreachable: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e Error
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("semd: %s (HTTP %d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("semd: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func (c *Client) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	var r SearchResponse
	err := c.do(ctx, http.MethodPost, "/v1/search", req, &r)
	return r, err
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

func (c *Client) Roots(ctx context.Context) (RootsResponse, error) {
	var r RootsResponse
	err := c.do(ctx, http.MethodGet, "/v1/roots", nil, &r)
	return r, err
}

func (c *Client) SetRoots(ctx context.Context, req RootsRequest) (RootsResponse, error) {
	var r RootsResponse
	err := c.do(ctx, http.MethodPut, "/v1/roots", req, &r)
	return r, err
}

func (c *Client) Failures(ctx context.Context) ([]Failure, error) {
	var f []Failure
	err := c.do(ctx, http.MethodGet, "/v1/failures", nil, &f)
	return f, err
}

func (c *Client) Settings(ctx context.Context) (Settings, error) {
	var s Settings
	err := c.do(ctx, http.MethodGet, "/v1/settings", nil, &s)
	return s, err
}

func (c *Client) SetSettings(ctx context.Context, s Settings) (Settings, error) {
	var r Settings
	err := c.do(ctx, http.MethodPut, "/v1/settings", s, &r)
	return r, err
}

func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/pause", nil, nil)
}

func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/resume", nil, nil)
}

func (c *Client) Reindex(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/reindex", nil, nil)
}

// Events streams events until ctx ends or the connection drops. The first
// event is an "open" made here: semd has subscribed, so every later event
// reaches fn. A "lagged" event reports how many events semd had to drop.
func (c *Client) Events(ctx context.Context, fn func(events.Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://semd/v1/events", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("semd unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("semd: events: HTTP %d", resp.StatusCode)
	}
	fn(events.Event{Type: "open"})
	sc := bufio.NewScanner(resp.Body)
	typ := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			typ = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if typ == "lagged" {
				fn(events.Event{Type: "lagged", Msg: data})
				continue
			}
			var ev events.Event
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fmt.Errorf("semd: bad event: %w", err)
			}
			fn(ev)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}
