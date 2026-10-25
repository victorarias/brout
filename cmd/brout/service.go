package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/victorarias/brout/internal/config"
	"github.com/victorarias/brout/internal/service"
)

func serviceCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: brout service install [--config path] | uninstall | status")
	}
	fs := flag.NewFlagSet("service "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var configPath *string
	if args[0] == "install" {
		configPath = fs.String("config", config.DefaultPath(), "path to config.toml")
	} else if args[0] != "status" && args[0] != "uninstall" {
		return fmt.Errorf("unknown service command %q; use install, uninstall or status", args[0])
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("service %s takes no arguments", args[0])
	}
	a, err := service.New()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		cfg, err := config.LoadService(*configPath)
		if err != nil {
			return err
		}
		if _, err := buildProviders(cfg); err != nil {
			return err
		}
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			state = filepath.Join(a.Home, ".local", "state")
		}
		if err := a.Install(ctx, *configPath, filepath.Join(state, "brout")); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "loaded %s\nconfigured address: http://%s\nplist: %s\n", service.Label, cfg.Listen, a.Path)
	case "uninstall":
		if err := a.Uninstall(ctx); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "uninstalled %s\n", a.Path)
	case "status":
		loaded, err := a.Loaded(ctx)
		if err != nil {
			return err
		}
		if !loaded {
			fmt.Fprintln(stdout, "not loaded")
			return nil
		}
		fmt.Fprintln(stdout, "loaded "+service.Label)
		path, err := a.ConfigPath()
		if err != nil {
			return err
		}
		cfg, err := config.LoadClient(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "configured address: http://%s\nconfig: %s\n", cfg.Listen, path)
	}
	return nil
}
