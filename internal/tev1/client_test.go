package tev1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func loadRequest(t *testing.T, path string) Request {
	t.Helper()
	var r Request
	if err := json.Unmarshal(readFile(t, path), &r); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return r
}

// instant makes retry backoff free in tests.
func instant(c *Client) *Client {
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return c
}

func TestQuestionSetKeepsOrder(t *testing.T) {
	qs := QuestionSet{{Name: "z", Type: TypeNoul, Instructions: "a"}, {Name: "a", Type: TypeNoul, Instructions: "b"}, {Name: "m", Type: TypeNoul, Instructions: "c"}}
	b, err := json.Marshal(qs)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !(strings.Index(s, `"z"`) < strings.Index(s, `"a"`) && strings.Index(s, `"a"`) < strings.Index(s, `"m"`)) {
		t.Fatalf("order lost: %s", s)
	}
	var back QuestionSet
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 || back[0].Name != "z" || back[2].Name != "m" {
		t.Fatalf("round trip lost order: %+v", back)
	}
}

// TestFixtures replays every testdata/api/*.request.json / *.response.json pair
// through the client against an httptest server and checks both that the
// request we send is semantically identical to the fixture and that the
// response parses.
func TestFixtures(t *testing.T) {
	reqs, _ := filepath.Glob("../../testdata/api/*.request.json")
	if len(reqs) == 0 {
		t.Fatal("no fixtures found")
	}
	for _, rp := range reqs {
		name := strings.TrimSuffix(filepath.Base(rp), ".request.json")
		t.Run(name, func(t *testing.T) {
			wantReq := readFile(t, rp)
			respBody := readFile(t, strings.TrimSuffix(rp, ".request.json")+".response.json")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				var got, want any
				b := make([]byte, 1<<16)
				n, _ := r.Body.Read(b)
				if err := json.Unmarshal(b[:n], &got); err != nil {
					t.Errorf("sent invalid JSON: %v", err)
				}
				_ = json.Unmarshal(wantReq, &want)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("request differs from fixture\n got:  %s\n want: %s", b[:n], wantReq)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(respBody)
			}))
			defer srv.Close()
			c := &Client{BaseURL: srv.URL, Timeout: 5 * time.Second}
			resp, err := c.Decide(context.Background(), loadRequest(t, rp))
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Answers) == 0 {
				t.Fatal("no answers")
			}
		})
	}
}

func TestClassifyFixtureValues(t *testing.T) {
	req := loadRequest(t, "../../testdata/api/classify.request.json")
	resp, err := ParseResponse(readFile(t, "../../testdata/api/classify.response.json"), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Answers["is_datapoint"]; got.Choice != "yes" || got.Probabilities["yes"] != 0.93 {
		t.Errorf("is_datapoint = %+v", got)
	}
	if got := resp.Answers["importance"]; got.Score == nil || *got.Score != 3.2 || got.Probabilities["3"] != 0.6 {
		t.Errorf("importance = %+v", got)
	}
}

func TestArrayShapeAnswersMatchedByOrder(t *testing.T) {
	req := loadRequest(t, "../../testdata/api/arrayshape.request.json")
	resp, err := ParseResponse(readFile(t, "../../testdata/api/arrayshape.response.json"), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Answers["is_datapoint"].Choice != "no" || resp.Answers["kind"].Choice != "narrative" {
		t.Errorf("array answers mis-assigned: %+v", resp.Answers)
	}
	// score probabilities given as an array are re-keyed by level index
	if p := resp.Answers["importance"].Probabilities["1"]; p != 0.3 {
		t.Errorf("level 1 probability = %v", p)
	}
}

func TestParseResponseMalformed(t *testing.T) {
	req := loadRequest(t, "../../testdata/api/classify.request.json")
	good := `"is_datapoint":{"choice":"yes","probabilities":{"yes":0.9,"no":0.05,"unknown":0.05}},"kind":{"choice":"stat"},"importance":{"score":2}`
	tests := []struct {
		name, body string
	}{
		{"not json", `<html>502</html>`},
		{"empty object", `{}`},
		{"answers null", `{"answers":null}`},
		{"answers wrong type", `{"answers":"nope"}`},
		{"missing answer", `{"answers":{"is_datapoint":{"choice":"yes"},"kind":{"choice":"stat"}}}`},
		{"choice missing", `{"answers":{"is_datapoint":{},"kind":{"choice":"stat"},"importance":{"score":2}}}`},
		{"choice undeclared option", `{"answers":{"is_datapoint":{"choice":"maybe"},"kind":{"choice":"stat"},"importance":{"score":2}}}`},
		{"score out of range", `{"answers":{"is_datapoint":{"choice":"yes"},"kind":{"choice":"stat"},"importance":{"score":9}}}`},
		{"score not a number", `{"answers":{"is_datapoint":{"choice":"yes"},"kind":{"choice":"stat"},"importance":{"score":"high"}}}`},
		{"probability out of range", `{"answers":{"is_datapoint":{"choice":"yes","probabilities":{"yes":1.7}},"kind":{"choice":"stat"},"importance":{"score":2}}}`},
		{"probabilities wrong type", `{"answers":{"is_datapoint":{"choice":"yes","probabilities":"x"},"kind":{"choice":"stat"},"importance":{"score":2}}}`},
		{"array wrong length", `{"answers":[{"choice":"yes"}]}`},
		{"truncated", `{"answers":{` + good[:20]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseResponse([]byte(tt.body), req)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
	if _, err := ParseResponse([]byte(`{"answers":{`+good+`}}`), req); err != nil {
		t.Fatalf("good body rejected: %v", err)
	}
	noul := Request{Questions: QuestionSet{{Name: "q", Type: TypeNoul}}}
	if _, err := ParseResponse([]byte(`{"answers":{"q":{"noul":1.4}}}`), noul); !errors.Is(err, ErrMalformed) {
		t.Errorf("noul out of range accepted: %v", err)
	}
}

func okBody() []byte {
	return []byte(`{"answers":{"says_hello":{"noul":0.9}}}`)
}

func basicReq(t *testing.T) Request {
	return loadRequest(t, "../../testdata/api/basic.request.json")
}

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			http.Error(w, "loading model", http.StatusServiceUnavailable)
			return
		}
		w.Write(okBody())
	}))
	defer srv.Close()
	c := instant(&Client{BaseURL: srv.URL, Retries: 3})
	if _, err := c.Decide(context.Background(), basicReq(t)); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3", atomic.LoadInt32(&calls))
	}
}

func TestNoRetryOnClientErrorOrMalformed(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		isAPI  bool
	}{
		{"422", 422, `{"error":"too many questions"}`, true},
		{"404 model", 404, `{"error":"model not found"}`, true},
		{"malformed 200", 200, `{"nope":1}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c := instant(&Client{BaseURL: srv.URL, Retries: 3})
			_, err := c.Decide(context.Background(), basicReq(t))
			if err == nil {
				t.Fatal("expected error")
			}
			if atomic.LoadInt32(&calls) != 1 {
				t.Errorf("calls = %d, want 1 (no retry)", atomic.LoadInt32(&calls))
			}
			var ae *APIError
			if tt.isAPI != errors.As(err, &ae) {
				t.Errorf("APIError match = %v, want %v (err: %v)", !tt.isAPI, tt.isAPI, err)
			}
			if !tt.isAPI && !errors.Is(err, ErrMalformed) {
				t.Errorf("want ErrMalformed, got %v", err)
			}
		})
	}
}

func TestGivesUpAfterRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := instant(&Client{BaseURL: srv.URL, Retries: 2})
	_, err := c.Decide(context.Background(), basicReq(t))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("err = %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("calls = %d, want 3", atomic.LoadInt32(&calls))
	}
}

func TestTimeoutIsRetriedThenFails(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	c := instant(&Client{BaseURL: srv.URL, Retries: 1, Timeout: 40 * time.Millisecond})
	start := time.Now()
	_, err := c.Decide(context.Background(), basicReq(t))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2", atomic.LoadInt32(&calls))
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took too long: %v", time.Since(start))
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{BaseURL: srv.URL, Retries: 5, Backoff: time.Hour,
		sleep: func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }}
	_, err := c.Decide(ctx, basicReq(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRequestTooLarge(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"}
	req := basicReq(t)
	req.State = strings.Repeat("x", MaxRequestBytes)
	if _, err := c.Decide(context.Background(), req); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v", err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		v    string
		want bool
	}{
		{"0.35.0", true}, {"0.35.1", true}, {"0.36.0-rc1", true}, {"v0.40.2", true}, {"1.0.0", true},
		{"0.34.9", false}, {"0.9.0", false},
	}
	for _, tt := range tests {
		got, err := VersionAtLeast(tt.v, MinOllamaVersion)
		if err != nil || got != tt.want {
			t.Errorf("VersionAtLeast(%q) = %v, %v; want %v", tt.v, got, err, tt.want)
		}
	}
	if _, err := VersionAtLeast("garbage", "0.35.0"); err == nil {
		t.Error("garbage version accepted")
	}
}

func preflightServer(version string, models ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			json.NewEncoder(w).Encode(map[string]string{"version": version})
		case "/api/tags":
			var ms []map[string]string
			for _, m := range models {
				ms = append(ms, map[string]string{"name": m, "model": m})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": ms})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name    string
		version string
		models  []string
		model   string
		wantErr string // substring, "" = ok
	}{
		{"ok", "0.35.0", []string{"tev1:0.8b"}, "tev1:0.8b", ""},
		{"ok implicit latest", "0.36.1", []string{"tev1:latest"}, "tev1", ""},
		{"too old", "0.34.0", []string{"tev1:0.8b"}, "tev1:0.8b", "too old"},
		{"model missing", "0.35.0", []string{"llama3:8b"}, "tev1:0.8b", "ollama pull tev1:0.8b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := preflightServer(tt.version, tt.models...)
			defer srv.Close()
			c := &Client{BaseURL: srv.URL}
			v, err := c.Preflight(context.Background(), tt.model)
			if tt.wantErr == "" {
				if err != nil || v != tt.version {
					t.Fatalf("v=%q err=%v", v, err)
				}
				return
			}
			var pe *PreflightError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestPreflightUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url}
	_, err := c.Preflight(context.Background(), "tev1:0.8b")
	var pe *PreflightError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "cannot reach Ollama") {
		t.Fatalf("err = %v", err)
	}
}
