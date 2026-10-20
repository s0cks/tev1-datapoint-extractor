# Input format

`tev1-rank` reads one file: the JSON written by the upstream text-preparation tool. It does no
segmenting, markup stripping or NLP of its own. `tev1-rank validate <file>` parses a file exactly as
`rank` would and summarises it; `testdata/segments.sample.json` (cut from `testdata/sample.md`) is a
small example.

```jsonc
{
  "source": "main.md",              // the document that was segmented
  "format": "md",
  "segments": [
    {
      "type": "segment",            // entries of any other type are skipped (counted in skipped_entries)
      "id": "s2",                   // required, unique
      "block": 2,                   // ordinal of the source block
      "kind": "paragraph",          // heading | paragraph | blockquote | list_item | ...
      "level": 4,                   // headings only
      "section": ["2015", "Hyatt I"],        // heading path, outermost first
      "range": { "start": 24, "end": 386 },  // byte offsets into the SOURCE document; end >= start
      "text":  "In February 2015 … **48L** …", // as in the source, markup included
      "plain": "In February 2015 … 48L …",    // markup stripped, whitespace collapsed
      "tts":   "In February twenty fifteen …", // speech rewrite; carried but not used
      "tokens":   [ { "t": "February", "pos": "NNP", "stem": "february",
                      "range": { "start": 3, "end": 11 } }, … ],   // ranges are within `plain`
      "entities": [ { "text": "Ehud Barak", "label": "PERSON",
                      "range": { "start": 49, "end": 59 } }, … ],  // optional
      "token_count": 62             // upstream's own count; not used (see below)
    }
  ]
}
```

## What this tool relies on

| field | used for |
|---|---|
| `id` | identity in every output; must be unique, or the file is rejected |
| `plain` | the text the model sees, the reported `text`, its hash, and the cache key. A segment whose `plain` is empty (or only whitespace / zero-width characters) is skipped and counted in `skipped_entries` |
| `range` | reported as `start`/`end`; also gives each segment's `position` (start ÷ the largest `end` in the file). A negative or inverted range is rejected |
| `kind`, `block`, `section` | passed through to `scored.jsonl` / `top.json` as `block_kind`, `block`, `section` |
| `tokens[].pos` | `numerics` = number of tokens tagged `CD` |
| `tokens[].t` | `words` = number of tokens containing a letter or digit |
| `entities` | `entities` = number of spans |

Everything else (`text`, `tts`, `level`, `stem`, token and entity ranges, `token_count`, unknown
fields) is read but not used. Unknown fields are ignored on purpose: the file belongs to another
tool, so it may grow.

Two cautions, both visible in real output:

- `token_count` is not `len(tokens)`; in the sample it differs on about 44% of segments, so
  nothing here uses it.
- POS tags and entity labels are whatever the upstream tagger said (it tags words like `million`,
  `one` and `zone` as `CD`, and sometimes labels a hotel brand `GPE`). They feed a *soft* ranking
  signal (`ranking.feature_weights`) and, if `state.include_features` is on, one line of the model's
  input. They are not ground truth.

## Offsets

`range` points into the source document, which `tev1-rank` does not read. The only check made on it is
that it is non-negative and not inverted. If stage 2 needs to cut the source, it has the document;
if the upstream tool ever changes what its offsets count (bytes vs. characters), this tool will not
notice.
