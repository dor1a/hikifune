// Command hikifune-agent runs on each Docker host and talks to its Docker
// Engine for the server, which dials it (R26).
//
//	hikifune-agent             serve the agent API (the default)
//	hikifune-agent inventory   print every container on the host as JSON (read only)
//	hikifune-agent check       ask each image's registry for updates and print them as JSON (read only)
//	hikifune-agent update [-watch 2m] <container>
//	                           recreate one running container on the image its reference names now,
//	                           roll it back if it fails the watch, and print the result as JSON
//	hikifune-agent healthcheck exit 0 if the local agent answers /healthz
//	hikifune-agent version     print the version
//
// The daemon is reached at HIKIFUNE_DOCKER_HOST (unix:///var/run/docker.sock
// by default, or tcp://host:port for a socket proxy). Registry credentials
// come from the Docker config.json at HIKIFUNE_DOCKER_CONFIG; without one,
// every registry is asked and pulled from anonymously. Registries listed in
// HIKIFUNE_INSECURE_REGISTRIES are asked over plain HTTP. The API listens on
// HIKIFUNE_AGENT_LISTEN and needs HIKIFUNE_AGENT_TOKEN.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dor1a/hikifune/internal/agent"
	"github.com/dor1a/hikifune/internal/buildinfo"
	"github.com/dor1a/hikifune/internal/docker"
	"github.com/dor1a/hikifune/internal/hostlock"
	"github.com/dor1a/hikifune/internal/inventory"
	"github.com/dor1a/hikifune/internal/registry"
	"github.com/dor1a/hikifune/internal/update"
)

const defaultListen = ":7778"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "inventory":
		err = printInventory()
	case "check":
		err = printChecks()
	case "update":
		err = runUpdate(os.Args[2:])
	case "healthcheck":
		err = healthcheck()
	case "version", "-version", "--version":
		fmt.Println(buildinfo.Version)
	default:
		err = fmt.Errorf("unknown command %q (use serve, inventory, check, update, healthcheck or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hikifune-agent:", err)
		os.Exit(1)
	}
}

func listenAddr() string {
	if v := os.Getenv("HIKIFUNE_AGENT_LISTEN"); v != "" {
		return v
	}
	return defaultListen
}

// clients builds the Docker client, the registry client and the keychain
// from the environment.
func clients() (*docker.Client, *registry.Client, *registry.Keychain, error) {
	c, err := docker.New(os.Getenv("HIKIFUNE_DOCKER_HOST"))
	if err != nil {
		return nil, nil, nil, err
	}
	keys, err := registry.LoadConfig(os.Getenv("HIKIFUNE_DOCKER_CONFIG"))
	if err != nil {
		return nil, nil, nil, err
	}
	var plain []string
	for _, r := range strings.Split(os.Getenv("HIKIFUNE_INSECURE_REGISTRIES"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			plain = append(plain, r)
		}
	}
	return c, registry.New(keys, nil).PlainHTTP(plain), keys, nil
}

// pullAuth gives the daemon the credentials for a pull, but none for a
// registry asked over plain HTTP.
func pullAuth(reg *registry.Client, keys *registry.Keychain) func(string) string {
	return func(r string) string {
		if reg.Plain(r) {
			return ""
		}
		return keys.PullAuth(r)
	}
}

func serve() error {
	d, reg, keys, err := clients()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := agent.New(ctx, agent.Config{Docker: d, Registry: reg, Auth: pullAuth(reg, keys),
		Token: os.Getenv("HIKIFUNE_AGENT_TOKEN"), LockPath: hostlock.Path})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listenAddr(), Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		slog.Info("hikifune-agent listening", "addr", srv.Addr, "version", buildinfo.Version, "docker", d.Host())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// A running update rolls back on the cancelled context; wait for it.
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	a.Wait()
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printInventory() error {
	c, err := docker.New(os.Getenv("HIKIFUNE_DOCKER_HOST"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := inventory.Collect(ctx, c)
	if err != nil {
		return err
	}
	return printJSON(rep)
}

func printChecks() error {
	d, reg, _, err := clients()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	rep, err := agent.Check(ctx, d, reg)
	if err != nil {
		return err
	}
	return printJSON(rep)
}

// runUpdate updates one container and prints the result. It exits non-zero
// when the update failed or was rolled back. An interrupt during the watch
// still rolls back.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	watch := fs.Duration("watch", update.DefaultWatch, "how long the new container must keep running (and stay healthy)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hikifune-agent update [-watch 2m] <container>")
	}
	d, reg, keys, err := clients()
	if err != nil {
		return err
	}
	// The agent's jobs hold the same lock, so run this with docker exec in
	// the agent's container to keep the two apart.
	lock, err := hostlock.Try(hostlock.Path)
	switch {
	case errors.Is(err, hostlock.ErrBusy):
		return errors.New("the agent is running a job on this host; try again when it ends")
	case err != nil:
		fmt.Fprintf(os.Stderr, "warning: cannot take the host lock (%s); the agent could start a job at the same time\n", hostlock.Hint(hostlock.Path, err))
	}
	defer lock.Release()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := update.Container(ctx, d, fs.Arg(0), update.Options{Watch: *watch, Auth: pullAuth(reg, keys), Check: reg.Check})
	if err := printJSON(res); err != nil {
		return err
	}
	if res.Outcome == update.Failed || res.Outcome == update.RolledBack {
		return fmt.Errorf("%s: %s", res.Outcome, res.Reason)
	}
	return nil
}

// healthcheck calls /healthz on the local agent, for the image's HEALTHCHECK.
func healthcheck() error {
	host, port, err := net.SplitHostPort(listenAddr())
	if err != nil {
		return fmt.Errorf("HIKIFUNE_AGENT_LISTEN: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", resp.Status)
	}
	return nil
}
