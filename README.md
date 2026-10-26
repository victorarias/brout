# brout

A local HTTP gateway for decision-model APIs. Send text or JSON state and named
questions to one endpoint, using TypeSafe, OpenAI, or SubQ behind it.

```text
your app -> brout on 127.0.0.1:7979 -> TypeSafe / OpenAI / SubQ
```

brout answers `noul` (a probability), `choice`, and `score` questions. It can try
an ordered routing list when a provider is unavailable. Every response names the
model that answered, and the request log keeps the full exchange for replay.

SubQ support is tested with fake providers only. It has not been tested against
the live API. Images, assets, and `box` questions are unsupported.

## Install

Build from source with [Go](https://go.dev/dl/) at the version in
[go.mod](https://github.com/victorarias/brout/blob/main/go.mod) or newer:

```sh
git clone https://github.com/victorarias/brout.git
cd brout
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/brout" ./cmd/brout
export PATH="$HOME/.local/bin:$PATH"
brout help
```

Add the PATH export to your shell profile to keep it in later sessions.

After the first release, you can download an archive from
[GitHub Releases](https://github.com/victorarias/brout/releases). The archives
contain `brout`, this README, the reference, and the MIT license. Pick
`darwin_arm64` for Apple Silicon, `darwin_amd64` for Intel Macs, or `linux_arm64`
or `linux_amd64` for Linux.
To download a release with curl, set `BROUT_VERSION` to a published version
without its leading `v`, and choose the platform above:

```sh
BROUT_VERSION=0.1.0
BROUT_PLATFORM=darwin_arm64
BROUT_ARCHIVE="brout_${BROUT_VERSION}_${BROUT_PLATFORM}.tar.gz"
curl -fL -o "$BROUT_ARCHIVE" \
  "https://github.com/victorarias/brout/releases/download/v${BROUT_VERSION}/${BROUT_ARCHIVE}"
curl -fL -o checksums.txt \
  "https://github.com/victorarias/brout/releases/download/v${BROUT_VERSION}/checksums.txt"
shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf "$BROUT_ARCHIVE"
mkdir -p "$HOME/.local/bin"
mv brout "$HOME/.local/bin/brout"
export PATH="$HOME/.local/bin:$PATH"
brout version
```

The macOS binaries are signed with a Developer ID and notarized by Apple, so
Gatekeeper lets them run whether you download them with curl or a browser. A
bare binary can't carry its notarization ticket, so the first run checks it
online. To check a binary yourself:

```sh
codesign --verify --strict --verbose=2 "$HOME/.local/bin/brout"
codesign -dv "$HOME/.local/bin/brout" 2>&1 | grep -E 'Authority=Developer ID|TeamIdentifier'
```

Run `brout version` once before installing the LaunchAgent, so the first-run
check happens in your shell and not under launchd.

## Get an answer

Create the default config directory:

```sh
mkdir -p "$HOME/.config/brout"
```

Save this as `~/.config/brout/config.toml`. Replace the placeholder with your
[TypeSafe API key](https://console.typesafe.ai/):

```toml
listen = "127.0.0.1:7979"
routing = ["typesafe/jev-latest"]
attempt_timeout = "4s"

[providers.typesafe]
api_key = "YOUR_TYPESAFE_API_KEY"

[log]
enabled = true
```

Protect the config and start the gateway:

```sh
chmod 600 "$HOME/.config/brout/config.toml"
brout serve
```

The command prints `brout listening on http://127.0.0.1:7979` and the request log
directory. In another terminal, send a request:

```sh
curl --fail-with-body -i http://127.0.0.1:7979/v1/systemone \
  -H 'Content-Type: application/json' \
  -d '{
    "state": "I was charged twice. Please fix this today.",
    "questions": {
      "billing": {"type": "noul", "instructions": "Is this about billing?"},
      "tone": {
        "type": "choice",
        "instructions": "What is the customer tone?",
        "criteria": {"calm": null, "frustrated": null}
      },
      "urgency": {
        "type": "score",
        "instructions": "How urgent is this?",
        "criteria": ["can wait", "this week", "today"]
      }
    }
  }'
```

Expect HTTP 200, a `Brout-Request-Id` header, and JSON with `model`, `answers`,
and `usage`. The model includes its provider, such as `typesafe/jev-1.14.0`.
Answers vary between runs. Keep the request ID to inspect or replay it.

brout listens without authentication. Keep `listen` on loopback for local use.

## Add providers and route requests

To add OpenAI as a fallback, replace your config with this full example and
fill in both keys:

```toml
listen = "127.0.0.1:7979"
routing = ["typesafe/jev-latest", "openai/gpt-6-luna"]
attempt_timeout = "4s"

[providers.typesafe]
api_key = "YOUR_TYPESAFE_API_KEY"
# base_url = "https://api.typesafe.ai"

[providers.openai]
api_key = "YOUR_OPENAI_API_KEY"
# base_url = "https://api.openai.com"

# To use SubQ, uncomment its section, set the key, and add
# "subq/subq-snap-1.1" to routing (untested against the live API).
# [providers.subq]
# api_key = "YOUR_SUBQ_API_KEY"
# base_url = "https://api.subq.ai"

[log]
enabled = true
# dir = "~/.local/state/brout/logs"
```

Restart `brout serve` after you edit the config. For another config location,
run `brout serve --config /absolute/path/config.toml`. The default config is
`$XDG_CONFIG_HOME/brout/config.toml` when `XDG_CONFIG_HOME` is set.

For foreground use, you can replace a provider's `api_key` with
`api_key_env = "TYPESAFE_API_KEY"` or `api_key_env = "OPENAI_API_KEY"`.
Set the named variable in the environment of `brout serve`. Use one key setting
per provider. Every configured provider needs a resolved key, even if it is
absent from `routing`.

Omit `model` or send `"model": "auto"` to try the routing list in order.
brout skips models whose known limits reject the request, then tries the rest.
A connection error, an attempt timeout, HTTP 401, 402, 403, 429, or any 5xx
(including 529) moves on to the next model. Other rejections go back to you.

To select one model, send `"model": "openai/gpt-6-luna"` or
`"model": "typesafe/jev-latest"`. A named request gets one attempt and never
fails over. brout does not retry attempts. Use provider-prefixed model IDs;
a bare `jev-latest` is an error.

Each attempt can take `attempt_timeout`, so timeouts add up across the routing
list. The default is 4 seconds per model. TypeSafe SDKs default to a 10-second
attempt timeout. Keep the routing list short or lower `attempt_timeout` so that
the SDK can wait for the entire sequence. Both SDKs default to two retries on
408, 429, 5xx, connection failures, and their own timeouts. An all-unavailable
auto request can therefore run the entire routing list three times, producing
separate log entries. A caller timeout appears as status 499 before a retry.
Configure the SDK's retry policy to change this behavior
([JavaScript](https://docs.typesafe.ai/sdk/javascript/api/interfaces/RetryPolicy),
[Python](https://docs.typesafe.ai/sdk/python/api/retries)).
List the routing models with:

```sh
curl --fail-with-body http://127.0.0.1:7979/v1/models
```

See [provider limits and state translation](docs/reference.md#provider-limits)
before sending large requests. brout does not count tokens, so a provider's
context-length rejection does not trigger failover.

## Use the TypeSafe SDKs

Point the SDK at brout and set its default model to `auto`. The SDK requires an
API key string, but brout ignores the incoming Authorization header and uses
the keys in its own config. A placeholder keeps provider keys out of your app.

For the [JavaScript SDK](https://docs.typesafe.ai/sdk/javascript/), use Node.js
22 or newer and install `@typesafe-ai/sdk`:

```sh
npm install @typesafe-ai/sdk
export TYPESAFE_BASE_URL=http://127.0.0.1:7979
```

```js
import { TypeSafeClient } from "@typesafe-ai/sdk";

const client = new TypeSafeClient({ apiKey: "brout-local", defaultModel: "auto" });
const result = await client.systemOne({
  state: "I was charged twice.",
  questions: { billing: { type: "noul", instructions: "Is this about billing?" } },
});
console.log(result);
```

For the [Python SDK](https://docs.typesafe.ai/sdk/python/), install `typesafe-sdk`
in your Python environment and set the same `TYPESAFE_BASE_URL`:

```sh
pip install typesafe-sdk
export TYPESAFE_BASE_URL=http://127.0.0.1:7979
```

```python
from typesafe_sdk import Noul, TypeSafeClient

with TypeSafeClient(api_key="brout-local", model="auto") as client:
    result = client.system_one(
        state="I was charged twice.",
        questions={"billing": Noul(instructions="Is this about billing?")},
    )
    print(result)
```

The default SDK model is a bare TypeSafe model name, so keep `defaultModel:
"auto"` (JavaScript) or `model="auto"` (Python), or use a provider-prefixed ID.
brout accepts plain JSON, gzip, x-gzip, and zstd request bodies, including the
SDKs' compressed requests.

## Run in the background on macOS

Stop a foreground `brout serve` with Ctrl-C before installing the agent.
LaunchAgents do not inherit your shell's environment. Put provider keys directly
in the config as `api_key`; `brout service install` rejects `api_key_env`, even
when the installing shell has that variable.

```sh
brout service install
brout service status
```

Install prints the plist path and configured address. Status prints whether the
agent is loaded, its launchd state, PID, last exit code, and configured address.
Unavailable diagnostics are shown as `unknown`. A nonzero last exit code points
to the stderr log. Status also reports a missing or non-executable installed
binary. Registration does not prove that the HTTP listener started. Send the
curl request above to check it.

The plist is `~/Library/LaunchAgents/io.github.victorarias.brout.plist`.
It starts the installed binary with `serve --config` and an absolute config
path, runs at login, and restarts the process if it exits. Keep the binary at
that path. To select a custom config, use:

```sh
brout service install --config /absolute/path/config.toml
```

There is one agent per macOS account. Another HOME still shares that account's
launchd domain. Uninstall from the HOME that installed the agent.

To stop it and remove the plist:

```sh
brout service uninstall
```

Uninstall waits for the process to exit. It keeps your config and logs. After
changing the config or replacing the binary, run uninstall followed by install.

For startup errors and failover messages, read
`~/.local/state/brout/service.stderr.log`. Startup output is in
`service.stdout.log` beside it. These paths use `$XDG_STATE_HOME/brout` when
set at installation. Install preserves HOME and XDG_STATE_HOME in the plist.
Linux users can run `brout serve` under their own process manager.

## Inspect and replay requests

brout logs `POST /v1/systemone` requests, including failures, with full requests
and responses in one JSONL file per UTC day under
`~/.local/state/brout/logs/YYYY-MM-DD.jsonl`. The default uses
`$XDG_STATE_HOME/brout/logs` when set. Set `[log] dir` to change it, or
`enabled = false` to disable request logging.

```sh
brout logs --limit 10
brout logs show REQUEST_ID
brout replay REQUEST_ID --model openai/gpt-6-luna
```

Replace `REQUEST_ID` with an ID from `brout logs` or a `POST /v1/systemone`
response header. Other HTTP paths also carry `Brout-Request-Id`, but are not
written to the request log.
`logs show` prints the full entry. Replay sends the original request through
the running gateway and compares each old and new answer. Omit `--model` to
keep the original request's model selection. The replay is logged with
`replay_of` pointing to the original ID.

Use `--config /path/config.toml` with any log command to select a different
config. Replay defaults to that config's `listen`; `--url http://host:port`
overrides the server address.

Logs contain your full state. Nothing deletes old files. Delete daily files to
prune them. Readers skip torn or unknown-version lines with a note on stderr.
See the [version 1 log entry format](docs/reference.md#request-log-entry).

## Handle errors

Errors have this shape, with a `Brout-Request-Id` response header. IDs from
`POST /v1/systemone` can be inspected with `brout logs show`:

```json
{"detail":{"error_type":"unknown_model","message":"model ... is not a model id; ..."}}
```

The error types are `invalid_request`, `unsupported`, `unknown_model`, `limit`,
`no_model_fits`, `rejected`, `unavailable`, `body_too_large`, `canceled`,
`not_found`, and `internal`. See the [HTTP and error reference](docs/reference.md#http-contract)
for statuses and causes.

## Build and verify changes

```sh
go vet ./...
go test ./...
goreleaser release --snapshot --clean
```

The [GoReleaser config](https://github.com/victorarias/brout/blob/main/.goreleaser.yaml) builds macOS and Linux binaries for
amd64 and arm64. A snapshot stays in `dist/` and publishes nothing. A local
snapshot leaves the macOS binaries unsigned, because
[scripts/sign-darwin.sh](https://github.com/victorarias/brout/blob/main/scripts/sign-darwin.sh) skips signing without
Apple credentials in the environment.
[CI](https://github.com/victorarias/brout/blob/main/.github/workflows/ci.yml) runs vet and tests on macOS and Linux for pull
requests. The [release workflow](https://github.com/victorarias/brout/blob/main/.github/workflows/release.yml) publishes when
a `v*` tag is pushed. It runs on macOS, signs and notarizes the darwin binaries,
and fails rather than publish them unsigned. Running it by hand (workflow
dispatch) rehearses a signed snapshot without publishing. It needs these
repository secrets: `APPLE_CERTIFICATE` (a base64 Developer ID Application
`.p12`), `APPLE_CERTIFICATE_PASSWORD`, `KEYCHAIN_PASSWORD`, `APPLE_API_ISSUER`,
`APPLE_API_KEY` (the App Store Connect key id) and `APPLE_API_KEY_P8` (the
base64 `.p8`).

On macOS, rerun the live install check with a TypeSafe key in the environment:

```sh
python3 scripts/service-smoke.py /tmp/brout-evidence
```

The check requires `TYPESAFE_API_KEY`, uses a temporary HOME and a separate
port, and refuses to replace an existing brout LaunchAgent. It installs and
uninstalls an agent in your real launchd account. On failure it preserves the
temporary HOME and prints a cleanup command.
