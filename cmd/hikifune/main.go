// Command hikifune is the server: it will hold the schedule, the web UI, the
// notifications and the state. For now it only answers /healthz.
//
//	hikifune              run the server (configured through HIKIFUNE_* variables)
//	hikifune healthcheck  exit 0 if the local server answers /healthz
//	hikifune version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "time/tzdata" // TZ works in images without zoneinfo

	"github.com/dor1a/hikifune/internal/buildinfo"
)

const defaultListen = ":8869"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "version", "-version", "--version":
		fmt.Println(buildinfo.Version)
	default:
		err = fmt.Errorf("unknown command %q (use serve, healthcheck, or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hikifune:", err)
		os.Exit(1)
	}
}

func listenAddr() string {
	if v := os.Getenv("HIKIFUNE_LISTEN"); v != "" {
		return v
	}
	return defaultListen
}

func serve() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{
		Addr:              listenAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("hikifune listening", "addr", srv.Addr, "version", buildinfo.Version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck calls /healthz on the local server. It exists because the image
// has no shell or curl for a HEALTHCHECK to use.
func healthcheck() error {
	host, port, err := net.SplitHostPort(listenAddr())
	if err != nil {
		return fmt.Errorf("HIKIFUNE_LISTEN: %w", err)
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
