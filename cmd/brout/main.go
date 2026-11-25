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
	"github.com/victorarias/brout/internal/requestlog"
	"github.com/victorarias/brout/internal/router"
	"github.com/victorarias/brout/internal/server"
)

var version = "dev"

const usage = `usage: brout <command> [flags]

commands:
  version               print the brout version
  serve                 run the gateway (POST /v1/systemone, GET /v1/models)
  logs [--limit n]      list the newest requests in the request log
  logs show <id>        print one request log entry
  replay <id> [--model <model id>]
                        send a logged request through the running gateway again
  service install [--config path]
                        install and load a macOS LaunchAgent
  service uninstall     stop the LaunchAgent and remove its plist
  service status        show whether it is loaded and its configured address
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
	case "version":
		fmt.Fprintln(stdout, "brout "+version)
		return nil
	case "serve":
		return serve(ctx, args[1:], stdout, stderr, ready)
	case "logs":
		if len(args) > 1 && args[1] == "show" {
			return logsShow(args[2:], stdout, stderr)
		}
		return logs(args[1:], stdout, stderr)
	case "replay":
		return replay(ctx, args[1:], stdout, stderr)
	case "service":
		return serviceCommand(ctx, args[1:], stdout, stderr)
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
	providers := buildProviders(cfg)

	opts := server.Options{
		Router: router.New(providers, cfg.Routing, cfg.AttemptTimeout, router.RealClock{}),
		Now:    time.Now,
		Stderr: stderr,
	}
	if cfg.Log.Enabled {
		opts.Finish = requestlog.NewWriter(cfg.Log.Dir, stderr).Finish
	}
	srv := server.New(opts)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s (config listen): %w", cfg.Listen, err)
	}
	fmt.Fprintf(stdout, "brout listening on http://%s\n", ln.Addr())
	if cfg.Log.Enabled {
		fmt.Fprintf(stdout, "brout logging requests to %s\n", cfg.Log.Dir)
	}
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
func buildProviders(cfg config.Config) map[string]provider.Provider {
	client := &http.Client{}
	providers := map[string]provider.Provider{}
	for name, pc := range cfg.Providers {
		switch pc.Protocol {
		case config.OpenAI:
			baseURL := openai.DefaultBaseURL
			if pc.BaseURL != "" {
				baseURL = pc.BaseURL
			}
			providers[name] = openai.New(name, baseURL, pc.Headers, client)
		case config.SystemOne:
			var spec systemone.Spec // a custom provider: config requires its base_url
			switch name {
			case "typesafe":
				spec = systemone.TypeSafe
			case "subq":
				spec = systemone.SubQ
			}
			if pc.BaseURL != "" {
				spec.BaseURL = pc.BaseURL
			}
			providers[name] = systemone.New(name, spec, pc.Headers, client)
		}
	}
	return providers
}
