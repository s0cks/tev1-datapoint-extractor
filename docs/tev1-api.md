# tev1 / `POST /v1/systemone`: API notes

> **Status: documentation-derived, NOT live-verified.**
> The notes below come from the published tev1 / TypeSafe ("System One") docs. The
> request side is documented in detail; the **response wire format has not been
> observed**. No real request/response pair has been captured yet, so the files in
> `testdata/api/` are synthetic (see `testdata/api/SYNTHETIC`).
>
> To verify: start Ollama (>= 0.35), `ollama pull tev1:0.8b`, run
> `scripts/capture-fixtures.sh`, then `go test ./...` and
> `go test -tags integration ./internal/pipeline -run Live -v`.
> Anything that disagrees with this page wins; fix the parser in
> `internal/tev1/types.go` and update this file.

## Endpoint

| | |
|---|---|
| Route | `POST http://localhost:11434/v1/systemone` (not `/api/chat` or `/api/generate`) |
| Needs | Ollama **0.35+** |
| Models | `tev1:0.8b` (default here), `tev1` / `tev1:4b`; also `-q8_0` and `-bf16` variants |
| Request limit | 64 KiB body, 1 to 64 questions |
| Context | ~2k tokens (model default `num_ctx` 2048). The whole state is scored against every question, so keep segments short. |
| Sampling | temperature 0 by default |

## Request

```json
{
  "model": "tev1:0.8b",
  "state": "string, or any JSON value",
  "questions": {
    "<name>": { "type": "choice|noul|score", "instructions": "...", "criteria": "..." }
  },
  "keep_alive": "5m"
}
```

- `state` may be a string or a JSON object/array. This tool sends
  `{"segment": "<text>", "features": "numbers=3; ..."}` (or just the text string when
  `state.include_features` is false).
- `questions` is a JSON **object**; answers come back "in the same order". The client therefore
  serialises questions in config order rather than Go map order.
- `keep_alive` is optional and is the only Ollama-style option documented for this endpoint.
  `num_ctx` is **not** documented as accepted, so we do not send it.

### Question types

| type | `criteria` | answer fields |
|---|---|---|
| `choice` | object `option -> description` (or `null` for self-describing names); 2 to 26 options | `choice`, `probabilities`, `confidence` |
| `noul` | optional `{"true": "...", "false": "..."}` | `noul` = P(true) |
| `score` | array of level descriptions, lowest first; levels are numbered **from 0** | `score` (expected level), `legend`, `probabilities`, `confidence` |

`confidence` (0 to 1) measures how concentrated the probabilities are; it is not the chance of
being right.

## How this tool maps its questions

| Question | Wire type | Notes |
|---|---|---|
| `is_datapoint` | `choice` with `yes` / `no` / `unknown` | `noul` has no documented "unknown" outcome, and `needs_review` wants a real P(unknown). `type: "noul"` also works if you prefer it (config validation accepts both). |
| `kind` | `choice` with 9 options | One-line description per option. |
| `importance` | `score` with 5 levels (0..4) | Reported to you as 1..5 (`score + 1`). |

## What is assumed about the response (unverified)

```json
{ "model": "tev1:0.8b",
  "answers": {
    "is_datapoint": { "choice": "yes", "probabilities": { "yes": 0.93, "no": 0.05, "unknown": 0.02 }, "confidence": 0.9 },
    "importance":   { "score": 3.2, "legend": ["..."], "probabilities": { "0": 0.01, "...": 0 }, "confidence": 0.58 }
  } }
```

The parser (`tev1.ParseResponse`) is deliberately tolerant because of this uncertainty:

- `answers` may be an object keyed by question name, **or** an array. Array entries are matched by
  an entry-level `name`/`id` when present, otherwise by position in request order.
- `probabilities` may be an object or an array of numbers (array entries are keyed `"0"`, `"1"`, ...).
- Missing `probabilities` is tolerated (treated as one-hot on the chosen option); a missing
  `choice` / `noul` / `score` is a malformed response.
- Out-of-range probabilities, a `choice` that is not a declared option, or a `score` outside
  `0..levels-1` are rejected as malformed (never retried).

If the real shape differs in some other way (for example `noul` returned as an object), the
client returns `ErrMalformed` with a message naming the offending question, and
`scripts/capture-fixtures.sh` plus the fixture tests will show exactly where.

## Errors and retries

- HTTP 429 and 5xx, network errors and per-attempt timeouts are retried with exponential backoff
  and jitter (`retries`, `backoff_ms`, `timeout_seconds` in config).
- Other 4xx (bad request, unknown model, too many questions) and malformed 200 responses are not retried.
- Ctrl-C cancels in-flight requests and any pending backoff.

## Preflight

1. `GET /api/version` returns `{"version": "..."}`; must be >= 0.35.0.
2. `GET /api/tags` must list the model (`name` or `model`; a missing tag means `:latest`).
   If your Ollama build does not list decision models there, use `--skip-preflight`.

## Open questions for live verification

1. Exact response shape (object vs array `answers`; `probabilities` keying for `score`).
2. Whether `noul` can return an "unknown" outcome of any kind.
3. Whether `options.num_ctx` or other options are accepted, and what an over-long state does (error vs truncation).
4. Whether `/api/tags` lists `tev1` models.
5. Real-world calibration of the 0.8B model (use `tev1-rank eval`).
