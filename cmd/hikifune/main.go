// Command hikifune is the server: it dials every agent on a daily schedule
// to check and update containers, keeps the state, and serves the web UI.
// The notifications come later.
//
//	hikifune                         run the server (configured through HIKIFUNE_* variables)
//	hikifune agent add <name> <url>  add an agent and print the token to give it
//	hikifune agent list              list the agents
//	hikifune agent remove <name>     remove an agent
//	hikifune run check|update        run once now and print the record as JSON
//	hikifune healthcheck             exit 0 if the local server answers /healthz
//	hikifune version                 print the version
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	_ "time/tzdata" // TZ works in images without zoneinfo

	"github.com/dor1a/hikifune/internal/auth"
	"github.com/dor1a/hikifune/internal/buildinfo"
	"github.com/dor1a/hikifune/internal/notify"
	"github.com/dor1a/hikifune/internal/protocol"
	"github.com/dor1a/hikifune/internal/server"
	"github.com/dor1a/hikifune/internal/store"
	"github.com/dor1a/hikifune/internal/update"
	"github.com/dor1a/hikifune/internal/web"
)

const (
	defaultListen = ":7777"
	defaultData   = "/data"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "agent":
		err = agentCommand(args)
	case "run":
		err = runOnce(args)
	case "healthcheck":
		err = healthcheck()
	case "version", "-version", "--version":
		fmt.Println(buildinfo.Version)
	default:
		err = fmt.Errorf("unknown command %q (use serve, agent, run, healthcheck or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hikifune:", err)
		os.Exit(1)
	}
}

// env reads a HIKIFUNE_* variable, or def when it is empty.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func listenAddr() string { return env("HIKIFUNE_LISTEN", defaultListen) }

func openStore() (*store.JSON, error) {
	keep := store.DefaultKeepRuns
	if v := os.Getenv("HIKIFUNE_KEEP_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("HIKIFUNE_KEEP_RUNS=%q is not a positive number", v)
		}
		keep = n
	}
	return store.OpenJSON(env("HIKIFUNE_DATA", defaultData), keep)
}

func runner() (*server.Runner, error) {
	st, err := openStore()
	if err != nil {
		return nil, err
	}
	watch, err := time.ParseDuration(env("HIKIFUNE_WATCH", update.DefaultWatch.String()))
	// The agent is sent whole seconds; anything else would be cut, and 0
	// would leave the agent's own default in its place.
	if err != nil || watch < time.Second || watch%time.Second != 0 {
		return nil, fmt.Errorf("HIKIFUNE_WATCH=%q is not a whole number of seconds of at least 1s, such as 2m or 90s", os.Getenv("HIKIFUNE_WATCH"))
	}
	r := &server.Runner{Store: st, Version: buildinfo.Version, Watch: watch}
	n, err := notifier(st)
	if err != nil {
		return nil, err
	}
	if n != nil {
		r.Notify = func(ctx context.Context, run store.Run) {
			if err := n.Run(ctx, run); err != nil {
				slog.Error("notification failed", "run", run.ID, "err", err)
			}
		}
	}
	return r, nil
}

// notifier reads the Discord settings; no webhook URL means no
// notifications.
func notifier(st store.Store) (*notify.Notifier, error) {
	hook := os.Getenv("HIKIFUNE_DISCORD_WEBHOOK_URL")
	if hook == "" {
		return nil, nil
	}
	if err := notify.CheckWebhook(hook); err != nil {
		return nil, fmt.Errorf("HIKIFUNE_DISCORD_WEBHOOK_URL: %w", err)
	}
	avatar := os.Getenv("HIKIFUNE_DISCORD_AVATAR_URL")
	if avatar != "" {
		if u, err := url.Parse(avatar); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("HIKIFUNE_DISCORD_AVATAR_URL=%q is not an http or https URL", avatar)
		}
	}
	return &notify.Notifier{
		Discord: &notify.Discord{URL: hook, Avatar: avatar},
		Template: func() string {
			s, err := st.Read()
			if err != nil {
				return ""
			}
			return s.Settings.Template
		},
		PerHost: func() bool {
			s, err := st.Read()
			return err == nil && s.Settings.MessagePerHost
		},
		Log: slog.Default(),
	}, nil
}

func serve() error {
	r, err := runner()
	if err != nil {
		return err
	}
	r.NotifyAfter = true
	loc := time.Local
	if tz := os.Getenv("HIKIFUNE_TIMEZONE"); tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return fmt.Errorf("HIKIFUNE_TIMEZONE: %w", err)
		}
	}
	checkAt, err := server.ParseClock(env("HIKIFUNE_CHECK_AT", "00:00"))
	if err != nil {
		return fmt.Errorf("HIKIFUNE_CHECK_AT: %w", err)
	}
	updateAt, err := server.ParseClock(env("HIKIFUNE_UPDATE_AT", "01:00"))
	if err != nil {
		return fmt.Errorf("HIKIFUNE_UPDATE_AT: %w", err)
	}
	checkEvery, err := server.ParseEvery(env("HIKIFUNE_CHECK_EVERY", "day"))
	if err != nil {
		return fmt.Errorf("HIKIFUNE_CHECK_EVERY: %w", err)
	}
	updateEvery, err := server.ParseEvery(env("HIKIFUNE_UPDATE_EVERY", "day"))
	if err != nil {
		return fmt.Errorf("HIKIFUNE_UPDATE_EVERY: %w", err)
	}
	n, err := notifier(r.Store)
	if err != nil {
		return err
	}
	authSettings, err := loadAuth(env)
	if err != nil {
		return err
	}
	gravatar, err := loadGravatar(env)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sched := &server.Schedule{Runner: r, CheckAt: checkAt, UpdateAt: updateAt, CheckEvery: checkEvery, UpdateEvery: updateEvery,
		Location: loc, Wake: make(chan struct{}, 1)}
	cfg := web.Config{Store: r.Store, Runner: r, Schedule: sched, Wake: sched.Wake, Location: loc,
		DefaultCheck:  server.When{Every: checkEvery, At: checkAt},
		DefaultUpdate: server.When{Every: updateEvery, At: updateAt},
		Watch:         r.Watch, Version: buildinfo.Version, Notifier: n, Gravatar: gravatar,
		Base: ctx, Log: slog.Default(),
		Status: func(ctx context.Context, a store.Agent) (protocol.Version, error) {
			c, err := server.NewClient(a.URL, a.Token)
			if err != nil {
				return protocol.Version{}, err
			}
			return c.Version(ctx)
		}}
	if j, ok := r.Store.(*store.JSON); ok {
		cfg.KeepRuns = j.KeepRuns()
	}
	if authSettings != nil {
		cfg.Auth = auth.New(*authSettings, slog.Default())
	} else {
		slog.Warn("HIKIFUNE_AUTH=none: the web UI has no sign-in; listen only where the proxy in front reaches it")
	}
	pages, err := web.New(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listenAddr(), Handler: pages.Handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sched.Loop(ctx)
	}()

	errc := make(chan error, 1)
	go func() {
		slog.Info("hikifune listening", "addr", srv.Addr, "version", buildinfo.Version,
			"check_at", checkAt.String(), "update_at", updateAt.String(), "timezone", loc.String())
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		stop()
		<-done
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return err
	}
	<-done
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// agentName is what an agent may be called. The name goes into the web UI's
// paths, so it has no slash or other character a path would need escaped.
var agentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func agentCommand(args []string) error {
	const usage = "use agent add <name> <url>, agent list or agent remove <name>"
	if len(args) == 0 {
		return errors.New(usage)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	switch {
	case args[0] == "add" && len(args) == 3:
		name, url := args[1], args[2]
		if !agentName.MatchString(name) {
			return fmt.Errorf("agent name %q: use up to 63 letters, digits, dots, underscores and hyphens, starting with a letter or digit", name)
		}
		if _, err := server.NewClient(url, ""); err != nil {
			return err
		}
		b := make([]byte, 40)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		token, id := hex.EncodeToString(b[:32]), hex.EncodeToString(b[32:])
		err := st.Update(func(s *store.State) error {
			for _, a := range s.Agents {
				if a.Name == name {
					return fmt.Errorf("an agent named %s exists; remove it first", name)
				}
			}
			s.Agents = append(s.Agents, store.Agent{ID: id, Name: name, URL: url, Token: token, Added: time.Now().UTC()})
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("added %s at %s\nset this on the agent, shown once:\n\nHIKIFUNE_AGENT_TOKEN=%s\n", name, url, token)
		return nil
	case args[0] == "list" && len(args) == 1:
		s, err := st.Read()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tURL\tADDED")
		for _, a := range s.Agents {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Name, a.URL, a.Added.Format(time.RFC3339))
		}
		return tw.Flush()
	case args[0] == "remove" && len(args) == 2:
		return st.Update(func(s *store.State) error {
			for i, a := range s.Agents {
				if a.Name == args[1] {
					s.Agents = append(s.Agents[:i], s.Agents[i+1:]...)
					// Its failed digests go too: an agent added later under
					// the same name is another host.
					kept := s.Failed[:0:0]
					for _, f := range s.Failed {
						if f.Agent != args[1] {
							kept = append(kept, f)
						}
					}
					s.Failed = kept
					return nil
				}
			}
			return fmt.Errorf("no agent named %s", args[1])
		})
	}
	return errors.New(usage)
}

func runOnce(args []string) error {
	if len(args) != 1 || (args[0] != server.KindCheck && args[0] != server.KindUpdate) {
		return errors.New("use run check or run update")
	}
	r, err := runner()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run, err := r.Run(ctx, args[0])
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(run)
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
