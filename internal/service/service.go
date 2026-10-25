// Package service manages brout's per-user macOS LaunchAgent.
package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/template"
)

const Label = "io.github.victorarias.brout"

// Agent manages one LaunchAgent per macOS account. HOME selects its files,
// but launchd's gui domain belongs to the account, regardless of HOME.
type Agent struct {
	Path   string
	Home   string
	domain string
}

func New() (Agent, error) {
	if runtime.GOOS != "darwin" {
		return Agent{}, errors.New("brout service requires macOS; run brout serve in the foreground on this platform")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Agent{}, err
	}
	return Agent{
		Path:   filepath.Join(home, "Library", "LaunchAgents", Label+".plist"),
		Home:   home,
		domain: fmt.Sprintf("gui/%d", os.Getuid()),
	}, nil
}

// Loaded checks registration with launchd. It does not imply HTTP readiness.
func (a Agent) Loaded(ctx context.Context) (bool, error) {
	s, err := a.Status(ctx)
	return s.Loaded, err
}

// Status keeps registration separate from optional launchd diagnostics.
// launchctl print is human output, so missing or changed fields stay unknown.
type Status struct {
	Loaded   bool
	State    string
	PID      int // zero when unknown
	LastExit string
}

func (a Agent) Status(ctx context.Context) (Status, error) {
	out, err := a.launchctl(ctx, "print", a.target())
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 113 {
		// launchctl print returns 113 when the service is not registered.
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	s := Status{Loaded: true}
	for _, line := range strings.Split(string(out), "\n") {
		// Nested coalitions also have a state. Only read top-level fields
		// in the observed print format; do not guess when the format changes.
		if value, ok := strings.CutPrefix(line, "\tstate = "); ok {
			s.State = strings.TrimSpace(value)
		} else if value, ok := strings.CutPrefix(line, "\tpid = "); ok {
			if pid, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && pid > 0 {
				s.PID = pid
			}
		} else if value, ok := strings.CutPrefix(line, "\tlast exit code = "); ok {
			s.LastExit = strings.TrimSpace(value)
		}
	}
	return s, nil
}

// Install writes the plist and loads it. An existing registration must be
// uninstalled first, including when another HOME owns its files.
func (a Agent) Install(ctx context.Context, configPath, stateDir string) error {
	loaded, err := a.Loaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		return errors.New("brout service is already loaded; run brout service uninstall from its installing HOME first")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	var body bytes.Buffer
	err = plistTemplate.Execute(&body, struct {
		Label, Executable, Config, Home, State, Stdout, Stderr string
	}{Label, executable, configPath, a.Home, os.Getenv("XDG_STATE_HOME"),
		filepath.Join(stateDir, "service.stdout.log"), filepath.Join(stateDir, "service.stderr.log")})
	if err != nil {
		return err
	}
	if err := os.WriteFile(a.Path, body.Bytes(), 0o600); err != nil {
		return err
	}
	if _, err := a.launchctl(ctx, "bootstrap", a.domain, a.Path); err != nil {
		if cleanup := os.Remove(a.Path); cleanup != nil {
			return errors.Join(err, fmt.Errorf("remove failed install plist %s: %w", a.Path, cleanup))
		}
		return err
	}
	return nil
}

// Uninstall waits for launchd to stop the process before deleting its plist.
// A failed bootout leaves the plist available for a later cleanup attempt.
func (a Agent) Uninstall(ctx context.Context) error {
	loaded, err := a.Loaded(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(a.Path); errors.Is(err, os.ErrNotExist) {
		if loaded {
			return fmt.Errorf("brout service is still loaded, but its plist is missing at %s; uninstall from the installing HOME or restore that plist before retrying", a.Path)
		}
		return nil
	} else if err != nil {
		return err
	}
	if loaded {
		if _, err := a.launchctl(ctx, "bootout", "--wait", a.target()); err != nil {
			return err
		}
	}
	return os.Remove(a.Path)
}

// Definition holds the installed paths, independent of the config's validity.
type Definition struct {
	Args   []string
	Stderr string
}

// Read reads direct children of the plist's root dictionary, so nested
// environment strings cannot be mistaken for ProgramArguments or log paths.
func (a Agent) Read() (Definition, error) {
	body, err := os.ReadFile(a.Path)
	if err != nil {
		return Definition{}, err
	}
	var p struct {
		Dict struct {
			Values []struct {
				XMLName xml.Name
				Text    string   `xml:",chardata"`
				Strings []string `xml:"string"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(body, &p); err != nil {
		return Definition{}, fmt.Errorf("read %s: %w", a.Path, err)
	}
	var def Definition
	for i := 0; i+1 < len(p.Dict.Values); i++ {
		key, value := p.Dict.Values[i], p.Dict.Values[i+1]
		if key.XMLName.Local != "key" {
			continue
		}
		switch key.Text {
		case "ProgramArguments":
			def.Args = value.Strings
		case "StandardErrorPath":
			def.Stderr = value.Text
		}
	}
	return def, nil
}

func (d Definition) ConfigPath() (string, error) {
	if len(d.Args) != 4 || d.Args[1] != "serve" || d.Args[2] != "--config" {
		return "", errors.New("installed plist has no brout serve --config arguments")
	}
	return d.Args[3], nil
}

func (a Agent) target() string { return a.domain + "/" + Label }

func (a Agent) launchctl(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("launchctl %v: %w: %s", args, err, bytes.TrimSpace(out))
	}
	return out, nil
}

var plistTemplate = template.Must(template.New("LaunchAgent").Funcs(template.FuncMap{
	"xml": func(s string) (string, error) {
		var b bytes.Buffer
		err := xml.EscapeText(&b, []byte(s))
		return b.String(), err
	},
}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{xml .Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{xml .Executable}}</string>
    <string>serve</string>
    <string>--config</string>
    <string>{{xml .Config}}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>{{xml .Home}}</string>{{if .State}}
    <key>XDG_STATE_HOME</key><string>{{xml .State}}</string>{{end}}
  </dict>
  <key>WorkingDirectory</key><string>{{xml .Home}}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>{{xml .Stdout}}</string>
  <key>StandardErrorPath</key><string>{{xml .Stderr}}</string>
</dict>
</plist>
`))
