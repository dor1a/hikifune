// Package docker is a small Docker Engine API client written on net/http.
// Every versioned request asks for APIVersion, so the same calls behave the
// same on every daemon hikifune supports.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the Engine API version of every versioned request. 1.43 is
// Docker 24, the oldest daemon hikifune supports, and newer daemons still
// serve it.
const APIVersion = "1.43"

// DefaultHost is the daemon address used when none is configured.
const DefaultHost = "unix:///var/run/docker.sock"

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 64 << 10

// Client talks to one Docker daemon.
type Client struct {
	hc   *http.Client
	base string // scheme and host used in request URLs
	host string // the address as configured, for messages
}

// New returns a client for a daemon at host: unix:///path/to/docker.sock, or
// tcp://host[:port] (http://host[:port] is accepted too) for a socket proxy.
// A tcp address without a port uses 2375. TLS is not supported.
func New(host string) (*Client, error) {
	if host == "" {
		host = DefaultHost
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	c := &Client{host: host}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			return nil, fmt.Errorf("docker host %q: no socket path", host)
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}
		c.base = "http://docker"
	case "tcp", "http":
		if u.Host == "" {
			return nil, fmt.Errorf("docker host %q: no host name", host)
		}
		if u.Path != "" && u.Path != "/" {
			return nil, fmt.Errorf("docker host %q: a path is not supported", host)
		}
		hostport := u.Host
		if u.Port() == "" {
			hostport = net.JoinHostPort(u.Hostname(), "2375")
		}
		tr.DialContext = dialer.DialContext
		c.base = "http://" + hostport
	default:
		return nil, fmt.Errorf("docker host %q: scheme %q is not supported, use unix:// or tcp://", host, u.Scheme)
	}
	c.hc = &http.Client{Transport: tr}
	return c, nil
}

// Host is the daemon address the client was created with.
func (c *Client) Host() string { return c.host }

// Error is a response from the daemon with a status of 300 or more.
type Error struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("docker %s %s: %d %s: %s", e.Method, e.Path, e.Status, http.StatusText(e.Status), e.Message)
}

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// do sends a request for APIVersion and decodes a JSON response into out.
// out may be nil to discard the body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	return c.call(ctx, method, "/v"+APIVersion+path, query, body, out)
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("docker %s %s: encode body: %w", method, path, err)
		}
		rd = bytes.NewReader(b)
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: readMessage(resp.Body)}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("docker %s %s: read body: %w", method, path, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("docker %s %s: decode body: %w", method, path, err)
	}
	return nil
}

// readMessage returns the daemon's {"message": ...}, or the raw body when the
// response is not in that shape.
func readMessage(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		return m.Message
	}
	return strings.TrimSpace(string(b))
}

// Version is the daemon's answer to GET /version.
type Version struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
}

// ServerVersion asks for the daemon's version without an API version in the
// path, so a daemon that would refuse APIVersion still answers.
func (c *Client) ServerVersion(ctx context.Context) (Version, error) {
	var v Version
	err := c.call(ctx, http.MethodGet, "/version", nil, nil, &v)
	return v, err
}

// CheckAPI returns the daemon's version, with an error when the daemon cannot
// serve APIVersion.
func (c *Client) CheckAPI(ctx context.Context) (Version, error) {
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return v, err
	}
	cmp, err := CompareAPIVersions(v.APIVersion, APIVersion)
	if err != nil {
		return v, fmt.Errorf("Docker %s reports API version: %w", v.Version, err)
	}
	if cmp < 0 {
		return v, fmt.Errorf("Docker %s serves Engine API %s, older than the %s hikifune needs", v.Version, v.APIVersion, APIVersion)
	}
	if v.MinAPIVersion != "" {
		cmp, err := CompareAPIVersions(v.MinAPIVersion, APIVersion)
		if err != nil {
			return v, fmt.Errorf("Docker %s reports minimum API version: %w", v.Version, err)
		}
		if cmp > 0 {
			return v, fmt.Errorf("Docker %s no longer serves Engine API %s, the oldest it serves is %s", v.Version, APIVersion, v.MinAPIVersion)
		}
	}
	return v, nil
}

// CompareAPIVersions compares two "major.minor" API versions and returns -1,
// 0 or 1.
func CompareAPIVersions(a, b string) (int, error) {
	am, an, err := parseAPIVersion(a)
	if err != nil {
		return 0, err
	}
	bm, bn, err := parseAPIVersion(b)
	if err != nil {
		return 0, err
	}
	switch {
	case am != bm:
		return cmpInt(am, bm), nil
	default:
		return cmpInt(an, bn), nil
	}
}

func parseAPIVersion(s string) (int, int, error) {
	major, minor, ok := strings.Cut(s, ".")
	if !ok {
		return 0, 0, fmt.Errorf("API version %q is not major.minor", s)
	}
	m, err := strconv.Atoi(major)
	if err != nil {
		return 0, 0, fmt.Errorf("API version %q is not major.minor", s)
	}
	n, err := strconv.Atoi(minor)
	if err != nil {
		return 0, 0, fmt.Errorf("API version %q is not major.minor", s)
	}
	return m, n, nil
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Ping checks that the daemon answers at APIVersion.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// Info is the part of GET /info that hikifune reads.
type Info struct {
	ID              string `json:"ID"`
	Name            string `json:"Name"`
	ServerVersion   string `json:"ServerVersion"`
	OperatingSystem string `json:"OperatingSystem"`
	Architecture    string `json:"Architecture"`
	Containers      int    `json:"Containers"`
	Images          int    `json:"Images"`
}

// Info returns facts about the daemon's host.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.do(ctx, http.MethodGet, "/info", nil, nil, &i)
	return i, err
}

// ContainerSummary is one entry of GET /containers/json.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Labels  map[string]string `json:"Labels"`
}

// ContainerList lists containers, stopped ones too when all is set.
func (c *Client) ContainerList(ctx context.Context, all bool) ([]ContainerSummary, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	var list []ContainerSummary
	err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &list)
	return list, err
}

// Container is the part of a container's inspect document that hikifune
// reads, plus the whole document.
type Container struct {
	ID     string          `json:"Id"`
	Name   string          `json:"Name"`
	Image  string          `json:"Image"`
	Config ContainerConfig `json:"Config"`
	State  ContainerState  `json:"State"`
	// Raw is the whole inspect document. Containers are recreated from it
	// as reported, so fields not named above must not be lost.
	Raw json.RawMessage `json:"-"`
}

// ContainerConfig is the part of an inspect document's Config that hikifune reads.
type ContainerConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// ContainerState is the part of an inspect document's State that hikifune reads.
type ContainerState struct {
	Status string  `json:"Status"`
	Health *Health `json:"Health"`
}

// Health is a container's health check state.
type Health struct {
	Status string `json:"Status"`
}

// ContainerInspect returns one container by ID or name.
func (c *Client) ContainerInspect(ctx context.Context, id string) (Container, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &raw); err != nil {
		return Container{}, err
	}
	var ct Container
	if err := json.Unmarshal(raw, &ct); err != nil {
		return Container{}, fmt.Errorf("docker inspect container %s: %w", id, err)
	}
	if ct.ID == "" {
		return Container{}, fmt.Errorf("docker inspect container %s: the answer has no Id", id)
	}
	ct.Raw = raw
	return ct, nil
}

// Image is the part of an image's inspect document that hikifune reads,
// plus the whole document.
type Image struct {
	ID          string          `json:"Id"`
	RepoTags    []string        `json:"RepoTags"`
	RepoDigests []string        `json:"RepoDigests"`
	Raw         json.RawMessage `json:"-"`
}

// ImageInspect returns one image by ID or reference.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &raw); err != nil {
		return Image{}, err
	}
	var img Image
	if err := json.Unmarshal(raw, &img); err != nil {
		return Image{}, fmt.Errorf("docker inspect image %s: %w", ref, err)
	}
	if img.ID == "" {
		return Image{}, fmt.Errorf("docker inspect image %s: the answer has no Id", ref)
	}
	img.Raw = raw
	return img, nil
}

// NetworkConnect connects a container to a network. endpoint is the
// EndpointSettings JSON as the container's inspect reported it, passed on
// unchanged, or nil for none.
func (c *Client) NetworkConnect(ctx context.Context, network, container string, endpoint json.RawMessage) error {
	body := struct {
		Container      string          `json:"Container"`
		EndpointConfig json.RawMessage `json:"EndpointConfig,omitempty"`
	}{Container: container, EndpointConfig: endpoint}
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}
