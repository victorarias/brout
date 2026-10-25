package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// launchctl is the external service boundary. The fake records registration
// and validates the real plist with macOS's plutil, without touching launchd.
func serviceHome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("LaunchAgent commands require macOS")
	}
	home := filepath.Join(t.TempDir(), "home & spaces")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fake := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$BROUT_TEST_LAUNCHCTL/calls"
case "$1" in
  print)
    if [ "${BROUT_TEST_PRINT_FAIL:-}" = yes ]; then echo 'permission denied' >&2; exit 1; fi
    if [ -f "$BROUT_TEST_LAUNCHCTL/loaded" ]; then exit 0; fi
    echo 'Could not find service' >&2
    exit 113
    ;;
  bootstrap)
    /usr/bin/plutil -lint "$3"
    /bin/cp "$3" "$BROUT_TEST_LAUNCHCTL/agent.plist"
    /usr/bin/touch "$BROUT_TEST_LAUNCHCTL/loaded"
    ;;
  bootout)
    if [ "${BROUT_TEST_BOOTOUT_FAIL:-}" = yes ]; then echo 'bootout failed' >&2; exit 1; fi
    test "$2" = --wait
    /bin/rm "$BROUT_TEST_LAUNCHCTL/loaded"
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state & spaces"))
	t.Setenv("BROUT_TEST_LAUNCHCTL", bin)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return home
}

func TestServiceLifecycle(t *testing.T) {
	home := serviceHome(t)
	path := writeConfig(t, `listen = "127.0.0.1:18979"
routing = ["typesafe/jev-latest"]
[providers.typesafe]
api_key = "must-not-appear-in-plist"
`)
	ctx := context.Background()
	var out bytes.Buffer
	command := func(args ...string) error {
		out.Reset()
		return run(ctx, append([]string{"service"}, args...), &out, io.Discard, nil)
	}
	if err := command("status"); err != nil || out.String() != "not loaded\n" {
		t.Fatalf("status: %q, %v", &out, err)
	}
	// Installation and status must retain a custom path even from another cwd.
	t.Chdir(filepath.Dir(path))
	if err := command("install", "--config", "config.toml"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:18979") {
		t.Fatalf("install: %s", &out)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "io.github.victorarias.brout.plist")
	body, err := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", plistPath).Output()
	if err != nil {
		t.Fatal(err)
	}
	var agent struct {
		Label                              string
		ProgramArguments                   []string
		EnvironmentVariables               map[string]string
		RunAtLoad, KeepAlive               bool
		StandardOutPath, StandardErrorPath string
	}
	if err := json.Unmarshal(body, &agent); err != nil {
		t.Fatal(err)
	}
	if len(agent.ProgramArguments) != 4 || !filepath.IsAbs(agent.ProgramArguments[0]) ||
		agent.ProgramArguments[1] != "serve" || agent.ProgramArguments[2] != "--config" || agent.ProgramArguments[3] != path ||
		!agent.RunAtLoad || !agent.KeepAlive || agent.EnvironmentVariables["HOME"] != home ||
		agent.EnvironmentVariables["XDG_STATE_HOME"] != os.Getenv("XDG_STATE_HOME") ||
		strings.Contains(string(body), "must-not-appear-in-plist") {
		t.Fatalf("plist: %s", body)
	}
	for _, logPath := range []string{agent.StandardOutPath, agent.StandardErrorPath} {
		if filepath.Dir(logPath) != filepath.Join(os.Getenv("XDG_STATE_HOME"), "brout") {
			t.Fatalf("service log: %s", logPath)
		}
	}
	// A second HOME shares launchd, so it must not replace a loaded agent.
	otherHome := t.TempDir()
	t.Setenv("HOME", otherHome)
	if err := command("install", "--config", path); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("second install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherHome, "Library")); !os.IsNotExist(err) {
		t.Fatalf("second install wrote files: %v", err)
	}
	t.Setenv("HOME", home)
	t.Chdir(home)
	if err := command("status"); err != nil || !strings.Contains(out.String(), "http://127.0.0.1:18979") || !strings.Contains(out.String(), path) {
		t.Fatalf("status: %q, %v", &out, err)
	}
	t.Setenv("BROUT_TEST_BOOTOUT_FAIL", "yes")
	if err := command("uninstall"); err == nil {
		t.Fatal("uninstall hid bootout failure")
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("failed bootout lost plist: %v", err)
	}
	t.Setenv("BROUT_TEST_BOOTOUT_FAIL", "")
	if err := command("uninstall"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("uninstall left plist: %v", err)
	}
	if err := command("status"); err != nil || out.String() != "not loaded\n" {
		t.Fatalf("status after uninstall: %q, %v", &out, err)
	}
	if err := command("uninstall"); err != nil {
		t.Fatalf("repeat uninstall: %v", err)
	}
}

func TestServiceInstallRejectsShellKeys(t *testing.T) {
	serviceHome(t)
	t.Setenv("BROUT_TEST_KEY", "available-in-this-shell")
	path := writeConfig(t, `[providers.typesafe]
api_key_env = "BROUT_TEST_KEY"
`)
	err := run(context.Background(), []string{"service", "install", "--config", path}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "providers.typesafe.api_key_env names BROUT_TEST_KEY") ||
		!strings.Contains(err.Error(), "launchd does not inherit your shell environment") || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("BROUT_TEST_LAUNCHCTL"), "calls")); !os.IsNotExist(err) {
		t.Fatalf("invalid config reached launchctl: %v", err)
	}
}

func TestServiceLaunchctlErrorsAreVisible(t *testing.T) {
	serviceHome(t)
	t.Setenv("BROUT_TEST_PRINT_FAIL", "yes")
	err := run(context.Background(), []string{"service", "status"}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("status hid error: %v", err)
	}
}

func TestServicePlatform(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-macOS behavior")
	}
	for _, cmd := range []string{"install", "uninstall", "status"} {
		err := run(context.Background(), []string{"service", cmd}, io.Discard, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "requires macOS") {
			t.Fatalf("service %s: %v", cmd, err)
		}
	}
}
