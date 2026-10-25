package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

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
		return serviceStatus(ctx, a, stdout)
	}
	return nil
}

func serviceStatus(ctx context.Context, a service.Agent, stdout io.Writer) error {
	s, err := a.Status(ctx)
	if err != nil {
		return err
	}
	if !s.Loaded {
		fmt.Fprintln(stdout, "not loaded")
		return nil
	}
	if s.State == "" {
		fmt.Fprintln(stdout, "loaded (state unknown)")
	} else {
		fmt.Fprintf(stdout, "loaded %s\nstate: %s\n", service.Label, s.State)
	}
	pid := "unknown"
	if s.PID > 0 {
		pid = strconv.Itoa(s.PID)
	}
	lastExit := s.LastExit
	if lastExit == "" {
		lastExit = "unknown"
	}
	fmt.Fprintf(stdout, "pid: %s\nlast exit code: %s\n", pid, lastExit)
	def, err := a.Read()
	if err != nil {
		return err
	}
	if def.Stderr != "" {
		fmt.Fprintf(stdout, "stderr: %s\n", def.Stderr)
		if code, err := strconv.Atoi(s.LastExit); err == nil && code != 0 {
			fmt.Fprintf(stdout, "last exit was nonzero; see %s\n", def.Stderr)
		}
	}
	var problems []error
	if len(def.Args) == 0 {
		problems = append(problems, errors.New("installed plist has no executable in ProgramArguments"))
	} else {
		binary := def.Args[0]
		fmt.Fprintf(stdout, "binary: %s\n", binary)
		if info, err := os.Stat(binary); err != nil {
			problems = append(problems, fmt.Errorf("installed binary %s: %w", binary, err))
		} else if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			problems = append(problems, fmt.Errorf("installed binary %s is not executable", binary))
		}
	}
	path, err := def.ConfigPath()
	if err != nil {
		problems = append(problems, err)
	} else {
		fmt.Fprintf(stdout, "config: %s\n", path)
		if cfg, err := config.LoadClient(path); err != nil {
			problems = append(problems, err)
		} else {
			fmt.Fprintf(stdout, "configured address: http://%s\n", cfg.Listen)
		}
	}
	return errors.Join(problems...)
}
