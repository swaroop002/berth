// Command berth is the platform control plane (serve) and its CLI.
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

	"github.com/swaroop002/berth/internal/api"
	"github.com/swaroop002/berth/internal/auth"
	"github.com/swaroop002/berth/internal/cost"
	"github.com/swaroop002/berth/internal/gateway"
	"github.com/swaroop002/berth/internal/metrics"
	"github.com/swaroop002/berth/internal/provider"
	"github.com/swaroop002/berth/internal/reconcile"
	"github.com/swaroop002/berth/internal/spec"
	"github.com/swaroop002/berth/internal/store"
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
	case "costs":
		err = costs(os.Args[2:])
	case "version":
		fmt.Println("berth 0.2.0-dev")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `berth — environment control plane

  berth serve    [--listen :8080] [--gateway-listen :8081] [--db ./berth.db] [--workers 4] [--resync 2m]
                 [--admin-token T] [--team-token team=T ...] [--fake-latency 300ms] [--fake-fail-every 0]
  berth apply    -f env.yaml                 create or update an environment
  berth get      [name] [-o yaml|json]       one environment, or a table of all
  berth delete   <name>                      tear down and remove
  berth events   <name> [--limit 50]         what happened to it
  berth status                               phases, pending changes, queue depth, run rate
  berth costs                                what every environment costs, by team

  --server http://localhost:8080 and --token apply to every client command (or BERTH_SERVER / BERTH_TOKEN).`)
}

type tokenList []string

func (k *tokenList) String() string     { return strings.Join(*k, ",") }
func (k *tokenList) Set(v string) error { *k = append(*k, v); return nil }

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "API address")
	gwListen := fs.String("gateway-listen", ":8081", "route gateway address (empty = no gateway)")
	gwPublic := fs.String("gateway-public", "", "how clients reach the gateway (default http://localhost<gateway-listen>)")
	db := fs.String("db", "./berth.db", "state database file")
	workers := fs.Int("workers", 4, "concurrent reconcilers")
	resync := fs.Duration("resync", 2*time.Minute, "how often converged environments are re-checked for drift")
	admin := fs.String("admin-token", os.Getenv("BERTH_TOKEN"), "token that sees every team (empty with no team tokens = open, development only)")
	fs.StringVar(admin, "api-token", *admin, "alias of --admin-token")
	var teams tokenList
	fs.Var(&teams, "team-token", "team=token pair; repeatable. A team's token sees only that team's environments")
	fakeLatency := fs.Duration("fake-latency", 200*time.Millisecond, "fake provider: latency per call")
	fakeFail := fs.Int("fake-fail-every", 0, "fake provider: every Nth call fails transiently")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	resolver, err := auth.New(teams, *admin)
	if err != nil {
		return err
	}
	if resolver.Open() {
		log.Warn("no --admin-token or --team-token: the API and every route are open (development mode)")
	}
	st, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	m := metrics.New()

	fake := provider.NewFake()
	fake.Latency, fake.FailEvery = *fakeLatency, *fakeFail
	if n, err := fake.Persist(*db + ".fake-inventory.json"); err != nil {
		return err
	} else if n > 0 {
		log.Info("fake provider inventory restored", "resources", n)
	}
	var pv provider.Provider = fake
	var gw *gateway.Gateway
	var routes func() any
	if *gwListen != "" {
		if *gwPublic == "" {
			if strings.HasPrefix(*gwListen, ":") {
				*gwPublic = "http://localhost" + *gwListen
			} else {
				*gwPublic = "http://" + *gwListen
			}
		}
		gw = gateway.New(resolver, log)
		gw.OnRequest = m.GatewayRequest
		gp := gateway.Wrap(pv, gw, *gwPublic)
		if envs, err := st.List(); err == nil {
			if n := gp.Restore(envs); n > 0 {
				log.Info("gateway routes restored", "routes", n)
			}
		}
		pv = gp
		routes = func() any { return gw.Hosts() }
	}

	ctrl := reconcile.New(st, pv, reconcile.Options{Workers: *workers, Resync: *resync, Logger: log,
		Hooks: reconcile.Hooks{
			Reconciled: func(name, result string, took time.Duration) {
				m.Reconciled(name, result, took)
				log.Info("reconciled", "env", name, "result", result, "took", took.Round(time.Millisecond))
			},
			QueueDepth: func(n int) { m.QueueDepth.Set(float64(n)) },
		}})
	srv := &http.Server{Addr: *listen,
		Handler:           api.New(st, ctrl, api.Options{Auth: resolver, Rates: cost.Default, Metrics: m.Handler(), Routes: routes}).Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 3)
	go func() { errc <- ctrl.Run(ctx) }()
	go func() {
		log.Info("berth listening", "addr", *listen, "db", *db, "provider", pv.Name(), "workers", *workers)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	var gwSrv *http.Server
	if gw != nil {
		gwSrv = &http.Server{Addr: *gwListen, Handler: gw, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Info("gateway listening", "addr", *gwListen, "public", *gwPublic)
			if err := gwSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	go observe(ctx, st, m)

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
	if gwSrv != nil {
		gwSrv.Shutdown(shutdown)
	}
	stop()
	return nil
}

// observe refreshes the per-phase and per-team gauges every few seconds.
func observe(ctx context.Context, st *store.Store, m *metrics.Metrics) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		envs, err := st.List()
		if err == nil {
			phases := map[string]int{}
			for _, e := range envs {
				phases[string(e.Status.Phase)]++
			}
			m.Observe(phases, cost.Default.Build(envs, time.Now()).TeamHourly)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- client ----------------------------------------------------------------------

type client struct {
	base, token string
	http        *http.Client
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

func costs(args []string) error {
	fs := flag.NewFlagSet("costs", flag.ContinueOnError)
	out := fs.String("o", "", "output: yaml | json (default: table)")
	c := newClientLazy(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var rep cost.Report
	if _, err := c().do(http.MethodGet, "/v1/costs", nil, &rep); err != nil {
		return err
	}
	if *out != "" {
		return render(rep, *out)
	}
	fmt.Print(api.CostTable(rep))
	return nil
}

func newClientLazy(fs *flag.FlagSet) func() *client {
	server := fs.String("server", envOr("BERTH_SERVER", "http://localhost:8080"), "berth API address")
	token := fs.String("token", os.Getenv("BERTH_TOKEN"), "bearer token")
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
