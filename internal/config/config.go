// Package config loads and checks brout's config.toml. It is the one place
// that resolves API keys (D14).
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultAttemptTimeout bounds one call to one provider. It must sit past
// measured provider latency and well under the TypeSafe SDK's 10 s attempt
// timeout (F5), or a hanging first attempt never reaches failover.
//
// Receipt (C3, 2026-10-25, scripts/latency.sh: typesafe/jev-latest answering
// as jev-1.14.0, 3 questions, 10 runs per row, curl's time_total):
//
//	                                  direct to TypeSafe   through brout
//	short state (370 tokens)          0.22 to 0.85 s       0.17 to 0.38 s
//	long state (30,581 tokens)        0.50 to 0.75 s       0.38 to 0.46 s
//
// jev takes at most about 32.8K input tokens, so the long state sits near
// the largest request TypeSafe answers. 4 s is about 5 times the slowest run
// and leaves 6 s of the SDK's 10 s for a second routing model.
//
// openai/gpt-6-luna through brout (C4, 2026-10-25, same script and runs):
// short state (402 tokens) 0.16 to 0.33 s; long state (29,085 tokens) 0.16
// to 0.43 s; 40 long states that were each unique, so OpenAI could not cache
// them, 0.24 to 0.72 s. The slowest call seen was the session's first, direct
// to OpenAI: 2.7 s for a short state. 4 s stays past it.
const DefaultAttemptTimeout = 4 * time.Second

const DefaultListen = "127.0.0.1:7979"

// Config is a loaded and checked config.
type Config struct {
	Listen         string
	Routing        []string // model ids, each naming a configured provider
	AttemptTimeout time.Duration
	Providers      map[string]Provider
	Log            Log
}

// Provider is one configured provider, with its key resolved.
type Provider struct {
	APIKey  string
	BaseURL string // "" for the provider's default
}

type Log struct {
	Enabled bool
	Dir     string
}

type file struct {
	Listen         *string
	Routing        []string
	AttemptTimeout *string `toml:"attempt_timeout"`
	Providers      map[string]struct {
		APIKey    string `toml:"api_key"`
		APIKeyEnv string `toml:"api_key_env"`
		BaseURL   string `toml:"base_url"`
	}
	Log struct {
		Enabled *bool
		Dir     string
	}
}

// Sample is a config to copy, printed when none exists.
const Sample = `listen = "127.0.0.1:7979"
routing = ["typesafe/jev-latest"]
# attempt_timeout = "4s"

[providers.typesafe]
api_key_env = "TYPESAFE_API_KEY"       # or api_key = "..."; brout run by launchd needs api_key
# base_url = "https://api.typesafe.ai"

# [providers.subq]
# api_key = "..."

# [providers.openai]
# api_key_env = "OPENAI_API_KEY"

[log]
enabled = true
# dir = "~/.local/state/brout/logs"
`

// DefaultPath is $XDG_CONFIG_HOME/brout/config.toml, or
// ~/.config/brout/config.toml.
func DefaultPath() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "brout", "config.toml")
}

// Client is what brout's client commands, logs and replay, need from the
// config.
type Client struct {
	Listen string
	Log    Log
}

// Load reads and checks the config at path. Every error names the bad field.
func Load(path string) (Config, error) { return load(path, true) }

// LoadClient reads the config at path without resolving API keys, so the
// client commands work in a shell that lacks serve's api_key_env variables.
func LoadClient(path string) (Client, error) {
	cfg, err := load(path, false)
	return Client{Listen: cfg.Listen, Log: cfg.Log}, err
}

func load(path string, resolveKeys bool) (Config, error) {
	var f file
	md, err := toml.DecodeFile(path, &f)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("no config at %s; create it, for example:\n\n%s", path, Sample)
	}
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("%s: unknown field %s", path, strings.Join(keys, ", "))
	}

	cfg := Config{
		Listen:         DefaultListen,
		Routing:        f.Routing,
		AttemptTimeout: DefaultAttemptTimeout,
		Providers:      map[string]Provider{},
		Log:            Log{Enabled: true, Dir: filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "brout", "logs")},
	}
	if f.Listen != nil {
		cfg.Listen = *f.Listen
	}
	if f.AttemptTimeout != nil {
		d, err := time.ParseDuration(*f.AttemptTimeout)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("%s: attempt_timeout %q is not a positive duration such as \"4s\"", path, *f.AttemptTimeout)
		}
		cfg.AttemptTimeout = d
	}
	if f.Log.Enabled != nil {
		cfg.Log.Enabled = *f.Log.Enabled
	}
	if f.Log.Dir != "" {
		cfg.Log.Dir = expandHome(f.Log.Dir)
	}
	if !resolveKeys {
		return cfg, nil
	}

	for name, p := range f.Providers {
		field := "providers." + name
		key := p.APIKey
		switch {
		case p.APIKey != "" && p.APIKeyEnv != "":
			return Config{}, fmt.Errorf("%s: %s sets both api_key and api_key_env; keep one", path, field)
		case p.APIKeyEnv != "":
			key = os.Getenv(p.APIKeyEnv)
			if key == "" {
				return Config{}, fmt.Errorf("%s: %s.api_key_env names %s, which is not set in brout's environment (launchd does not pass on your shell's); put the key in the config as api_key", path, field, p.APIKeyEnv)
			}
		case key == "":
			return Config{}, fmt.Errorf("%s: %s needs api_key or api_key_env", path, field)
		}
		cfg.Providers[name] = Provider{APIKey: key, BaseURL: p.BaseURL}
	}

	for _, id := range cfg.Routing {
		name, model, ok := strings.Cut(id, "/")
		if !ok || model == "" {
			return Config{}, fmt.Errorf("%s: routing entry %q is not a model id <provider>/<model>, such as typesafe/jev-latest", path, id)
		}
		if _, ok := cfg.Providers[name]; !ok {
			return Config{}, fmt.Errorf("%s: routing entry %q names provider %q, which has no [providers.%s] section", path, id, name, name)
		}
	}
	return cfg, nil
}

// ProviderNames returns the configured provider names, sorted.
func (c Config) ProviderNames() []string {
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func xdg(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return path
}
