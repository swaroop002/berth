// Command forge is the platform control plane (serve) and its CLI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/swaroop002/forge/internal/api"
	"github.com/swaroop002/forge/internal/provider"
	"github.com/swaroop002/forge/internal/reconcile"
	"github.com/swaroop002/forge/internal/spec"
	"github.com/swaroop002/forge/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "apply":
		err = apply(os.Args[2:])
	case "get":
		err = get(os.Args[2:])
	case "delete":
		err = del(os.Args[2:])
	case "events":
		err = events(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "version":
		fmt.Println("forge 0.1.0-dev")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `forge — environment control plane

  forge serve    [--listen :8080] [--db ./forge.db] [--workers 4] [--resync 2m] [--api-token T]
                 [--fake-latency 300ms] [--fake-fail-every 0]
  forge apply    -f env.yaml                 create or update an environment
  forge get      [name] [-o yaml|json]       one environment, or a table of all
  forge delete   <name>                      tear down and remove
  forge events   <name> [--limit 50]         what happened to it
  forge status                               phases, pending changes, queue depth

  --server http://localhost:8080 and --token apply to every client command (or FORGE_SERVER / FORGE_TOKEN).`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "address to listen on")
	db := fs.String("db", "./forge.db", "state database file")
	workers := fs.Int("workers", 4, "concurrent reconcilers")
	resync := fs.Duration("resync", 2*time.Minute, "how often converged environments are re-checked for drift")
	token := fs.String("api-token", os.Getenv("FORGE_TOKEN"), "bearer token clients must send (empty = open, development only)")
	fakeLatency := fs.Duration("fake-latency", 200*time.Millisecond, "fake provider: latency per call")
	fakeFail := fs.Int("fake-fail-every", 0, "fake provider: every Nth call fails transiently")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	pv := provider.NewFake()
	pv.Latency, pv.FailEvery = *fakeLatency, *fakeFail

	ctrl := reconcile.New(st, pv, reconcile.Options{Workers: *workers, Resync: *resync, Logger: log,
		Hooks: reconcile.Hooks{Reconciled: func(name, result string, took time.Duration) {
			log.Info("reconciled", "env", name, "result", result, "took", took.Round(time.Millisecond))
		}}})
	if *token == "" {
		log.Warn("no --api-token: the API is open (development mode)")
	}
	srv := &http.Server{Addr: *listen, Handler: api.New(st, ctrl, *token).Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)
	go func() { errc <- ctrl.Run(ctx) }()
	go func() {
		log.Info("forge listening", "addr", *listen, "db", *db, "provider", pv.Name(), "workers", *workers)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil {
			return err
		}
	}
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdown)
	stop()
	return nil
}

// --- client ----------------------------------------------------------------------

type client struct {
	base, token string
	http        *http.Client
}

func newClient(fs *flag.FlagSet) *client {
	server := fs.String("server", envOr("FORGE_SERVER", "http://localhost:8080"), "forge API address")
	token := fs.String("token", os.Getenv("FORGE_TOKEN"), "bearer token")
	return &client{base: *server, token: *token, http: &http.Client{Timeout: 15 * time.Second}}
}

func (c *client) do(method, path string, body []byte, out any) (int, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		json.Unmarshal(b, &e)
		return resp.StatusCode, fmt.Errorf("%s (http %d)", strings.TrimSpace(e.Error), resp.StatusCode)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(b, out)
	}
	return resp.StatusCode, nil
}

func apply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	file := fs.String("f", "", "environment YAML or JSON file (- for stdin)")
	c := newClientLazy(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("apply needs -f <file>")
	}
	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	var env spec.Environment
	if err := yaml.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("parse %s: %w", *file, err)
	}
	body, _ := json.Marshal(env)
	var stored spec.Environment
	code, err := c().do(http.MethodPut, "/v1/environments/"+env.Metadata.Name, body, &stored)
	if err != nil {
		return err
	}
	verb := "configured"
	if code == http.StatusCreated {
		verb = "created"
	}
	fmt.Printf("environment/%s %s (generation %d)\n", stored.Metadata.Name, verb, stored.Metadata.Generation)
	return nil
}

func get(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	out := fs.String("o", "", "output: yaml | json (default: human)")
	team := fs.String("team", "", "filter the table by team")
	c := newClientLazy(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 1 {
		var env spec.Environment
		if _, err := c().do(http.MethodGet, "/v1/environments/"+fs.Arg(0), nil, &env); err != nil {
			return err
		}
		return render(&env, *out)
	}
	var list struct{ Items []*spec.Environment }
	q := ""
	if *team != "" {
		q = "?team=" + *team
	}
	if _, err := c().do(http.MethodGet, "/v1/environments"+q, nil, &list); err != nil {
		return err
	}
	if *out != "" {
		return render(list.Items, *out)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTEAM\tSIZE\tREGION\tPHASE\tREADY\tGEN\tAGE")
	for _, e := range list.Items {
		ready := "-"
		if cnd := e.Status.Condition("Ready"); cnd != nil {
			ready = cnd.Status
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\n", e.Metadata.Name, e.Metadata.Team, e.Spec.Size, e.Spec.Region,
			e.Status.Phase, ready, e.Status.ObservedGeneration, e.Metadata.Generation, age(e.Metadata.CreatedAt))
	}
	return tw.Flush()
}

func del(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	c := newClientLazy(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("delete needs exactly one name")
	}
	if _, err := c().do(http.MethodDelete, "/v1/environments/"+fs.Arg(0), nil, nil); err != nil {
		return err
	}
	fmt.Printf("environment/%s deletion requested; resources are torn down in reverse order\n", fs.Arg(0))
	return nil
}

func events(args []string) error {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "newest N events")
	c := newClientLazy(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("events needs exactly one name")
	}
	var list struct{ Items []store.Event }
	if _, err := c().do(http.MethodGet, fmt.Sprintf("/v1/environments/%s/events?limit=%d", fs.Arg(0), *limit), nil, &list); err != nil {
		return err
	}
	for _, ev := range list.Items {
		fmt.Printf("%s  %-14s %s\n", ev.At.Local().Format("15:04:05.000"), ev.Type, ev.Message)
	}
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	c := newClientLazy(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var out map[string]any
	if _, err := c().do(http.MethodGet, "/v1/status", nil, &out); err != nil {
		return err
	}
	return render(out, "yaml")
}

func newClientLazy(fs *flag.FlagSet) func() *client {
	server := fs.String("server", envOr("FORGE_SERVER", "http://localhost:8080"), "forge API address")
	token := fs.String("token", os.Getenv("FORGE_TOKEN"), "bearer token")
	return func() *client {
		return &client{base: *server, token: *token, http: &http.Client{Timeout: 15 * time.Second}}
	}
}

func render(v any, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case "yaml":
		b, err := yaml.Marshal(v)
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil
	default:
		if e, ok := v.(*spec.Environment); ok {
			fmt.Print(api.Describe(e))
			return nil
		}
		return render(v, "yaml")
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
