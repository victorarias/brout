# brout reference

## Config

The default config path is `$XDG_CONFIG_HOME/brout/config.toml`, or
`~/.config/brout/config.toml` when `XDG_CONFIG_HOME` is unset. `serve`, `logs`,
`logs show`, `replay`, and `service install` accept `--config`.
`service status` reads the config path from the installed plist.

| Field | Default | Meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:7979` | HTTP listen address |
| `routing` | Empty | Ordered provider-prefixed model IDs for auto requests |
| `attempt_timeout` | `4s` | Positive Go duration for each provider attempt |
| `providers.<name>.api_key` | Unset | Provider key in the config |
| `providers.<name>.api_key_env` | Unset | Environment variable holding the provider key, for foreground use |
| `providers.<name>.base_url` | Provider's URL | API root, without `/v1/systemone` or `/v1/decisions` |
| `log.enabled` | `true` | Request logging |
| `log.dir` | `$XDG_STATE_HOME/brout/logs`, or `~/.local/state/brout/logs` | JSONL directory; `~/` expands to HOME |

Provider names are `typesafe`, `openai`, and `subq`. Each configured provider
needs exactly one key setting. Every routing entry must name a configured
provider. Unknown config fields are errors. `serve` loads config once at startup.

## Provider limits

These bounds combine the provider documentation with live measurements on
2026-10-25. Unknown bounds are left to the provider.

| Provider | Questions | Choice options | Score levels | Context | Request body |
| --- | --- | --- | --- | --- | --- |
| TypeSafe | Unknown | 1 to 255 | 1 to 10 | About 32.8K tokens, measured on jev-1.14.0 | 32 MiB |
| SubQ | 1 to 128 | 2 to 255 | 2 to 10 | 384K tokens, documented | 13 MiB |
| OpenAI | 1 to 400 | 2 to 255 | 2 to 10 | Unknown | Unknown |

SubQ question names have a documented limit of 100 characters. SubQ has not
been tested against the live API. The adapters enforce question counts, choice
counts, score counts, and the SubQ name limit. brout does not count tokens or
enforce provider-specific body sizes. It accepts at most 64 MiB after
decompression. A provider context or body-size rejection is returned without
failover.

Sources: [TypeSafe System One](https://docs.typesafe.ai/api/system-one),
[SubQ Decisions API](https://docs.subq.ai/endpoints/decisions-api/), and
[OpenAI Decisions](https://developers.openai.com/api/docs/guides/decisions).

TypeSafe and SubQ receive System One requests. OpenAI receives a translated
`POST /v1/decisions` request: text state becomes text input, object state becomes
`input_json`, and array state becomes `input_json` wrapped as `{"state": [...]}`.
brout maps `noul` to a predicate, choice criteria to choices, and score criteria
to levels labeled `"0"`, `"1"`, and so on. The answer is mapped back to System
One. An OpenAI refusal is a 422 `rejected` error naming the question.

## HTTP contract

`POST /v1/systemone` accepts a JSON body with these fields:

| Field | Meaning |
| --- | --- |
| `model` | Optional. Absent, null, empty, or `"auto"` uses the routing list. Otherwise a `<provider>/<model>` ID. |
| `state` | Required string, object, or array |
| `questions` | Required object of question names to question objects |

Question objects have `type`, optional `instructions`, and `criteria`.
`instructions` can be text or JSON. A `noul` takes optional criteria keyed by
`"true"` and `"false"`. TypeSafe requires a `noul` to have at least one of
`instructions` or `criteria`. Omitting both produces a 400 provider rejection
without failover. A `choice` takes an object of option names to descriptions
(including null). A `score` takes an ordered array of level descriptions.
Top-level `assets`, `images`, and `box` questions are unsupported.

Accepted encodings are plain JSON, gzip, x-gzip, and zstd. A successful response
has HTTP 200 and this form:

```json
{
  "model": "typesafe/jev-1.14.0",
  "answers": {"billing": {"type": "noul", "noul": 0.99}},
  "usage": {"input_tokens": 40, "output_tokens": 1}
}
```

This is an illustrative response. Every response carries a `Brout-Request-Id`
header. Choice answers have `choice`, `confidence`, and `probabilities`. Score
answers have `score`, `confidence`, `legend`, and `probabilities`. Scores can
fall between level indices. The response model includes the provider and the
upstream model version when the provider returns one.

`GET /v1/models` lists `auto` followed by the configured routing entries. It is
a routing list, not an upstream model catalog.

Errors have the form
`{"detail":{"error_type":"...","message":"..."}}`.

| `error_type` | HTTP status | Cause |
| --- | --- | --- |
| `invalid_request` | 422, 415 for unsupported encoding, 400 for a body read failure | Invalid JSON, fields, questions, or encoding |
| `unsupported` | 422 | Assets, images, or box questions |
| `unknown_model` | 422 | Bare model name, missing model component, or unconfigured provider |
| `limit` | 422 | A named model's known limits reject the request |
| `no_model_fits` | 422 | No routing model fits, or the routing list is empty |
| `rejected` | Provider's status; 422 for an OpenAI refusal | Provider rejection without failover |
| `unavailable` | Provider's status; 502 for a connection or invalid upstream response; 504 for a timeout | A named attempt failed |
| `unavailable` | 503 | Every auto attempt failed |
| `body_too_large` | 413 | Body exceeds 64 MiB after decompression |
| `canceled` | 499 in the log | Caller canceled the request |
| `not_found` | 404 | Unknown HTTP path or method |
| `internal` | 500 | Unexpected internal error |

An unavailable attempt is a connection error, timeout, HTTP 401, 402, 403, 429,
or any 5xx. Other 4xx responses are rejections. brout logs every attempt and
prints a line to stderr for each failover. brout never retries an attempt.

## Request log entry

Only `POST /v1/systemone` is logged, including failed requests. Other HTTP paths
have request IDs but no log entries. The request log contains one JSON object
per line in `YYYY-MM-DD.jsonl`, with the day in UTC. This is an illustrative
version 1 entry, formatted for reading:

```json
{
  "v": 1,
  "id": "20261025-153012-7f3a9c",
  "time": "2026-10-25T15:30:12.123Z",
  "duration_ms": 412,
  "requested_model": "auto",
  "model": "typesafe/jev-1.14.0",
  "status": 200,
  "attempts": [
    {"model": "typesafe/jev-latest", "status": 200, "duration_ms": 405}
  ],
  "replay_of": null,
  "request": {
    "state": "I was charged twice.",
    "questions": {"billing": {"type": "noul", "instructions": "Is this about billing?"}}
  },
  "response": {
    "model": "typesafe/jev-1.14.0",
    "answers": {"billing": {"type": "noul", "noul": 0.99}},
    "usage": {"input_tokens": 40, "output_tokens": 1}
  }
}
```

`requested_model` is null when the body could not be parsed, and `model` is null
when no model answered. Each failed attempt adds an `error` string. Attempt
status 0 means that the provider sent no HTTP response. `replay_of` is the
original request ID for a replay, otherwise null.

`request` is the full decompressed body, a string when the body is not JSON,
or null when the body could not be read. `response` is the body sent to the
caller, including errors. An ID starts with its UTC day, so show and replay
look in one daily file. Logs have no retention limit. A failed append prints
to stderr and does not fail the request. Readers skip torn or unknown-version
lines with a stderr note.
