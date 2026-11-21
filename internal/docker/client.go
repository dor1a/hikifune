// Package docker is a small Docker Engine API client written on net/http.
// Every versioned request asks for the daemon's own API version. A container
// is recreated from its inspect output, and an older version would hide or
// drop settings the daemon has: creating at API 1.43 drops a health check's
// start interval and makes read-only mounts non-recursive, and inspecting
// before 1.45 adds the container's short ID to its network aliases.
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
	"sync"
	"time"
)

// MinAPIVersion is the oldest Engine API hikifune supports: Docker 24.
const MinAPIVersion = "1.43"

// DefaultHost is the daemon address used when none is configured.
const DefaultHost = "unix:///var/run/docker.sock"

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 64 << 10

// Client talks to one Docker daemon.
type Client struct {
	hc   *http.Client
	base string // scheme and host used in request URLs
	host string // the address as configured, for messages

	mu  sync.Mutex
	api string // the daemon's API version, set by CheckAPI
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
	// The Engine API never redirects. Following one would send the
	// X-Registry-Auth of a pull to wherever it points.
	c.hc = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
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

// IsNotModified reports whether err is a 304 from the daemon, which start
// and stop answer when the container is already in that state.
func IsNotModified(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotModified
}

// IsConflict reports whether err is a 409 from the daemon, such as a name in
// use or an image a container still uses.
func IsConflict(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusConflict
}

// request is one call to the daemon.
type request struct {
	method string
	path   string // without the version prefix for versioned calls
	query  url.Values
	header http.Header
	body   any // encoded as JSON; a json.RawMessage is sent as it is
	out    any // the JSON response is decoded into it; nil discards the body
	// stream, when set, reads the response body instead of out.
	stream func(io.Reader) error
}

// do sends a request at the daemon's API version, asking for the version
// first when this client has not yet.
func (c *Client) do(ctx context.Context, r request) error {
	v, err := c.version(ctx)
	if err != nil {
		return err
	}
	r.path = "/v" + v + r.path
	return c.send(ctx, r)
}

func (c *Client) version(ctx context.Context) (string, error) {
	c.mu.Lock()
	v := c.api
	c.mu.Unlock()
	if v != "" {
		return v, nil
	}
	if _, err := c.CheckAPI(ctx); err != nil {
		return "", err
	}
	return c.APIVersion(), nil
}

// APIVersion is the API version this client asks for, empty until CheckAPI
// or the first versioned request has asked the daemon.
func (c *Client) APIVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.api
}

func (c *Client) send(ctx context.Context, r request) error {
	var rd io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("docker %s %s: encode body: %w", r.method, r.path, err)
		}
		rd = bytes.NewReader(b)
	}
	target := c.base + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, target, rd)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", r.method, r.path, err)
	}
	for k, vs := range r.header {
		req.Header[k] = vs
	}
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", r.method, r.path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &Error{Method: r.method, Path: r.path, Status: resp.StatusCode, Message: readMessage(resp.Body)}
	}
	if r.stream != nil {
		if err := r.stream(resp.Body); err != nil {
			return fmt.Errorf("docker %s %s: %w", r.method, r.path, err)
		}
		return nil
	}
	if r.out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("docker %s %s: read body: %w", r.method, r.path, err)
	}
	if err := json.Unmarshal(b, r.out); err != nil {
		return fmt.Errorf("docker %s %s: decode body: %w", r.method, r.path, err)
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
// path, so any daemon answers.
func (c *Client) ServerVersion(ctx context.Context) (Version, error) {
	var v Version
	err := c.send(ctx, request{method: http.MethodGet, path: "/version", out: &v})
	return v, err
}

// CheckAPI asks the daemon for its version. When the daemon serves
// MinAPIVersion or newer, later requests ask for the daemon's own API
// version; otherwise it returns an error and later requests fail the same way.
func (c *Client) CheckAPI(ctx context.Context) (Version, error) {
	v, err := c.ServerVersion(ctx)
	if err != nil {
		return v, err
	}
	cmp, err := CompareAPIVersions(v.APIVersion, MinAPIVersion)
	if err != nil {
		return v, fmt.Errorf("Docker %s reports API version: %w", v.Version, err)
	}
	if cmp < 0 {
		return v, fmt.Errorf("Docker %s serves Engine API %s, older than the %s hikifune needs", v.Version, v.APIVersion, MinAPIVersion)
	}
	c.mu.Lock()
	c.api = v.APIVersion
	c.mu.Unlock()
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

// Ping checks that the daemon answers at its API version.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodGet, path: "/_ping"})
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
	err := c.do(ctx, request{method: http.MethodGet, path: "/info", out: &i})
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
	err := c.do(ctx, request{method: http.MethodGet, path: "/containers/json", query: q, out: &list})
	return list, err
}

// Container is the part of a container's inspect document that hikifune
// reads, plus the whole document.
type Container struct {
	ID           string          `json:"Id"`
	Name         string          `json:"Name"`
	Image        string          `json:"Image"`
	RestartCount int             `json:"RestartCount"`
	Config       ContainerConfig `json:"Config"`
	HostConfig   HostConfig      `json:"HostConfig"`
	State        ContainerState  `json:"State"`
	// Raw is the whole inspect document. Containers are recreated from it
	// as reported, so fields not named above must not be lost.
	Raw json.RawMessage `json:"-"`
}

// ContainerConfig is the part of an inspect document's Config that hikifune reads.
type ContainerConfig struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

// HostConfig is the part of an inspect document's HostConfig that hikifune reads.
type HostConfig struct {
	AutoRemove bool `json:"AutoRemove"`
}

// ContainerState is the part of an inspect document's State that hikifune reads.
type ContainerState struct {
	Status     string  `json:"Status"`
	Running    bool    `json:"Running"`
	Restarting bool    `json:"Restarting"`
	OOMKilled  bool    `json:"OOMKilled"`
	ExitCode   int     `json:"ExitCode"`
	Error      string  `json:"Error"`
	StartedAt  string  `json:"StartedAt"`
	Health     *Health `json:"Health"`
}

// Health is a container's health check state.
type Health struct {
	Status        string      `json:"Status"`
	FailingStreak int         `json:"FailingStreak"`
	Log           []HealthLog `json:"Log"`
}

// HealthLog is one health check run.
type HealthLog struct {
	ExitCode int    `json:"ExitCode"`
	Output   string `json:"Output"`
}

// ContainerInspect returns one container by ID or name.
func (c *Client) ContainerInspect(ctx context.Context, id string) (Container, error) {
	var raw json.RawMessage
	if err := c.do(ctx, request{method: http.MethodGet, path: "/containers/" + url.PathEscape(id) + "/json", out: &raw}); err != nil {
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
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	// Os, Architecture and Variant are the image's platform.
	Os           string          `json:"Os"`
	Architecture string          `json:"Architecture"`
	Variant      string          `json:"Variant"`
	Raw          json.RawMessage `json:"-"`
}

// ImageInspect returns one image by ID or reference.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var raw json.RawMessage
	if err := c.do(ctx, request{method: http.MethodGet, path: "/images/" + url.PathEscape(ref) + "/json", out: &raw}); err != nil {
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

// platformAPI is the first Engine API version whose image inspect takes a
// platform query.
const platformAPI = "1.49"

// ImagePlatformID returns the ID image inspect gives for the daemon's own
// platform within ref. On the containerd image store that is the platform
// image, where a plain inspect gives the index; it is the value compose keeps
// in its com.docker.compose.image label. ok is false when the daemon serves
// an API older than 1.49, which has no platform query.
func (c *Client) ImagePlatformID(ctx context.Context, ref string) (id string, ok bool, err error) {
	v, err := c.version(ctx)
	if err != nil {
		return "", false, err
	}
	if cmp, err := CompareAPIVersions(v, platformAPI); err != nil || cmp < 0 {
		return "", false, err
	}
	sv, err := c.ServerVersion(ctx)
	if err != nil {
		return "", false, err
	}
	platform, err := json.Marshal(struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}{sv.Os, sv.Arch})
	if err != nil {
		return "", false, err
	}
	var out struct {
		ID string `json:"Id"`
	}
	err = c.do(ctx, request{method: http.MethodGet, path: "/images/" + url.PathEscape(ref) + "/json",
		query: url.Values{"platform": {string(platform)}}, out: &out})
	if err != nil {
		return "", false, err
	}
	if out.ID == "" {
		return "", false, fmt.Errorf("docker inspect image %s for %s/%s: the answer has no Id", ref, sv.Os, sv.Arch)
	}
	return out.ID, true, nil
}

// NetworkConnect connects a container to a network. endpoint is the
// EndpointSettings JSON, passed on unchanged, or nil for none.
func (c *Client) NetworkConnect(ctx context.Context, network, container string, endpoint json.RawMessage) error {
	body := struct {
		Container      string          `json:"Container"`
		EndpointConfig json.RawMessage `json:"EndpointConfig,omitempty"`
	}{Container: container, EndpointConfig: endpoint}
	return c.do(ctx, request{method: http.MethodPost, path: "/networks/" + url.PathEscape(network) + "/connect", body: body})
}

// Created is the answer to a container create.
type Created struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// ContainerCreate creates a container named name. body is the JSON the
// create endpoint takes, the Config fields with HostConfig and
// NetworkingConfig beside them, and is sent as it is.
func (c *Client) ContainerCreate(ctx context.Context, name string, body json.RawMessage) (Created, error) {
	var out Created
	err := c.do(ctx, request{method: http.MethodPost, path: "/containers/create",
		query: url.Values{"name": {name}}, body: body, out: &out})
	if err == nil && out.ID == "" {
		err = fmt.Errorf("docker create %s: the answer has no Id", name)
	}
	return out, err
}

// ContainerStart starts a container. One that is already running is not an error.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	err := c.do(ctx, request{method: http.MethodPost, path: "/containers/" + url.PathEscape(id) + "/start"})
	if IsNotModified(err) {
		return nil
	}
	return err
}

// ContainerStop stops a container with the stop signal and timeout it was
// created with. One that is already stopped is not an error.
func (c *Client) ContainerStop(ctx context.Context, id string) error {
	err := c.do(ctx, request{method: http.MethodPost, path: "/containers/" + url.PathEscape(id) + "/stop"})
	if IsNotModified(err) {
		return nil
	}
	return err
}

// ContainerRename gives a container a new name.
func (c *Client) ContainerRename(ctx context.Context, id, name string) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/containers/" + url.PathEscape(id) + "/rename",
		query: url.Values{"name": {name}}})
}

// ContainerRemove removes a container, a running one too when force is set.
// Its volumes are always kept: a recreated container mounts the same ones.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.do(ctx, request{method: http.MethodDelete, path: "/containers/" + url.PathEscape(id), query: q})
}

// ImagePull pulls repository:tag. auth is the X-Registry-Auth value, or
// empty to pull anonymously. The daemon reports a failure inside the stream
// it answers with, so the stream is read to the end.
func (c *Client) ImagePull(ctx context.Context, repository, tag, auth string) error {
	h := http.Header{}
	if auth != "" {
		h.Set("X-Registry-Auth", auth)
	}
	return c.do(ctx, request{method: http.MethodPost, path: "/images/create",
		query: url.Values{"fromImage": {repository}, "tag": {tag}}, header: h,
		stream: readPullStream})
}

func readPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read pull progress: %w", err)
		}
		if m.ErrorDetail.Message != "" {
			return errors.New(m.ErrorDetail.Message)
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}

// ImagesPrune removes dangling images, the ones no tag names and no
// container uses, and returns the space it reclaimed in bytes, the larger of
// what the daemon reports and how much the images' layers shrank. Docker 24
// reports 0 for what it removed; with the containerd store it reports the
// unpacked layers but not the downloaded blobs.
func (c *Client) ImagesPrune(ctx context.Context) (int64, error) {
	before, beforeErr := c.imageLayersSize(ctx)
	var out struct {
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	err := c.do(ctx, request{method: http.MethodPost, path: "/images/prune",
		query: url.Values{"filters": {`{"dangling":["true"]}`}}, out: &out})
	if err != nil {
		return 0, err
	}
	reclaimed := out.SpaceReclaimed
	if beforeErr == nil {
		if after, err := c.imageLayersSize(ctx); err == nil {
			reclaimed = max(reclaimed, before-after)
		}
	}
	return reclaimed, nil
}

// diskUsageLimit bounds each disk usage report, so a slow one cannot use up
// the time the prune itself was given.
var diskUsageLimit = time.Minute

// imageLayersSize is the size of the layers of all images, from the disk
// usage report limited to images (API 1.42).
func (c *Client) imageLayersSize(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, diskUsageLimit)
	defer cancel()
	var out struct {
		LayersSize *int64 `json:"LayersSize"`
	}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/system/df", query: url.Values{"type": {"image"}}, out: &out}); err != nil {
		return 0, err
	}
	// A report without it is not a size of 0.
	if out.LayersSize == nil {
		return 0, errors.New("the disk usage report has no LayersSize")
	}
	return *out.LayersSize, nil
}
