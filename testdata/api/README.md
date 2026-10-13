# testdata/api

Request/response pairs replayed by `internal/tev1` tests (`*.request.json` ↔ `*.response.json`).

**While the file `SYNTHETIC` exists, the responses here were written by hand from the
published tev1 / TypeSafe docs and were NOT captured from a live Ollama.** The request
side follows the documented shape; the response shape (notably whether `answers` is an
object or array, and how `probabilities` are keyed) is unverified.

Run `scripts/capture-fixtures.sh` on a machine with Ollama 0.35+ and the model pulled. It
overwrites the responses with real ones and deletes `SYNTHETIC`. Re-run `go test ./...`
afterwards; any shape mismatch will show up as a failing fixture test.

`arrayshape.*` deliberately tests the tolerant array form of `answers`. The capture script
leaves it alone because it is a parser-tolerance test, not a recording.
