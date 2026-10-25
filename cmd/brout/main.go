// Command brout is a local gateway for decision-model APIs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/victorarias/brout/internal/config"
	"github.com/victorarias/brout/internal/provider"
	"github.com/victorarias/brout/internal/provider/openai"
	"github.com/victorarias/brout/internal/provider/systemone"
	"github.com/victorarias/brout/internal/router"
	"github.com/victorarias/brout/internal/server"
)

const usage = `usage: brout <command> [flags]

commands:
  serve    run the gateway (POST /v1/systemone, GET /v1/models)
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "brout:", err)
		os.Exit(1)
	}
}

// run runs a brout command. ready, when set, receives the address serve
// listens on.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(addr string)) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("no command")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stdout, stderr, ready)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(string)) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath(), "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		return err
	}

	srv := server.New(server.Options{
		Router: router.New(providers, cfg.Routing, cfg.AttemptTimeout, router.RealClock{}),
		Now:    time.Now,
		Stderr: stderr,
	})
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s (config listen): %w", cfg.Listen, err)
	}
	fmt.Fprintf(stdout, "brout listening on http://%s\n", ln.Addr())
	if ready != nil {
		ready(ln.Addr().String())
	}

	hs := &http.Server{Handler: srv}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// An in-flight request ends within one attempt timeout per routing model.
	grace := cfg.AttemptTimeout * time.Duration(len(cfg.Routing)+1)
	shutdown, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return hs.Shutdown(shutdown)
}

// buildProviders makes an adapter for each configured provider.
func buildProviders(cfg config.Config) (map[string]provider.Provider, error) {
	client := &http.Client{}
	providers := map[string]provider.Provider{}
	for _, name := range cfg.ProviderNames() {
		pc := cfg.Providers[name]
		var spec systemone.Spec
		switch name {
		case "typesafe":
			spec = systemone.TypeSafe
		case "subq":
			spec = systemone.SubQ
		case "openai":
			baseURL := openai.DefaultBaseURL
			if pc.BaseURL != "" {
				baseURL = pc.BaseURL
			}
			providers[name] = openai.New(baseURL, pc.APIKey, client)
			continue
		default:
			return nil, fmt.Errorf("config: [providers.%s] is not a provider brout knows; it knows typesafe, subq and openai", name)
		}
		if pc.BaseURL != "" {
			spec.BaseURL = pc.BaseURL
		}
		providers[name] = systemone.New(name, spec, pc.APIKey, client)
	}
	return providers, nil
}
