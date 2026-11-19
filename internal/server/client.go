// Package server runs the check and update runs: it dials every agent
// (R26), records what each run found and did (R31), and runs on a daily
// schedule (R6).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dor1a/hikifune/internal/protocol"
)

// agentAPI is what a run asks of one agent.
type agentAPI interface {
	Version(ctx context.Context) (protocol.Version, error)
	Check(ctx context.Context) (protocol.CheckReport, error)
	StartUpdate(ctx context.Context, req protocol.UpdateRequest) (protocol.Job, error)
	Job(ctx context.Context, id string) (protocol.Job, error)
	Prune(ctx context.Context) (protocol.Pruned, error)
}

// Client talks to one agent.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient returns a client for the agent at base (http://host:port) with
// its token. Redirects are not followed, so the token stays with the agent.
func NewClient(base, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("agent URL %q must be http://host:port or https://host:port", base)
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, hc: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// StatusError is an agent answer that is not a success.
type StatusError struct {
	Status int
	Body   protocol.Error
}

func (e *StatusError) Error() string {
	msg := e.Body.Error
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("agent answered %d: %s", e.Status, msg)
}

const maxAnswer = 32 << 20

// Limits per call, so an agent that stops answering cannot hold a run, and
// with it the schedule. A check asks every registry; a prune may take long
// on a host with many images.
var (
	shortCall = 30 * time.Second
	checkCall = 15 * time.Minute
	pruneCall = 10 * time.Minute
)

func (c *Client) call(ctx context.Context, limit time.Duration, method, path string, body, out any, want int) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("%s %s: %w", method, path, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return err
	}
	if resp.StatusCode != want {
		e := &StatusError{Status: resp.StatusCode}
		json.Unmarshal(b, &e.Body)
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}

func (c *Client) Version(ctx context.Context) (protocol.Version, error) {
	var v protocol.Version
	err := c.call(ctx, shortCall, http.MethodGet, protocol.PathVersion, nil, &v, http.StatusOK)
	return v, err
}

func (c *Client) Check(ctx context.Context) (protocol.CheckReport, error) {
	var r protocol.CheckReport
	err := c.call(ctx, checkCall, http.MethodPost, protocol.PathCheck, nil, &r, http.StatusOK)
	return r, err
}

func (c *Client) StartUpdate(ctx context.Context, req protocol.UpdateRequest) (protocol.Job, error) {
	var j protocol.Job
	err := c.call(ctx, shortCall, http.MethodPost, protocol.PathUpdates, req, &j, http.StatusAccepted)
	return j, err
}

func (c *Client) Job(ctx context.Context, id string) (protocol.Job, error) {
	var j protocol.Job
	err := c.call(ctx, shortCall, http.MethodGet, protocol.PathUpdates+"/"+url.PathEscape(id), nil, &j, http.StatusOK)
	return j, err
}

func (c *Client) Prune(ctx context.Context) (protocol.Pruned, error) {
	var p protocol.Pruned
	err := c.call(ctx, pruneCall, http.MethodPost, protocol.PathPrune, nil, &p, http.StatusOK)
	return p, err
}

// sleep waits d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
