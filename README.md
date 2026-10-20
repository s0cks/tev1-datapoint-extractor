# tev1-rank

Stage 1 of the infographic pipeline. Splits a stripped-down plain-text file into segments, has a
**tev1** decision model (via Ollama's `/v1/systemone`) classify and score each one, and writes a
ranked list for the later stages. It does not build Qwen JSON, layout, rendering or QA.

```
input.txt ─► segmenter ─► features ─► tev1 request 1: kind ──► "other"? ─► done (not a datapoint)
                (no model)  (no model)                              │ no
                                                  tev1 request 2: importance ─► score ─► rank/dedupe/select
                                                  (cached on disk, sequential)           scored.jsonl  top.json  run.json
```

## Quick start

```bash
go mod tidy                       # first time only (resolves go.sum)
go build -o bin/tev1-rank .

./bin/tev1-rank segments testdata/sample.txt                       # segmenter only, no model
./bin/tev1-rank rank testdata/sample.txt --no-model --out-dir out  # deterministic features only (works in CI)
./bin/tev1-rank rank testdata/sample.txt --out-dir out --top 10    # full run (needs Ollama >= 0.35 + tev1:0.8b)
./bin/tev1-rank eval testdata/labels.example.jsonl                 # accuracy / per-kind P&R / precision@K
./bin/tev1-rank config > my.json                                   # dump defaults, edit, pass with --config my.json
```

Useful flags: `--model tev1:4b`, `--top N`, `--min-score S`, `--max-per-kind N`, `--no-cache`,
`--concurrency N` (default 1), `--ollama-url` (or `$OLLAMA_HOST`), `--skip-preflight`, `-v`.

## How a segment is classified

Two separate requests per segment, which keeps each prompt small (see "Context window"):

1. **`kind`**: which card block the segment would become: `stats`, `chart`, `timeline`, `quote`,
   `findings`, `threads`, `callout`, `profile`, `sources`, or `other`. These mirror the prose-derivable
   block types of the card schema (`qr` and `image` cannot come from prose). **`other` means the model
   decided the segment is not a datapoint**: it gets no second request, is cached as such, and is not
   selected into `top.json` (`selection.exclude_kinds`). There is no separate `is_datapoint` question;
   `p_datapoint` is `1 - P(kind = other)`.
2. **`importance`** (1..5), only for datapoints. Its state also carries the kind from request 1.

Tune the kind descriptions in `internal/config/default.json` (or a `--config` file) and measure with
`tev1-rank eval`, which prints per-kind precision/recall and a kind confusion matrix.

## Read this first

The tev1 request format is documented, but **the response format has not been verified against a
live Ollama**; the fixtures in `testdata/api/` are synthetic. See [docs/tev1-api.md](docs/tev1-api.md).
Before trusting results, on a machine with Ollama:

```bash
ollama pull tev1:0.8b
scripts/capture-fixtures.sh                    # replaces synthetic fixtures with real ones
go test ./...
go test -tags integration ./internal/pipeline -run Live -v
```

## Context window

tev1's context is small (~2k tokens). Each request now carries one question instead of the whole
set, so there is much more room for segment text; the numbers below were measured with the old
single-request layout and should be re-measured. Segments can still overflow it. Every segment records its `input_tokens` (summed over its requests) and `peak_input_tokens` (the
largest single request); ones whose peak exceeds the window are flagged
`context_exceeded` / `needs_review`. Run `scripts/measure-tokens.sh` to see where the tokens go,
and see "Context budget" in [docs/tev1-api.md](docs/tev1-api.md) for fixes (derived model with a
larger `num_ctx`, shorter questions).

## Layout

| path | purpose |
|---|---|
| `internal/segment` | deterministic paragraph/sentence segmenter (byte offsets, sha256 hashes) |
| `internal/features` | numbers, %, currency, years/dates, entities, TF-IDF-style term weight |
| `internal/tev1` | `/v1/systemone` client: ordered questions, tolerant parsing, retries, preflight |
| `internal/config` | embedded `default.json`, file overrides (strict), `question_set_hash` |
| `internal/rank` | score, review flags, dedupe, selection (`--top/--min-score/--max-per-kind`) |
| `internal/cache` | on-disk answer cache keyed by `sha256(model + question_set_hash + segment hash)` |
| `internal/pipeline` | orchestration, outputs, manifest |
| `internal/evaluate` | `eval` metrics |
| `schema/top.schema.json` | JSON Schema for `top.json` |
| `docs/` | `tev1-api.md`, `output-format.md` |

## Tests

`go vet ./... && gofmt -l . && go test -race ./...`. The live-Ollama tests are behind the
`integration` build tag and skip themselves if Ollama is unreachable.
