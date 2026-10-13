# Output format

`tev1-rank rank <input.txt> --out-dir out/` writes three files. All JSON is UTF-8, hashes are
always `sha256:<hex>`, and offsets are **byte** offsets into the original input file
(`input[start:end] == text`; this is a tested invariant).

`scored.jsonl` and `top.json` contain no timestamps: with the same input, config, model and cache
they are byte-identical across runs (modulo the `cached` flag in `scored.jsonl`). `run.json` carries
the timings.

Schema versions: `top.json` is `tev1-rank/top/v1`, `run.json` is `tev1-rank/run/v1`. Breaking
changes bump the version; stage 2 should check it.

## `top.json`: what stage 2 consumes

Machine-checkable schema: [`schema/top.schema.json`](../schema/top.schema.json) (JSON Schema
2020-12; a test validates real output against it, in both modes).

```jsonc
{
  "schema_version": "tev1-rank/top/v1",
  "mode": "model",                       // or "no-model"
  "source": { "file": "sample.txt", "hash": "sha256:…" },   // base name + hash of the whole input
  "model": "tev1:0.8b",                  // null in no-model mode
  "question_set_hash": "sha256:…",       // null in no-model mode
  "selection": { "top": 20, "min_score": 0, "max_per_kind": 0 },
  "count": 20,
  "items": [                             // best first
    {
      "rank": 1,
      "id": "seg-0012-ab12cd34",
      "hash": "sha256:…",                // hash of `text`
      "start": 1034, "end": 1151,        // byte offsets into the input
      "text": "Revenue grew 14% …",
      "kind": "stat",                    // null in no-model mode
      "final_score": 0.8312,             // 0..1
      "needs_review": false,
      "review_reasons": [],              // see below
      "features": { "numbers": 3, "percentages": 1, "currency": 1, "years": 1, "dates": 0,
                    "entities": 0, "length": 117, "words": 19, "position": 0.31, "term_weight": 0.42 },
      "model": {                         // null in no-model mode
        "p_datapoint": 0.93, "p_unknown": 0.02,
        "importance": 4.2,               // expected importance on 1..5
        "kind_probs": { "stat": 0.81, "claim": 0.03, "…": 0 },
        "confidence": 0.58               // lowest model-reported confidence; null if not reported
      }
    }
  ]
}
```

`review_reasons` values: `high_unknown_probability`, `datapoint_uncertain` (noul questions only),
`kind_top_two_close`, `low_confidence`, `model_error` (that segment fell back to feature scoring).

Items are never duplicates: near-identical segments (same text after lower-casing and removing
punctuation/spacing) are collapsed, keeping the first occurrence.

## `scored.jsonl`: every segment, one JSON object per line, in document order

| field | meaning |
|---|---|
| `id`, `start`, `end`, `hash`, `text` | segment identity. `id` = `seg-<ordinal>-<first 8 hex of hash>`. |
| `features` | as in `top.json` |
| `scored_by` | `model`, `features` (`--no-model`), or `features_fallback` (the model call for this segment failed) |
| `answers` | **raw model answers**: `{ "<question>": { "choice" / "noul" / "score", "probabilities", "legend", "confidence" } }`. Omitted unless `scored_by` is `model`. |
| `signals` | derived values: `p_datapoint`, `p_unknown`, `importance`, `importance_norm`, `kind`, `kind_probs`, `kind_weight`, `kind_margin`, `confidence` |
| `cached` | answers came from the on-disk cache |
| `state_truncated` | segment text exceeded `state.max_text_bytes` and was cut before being sent |
| `model_error` | error text when `scored_by` is `features_fallback` |
| `feature_bonus` | 0..1 weighted feature score |
| `final_score` | 0..1 |
| `rank` | 1-based, or `null` for a removed near-duplicate |
| `duplicate_of` | id of the kept segment when `rank` is null |
| `selected` | included in `top.json` |
| `needs_review`, `review_reasons` | as above |

### How `final_score` is computed

```
final_score = ( w.datapoint  * P(is_datapoint = yes)
              + w.importance * (importance_level / (levels-1))
              + w.kind       * Σ_k P(kind=k) * kind_weights[k]
              + w.features   * feature_bonus ) / (sum of the four weights)

feature_bonus = Σ fw[f] * min(1, count_f / feature_saturation) / Σ fw      (term_weight is used as-is)
```

Every weight, `kind_weights`, `feature_weights`, `feature_saturation` and the review thresholds
live in the config (`tev1-rank config` prints the defaults). In `--no-model` mode
`final_score = feature_bonus`. A segment whose model call failed is scored with the model terms
set to zero, so it sinks rather than floats to the top.

## `run.json`: manifest

```jsonc
{
  "schema_version": "tev1-rank/run/v1",
  "tool": { "name": "tev1-rank", "version": "dev" },
  "started_at": "…Z", "finished_at": "…Z",
  "mode": "model",                                // or "no-model"
  "input": { "path": "sample.txt", "hash": "sha256:…", "bytes": 4210 },
  "model": { "name": "tev1:0.8b", "ollama_version": "0.35.0",
             "question_set_hash": "sha256:…",
             "state_format": "state/v1;features=true;max_text_bytes=4000" },   // null in no-model mode
  "config": { … },                                // the effective config, including the questions
  "selection": { "top": 20, "min_score": 0, "max_per_kind": 0 },
  "counts": { "segments": 26, "duplicates": 1, "ranked": 25, "selected": 20, "needs_review": 3,
              "model_calls": 25, "cache_hits": 1, "cache_misses": 25, "model_errors": 0 },
  "timings_ms": { "segment": 1, "preflight": 12, "classify": 8123, "total": 8140, "model_calls_sum": 8090 },
  "outputs": { "scored": "scored.jsonl", "top": "top.json" }
}
```

`question_set_hash` is `sha256` over the model name, the ordered questions and the state format.
Changing any of them changes the hash and therefore invalidates cached answers. Ranking weights,
concurrency, retries and the like deliberately do **not** change it.

## Exit codes

- `0`: success.
- `1`: error. This includes some segments falling back to feature scoring after model errors
  (unless `--allow-errors`); outputs are still written in that case.
- `130`: interrupted. No outputs are written; already-classified segments stay in the cache, so a
  rerun resumes.
