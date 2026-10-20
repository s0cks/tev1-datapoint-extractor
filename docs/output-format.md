# Output format

`tev1-rank rank <segments.json> --out-dir out/` writes three files. All JSON is UTF-8 and hashes are
always `sha256:<hex>`. `start`/`end` are **byte** offsets into the *source document* the upstream
tool segmented (e.g. `main.md`), which this tool never sees, so they cannot be checked against the
input. `text` is the segment's **plain text** (markup stripped), which is what the model saw, so
`source[start:end]` equals the upstream `text`, not this `text`, whenever the segment has markup.

`scored.jsonl` and `top.json` contain no timestamps: with the same input, config, model and cache
they are byte-identical across runs (modulo the `cached` flag in `scored.jsonl`). `run.json` carries
the timings.

Schema versions: `top.json` is `tev1-rank/top/v3`, `run.json` is `tev1-rank/run/v2`. Breaking
changes bump the version; stage 2 should check it. v3 (this tool stopped segmenting and analysing
text itself): `source` is now `{file, format, segments_hash}`; `start`/`end` refer to the upstream
source document; `text` is plain text; items gained `block`, `block_kind` and `section`; and
`features` is now `{numerics, entities, words, position}` (it was `numbers`, `percentages`,
`currency`, `years`, `dates`, `entities`, `length`, `words`, `position`, `term_weight`). The v2
changes still apply: no `model.p_unknown`, no `high_unknown_probability` review reason, and `kind`
values are the card-schema block types.

## `top.json`: what stage 2 consumes

Machine-checkable schema: [`schema/top.schema.json`](../schema/top.schema.json) (JSON Schema
2020-12; a test validates real output against it, in both modes).

```jsonc
{
  "schema_version": "tev1-rank/top/v3",
  "mode": "model",                       // or "no-model"
  "source": { "file": "main.md", "format": "md",          // the document the upstream tool segmented
              "segments_hash": "sha256:…" },              // hash of the segments file that was read
  "model": "tev1:0.8b",                  // null in no-model mode
  "question_set_hash": "sha256:…",       // null in no-model mode
  "selection": { "top": 20, "min_score": 0, "max_per_kind": 0 },
  "count": 20,
  "items": [                             // best first
    {
      "rank": 1,
      "id": "s12",                       // the upstream tool's segment id
      "hash": "sha256:…",                // hash of `text`
      "block": 9, "block_kind": "paragraph",           // upstream structure (not the tev1 kind)
      "section": ["2024", "Results"],                  // heading path
      "start": 1034, "end": 1151,        // byte offsets into the SOURCE document
      "text": "Revenue grew 14% …",      // plain text, as the model saw it
      "kind": "stats",                   // never "other" by default; null in no-model mode
      "final_score": 0.8312,             // 0..1
      "needs_review": false,
      "review_reasons": [],              // see below
      "features": { "numerics": 3, "entities": 1, "words": 19, "position": 0.31 },
      "model": {                         // null in no-model mode
        "p_datapoint": 0.93,             // 1 - P(kind = other)
        "importance": 4.2,               // expected importance on 1..5
        "kind_probs": { "stats": 0.81, "chart": 0.03, "…": 0 },
        "confidence": 0.81               // lowest model confidence among review.confidence_questions (default: kind); null if not reported
      }
    }
  ]
}
```

`needs_review` is set when any of these hold (thresholds in `ranking.review`): `p_datapoint` is within
0.1 of 0.5 (the model is torn between "datapoint" and `other`); the top two `kind` probabilities are
within `kind_margin`; or the model confidence of the questions in `confidence_questions` is below
`min_confidence`. By default only `kind` counts for confidence: `importance` (5-level ordinal) reports
inherently low confidence and would flag every segment.

`review_reasons` values: `datapoint_uncertain`, `kind_top_two_close`, `low_confidence`,
`context_window_exceeded`, `model_error` (that segment fell back to feature scoring).

Segments the model classified as `other` are ranked in `scored.jsonl` but never appear in `top.json`
unless you set `selection.exclude_kinds` to `[]`.

Items are never duplicates: near-identical segments (same text after lower-casing and removing
punctuation/spacing) are collapsed, keeping the first occurrence.

## `scored.jsonl`: every segment, one JSON object per line, in document order

| field | meaning |
|---|---|
| `id`, `block`, `block_kind`, `section`, `start`, `end`, `hash`, `text` | segment identity and structure, from the upstream tool. `id` is the upstream id; `start`/`end` are source-document offsets; `text` is the plain text and `hash` is its sha256. |
| `features` | as in `top.json` |
| `scored_by` | `model`, `features` (`--no-model`), or `features_fallback` (the model call for this segment failed) |
| `answers` | **raw model answers**: `{ "kind": {...}, "importance": {...} }`, each with `choice` / `score`, `probabilities`, `legend`, `confidence`. `importance` is absent for `other` segments (never asked). Omitted unless `scored_by` is `model`. |
| `signals` | derived values: `p_datapoint`, `importance`, `importance_norm`, `kind`, `kind_probs`, `kind_weight`, `kind_margin`, `confidence` |
| `cached` | answers came from the on-disk cache |
| `input_tokens`, `output_tokens` | what the server reported, summed over the segment's requests (one for `other`, two otherwise; cached hits report the original values) |
| `peak_input_tokens` | the largest single request of the segment |
| `context_exceeded` | `peak_input_tokens` > `state.context_tokens`; the prompt may have been truncated, so the answers are unreliable |
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
final_score = ( w.datapoint  * (1 - P(kind = other))
              + w.importance * (importance_level / (levels-1))
              + w.kind       * Σ_k P(kind=k) * kind_weights[k]
              + w.features   * feature_bonus ) / (sum of the four weights)

feature_bonus = Σ fw[f] * min(1, count_f / feature_saturation) / Σ fw      (f ∈ numerics, entities)
```

`numerics` and `entities` are counts read off the upstream annotations (see
[input-format.md](input-format.md)); `words` and `position` are reported but carry no weight. Every
weight, `kind_weights`, `feature_weights`, `feature_saturation` and the review thresholds
live in the config (`tev1-rank config` prints the defaults). In `--no-model` mode
`final_score = feature_bonus`. A segment whose model call failed is scored with the model terms
set to zero, so it sinks rather than floats to the top.

## `run.json`: manifest

```jsonc
{
  "schema_version": "tev1-rank/run/v2",
  "tool": { "name": "tev1-rank", "version": "dev" },
  "started_at": "…Z", "finished_at": "…Z",
  "mode": "model",                                // or "no-model"
  "input": { "path": "segments.json", "hash": "sha256:…", "bytes": 4210,   // the segments file read
             "source": "main.md", "format": "md" },                          // as reported by the upstream tool
  "model": { "name": "tev1:0.8b", "ollama_version": "0.35.0",
             "question_set_hash": "sha256:…",
             "state_format": "state/v4;two-stage;features=true;max_text_bytes=4000" },   // null in no-model mode
  "config": { … },                                // the effective config, including the questions
  "selection": { "top": 20, "min_score": 0, "max_per_kind": 0 },
  "counts": { "segments": 26, "skipped_entries": 0, "duplicates": 1, "ranked": 25, "selected": 20, "needs_review": 3,
              "model_calls": 39, "importance_skipped": 13,   // requests sent; segments classified "other" got no importance request "cache_hits": 1, "cache_misses": 25, "model_errors": 0,
              "context_exceeded": 0, "input_tokens": 45075, "output_tokens": 100 },       // summed over model calls, as reported by the server
  "timings_ms": { "load": 1, "preflight": 12, "classify": 8123, "total": 8140, "model_calls_sum": 8090 },
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

## Text sent to the model vs. text reported

The model is sent the upstream `plain` text, cut at `state.max_text_bytes` (flagged
`state_truncated`), and, with `state.include_features`, a one-line summary of the counts
(`numerics=3; named_entities=1; words=19`). The record's `text` is that same plain text. The
upstream `text` (with markup) and `tts` fields are not carried into the outputs; if stage 2 wants
either, say so and it can be added as an additive field.

## Config warnings

At startup (model mode) the tool warns when `kind` options have no `ranking.kind_weights` entry
(they fall back to `default_kind_weight`, so the kind term cannot separate them), when
`kind_weights` has entries that match no option, and when the config has questions other than
`kind` and `importance` (they are never sent), and when `ranking.feature_weights` names something
other than `numerics` and `entities` (for example `years` or `term_weight` from older configs).
`kind_weights` and `feature_weights` are **merged** with the embedded defaults, so stale keys
linger; they are ignored but reported.
