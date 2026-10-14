# testdata/api

Request/response pairs replayed by `internal/tev1` tests (`*.request.json` ↔ `*.response.json`).

- `classify.*`: **real**. The response was captured from Ollama 0.40.1 serving `tev1:0.8b`
  (the request is the one in this directory). It confirms: `answers` is an object keyed by
  question name; each answer has `type`; `choice` answers carry `choice`, `probabilities`
  (object keyed by option) and `confidence`; `score` answers carry `score` (the
  probability-weighted 0-based level), `legend` (an **object** `{"0": "...", ...}`),
  `probabilities` (object keyed by level index) and `confidence`; the response also has
  `model` and `usage {input_tokens, output_tokens}`.
- `basic.*`: synthetic (hand-written from the docs). Not yet captured.
- `arrayshape.*`: synthetic on purpose; exercises the tolerant "answers as an array" path that
  the real server does not (as far as we know) use.

`SYNTHETIC` lists which files are hand-written. `scripts/capture-fixtures.sh` re-captures
`basic` and `classify` from a live Ollama; review the diff before committing.
