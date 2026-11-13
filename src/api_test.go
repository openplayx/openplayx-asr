package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModel blocks until released (or cancelled) so tests control timing.
type fakeModel struct {
	mu      sync.Mutex
	release chan struct{}
	calls   int
}

func (m *fakeModel) Info() ModelInfo { return ModelInfo{Type: "base", Multilingual: true} }
func (m *fakeModel) Close()          {}
func (m *fakeModel) Transcribe(ctx context.Context, req TranscribeRequest, progress func(int64, int64)) ([]byte, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	progress(500, 1000)
	select {
	case <-ctx.Done():
		return nil, &EngineError{"cancelled"}
	case <-m.release:
	}
	if _, err := os.Stat(req.Input); err != nil {
		return nil, &EngineError{"audio_unavailable"}
	}
	return []byte(`{"audio_ms":1000,"window_ms":30000,"overlap_ms":1000,"language":"en","windows":[{"offset_ms":0,"duration_ms":1000,"skipped":false,"segments":[{"start_ms":100,"end_ms":900,"text":" hi","no_speech_prob":0}]}]}`), nil
}

type fakeEngine struct{ model *fakeModel }

func (e fakeEngine) Info() EngineInfo {
	return EngineInfo{Available: true, Engine: "whisper.cpp", Version: "test"}
}
func (e fakeEngine) OpenModel(string) (Model, error) { return e.model, nil }

func wav(ms int, rate, channels uint32) []byte {
	n := ms * int(rate) / 1000 * int(channels)
	b := &bytes.Buffer{}
	w := func(v any) { _ = binary.Write(b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + n*2))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(channels))
	w(rate)
	w(rate * channels * 2)
	w(uint16(channels * 2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(n * 2))
	b.Write(make([]byte, n*2))
	return b.Bytes()
}

type env struct {
	t     *testing.T
	srv   *httptest.Server
	model *fakeModel
	cfg   *Config
	store *Store
	stop  context.CancelFunc
}

func newEnv(t *testing.T, dir string, limits Limits) *env {
	t.Helper()
	model := filepath.Join(dir, "model.bin")
	_ = os.WriteFile(model, []byte("weights"), 0o600)
	sum := sha256.Sum256([]byte("weights"))
	tokens := filepath.Join(dir, "tokens.json")
	_ = os.WriteFile(tokens, []byte(fmt.Sprintf(`{"tokens":[{"user":"alice","sha256":"%s"},{"user":"bob","sha256":"%s"}]}`,
		sha256Hex([]byte("alice-token")), sha256Hex([]byte("bob-token")))), 0o600)
	cfg := &Config{DataDir: filepath.Join(dir, "data"), TokensFile: tokens, Limits: limits,
		Models: []ModelConfig{{ID: "whisper-base-multilingual", Path: model, SHA256: hex.EncodeToString(sum[:])}}}
	cfg.applyDefaults()
	if err := cfg.validate(dir); err != nil {
		t.Fatal(err)
	}
	fm := &fakeModel{release: make(chan struct{})}
	api, store, _, err := setup(cfg, fakeEngine{fm})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { store.Run(ctx); close(done) }()
	srv := httptest.NewServer(api.Handler())
	e := &env{t: t, srv: srv, model: fm, cfg: cfg, store: store, stop: func() { stop(); <-done }}
	t.Cleanup(func() { srv.Close(); stop(); <-done })
	return e
}

func (e *env) do(method, path, token string, body []byte, headers map[string]string) (int, map[string]any) {
	req, _ := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func (e *env) upload(token, key string, audio []byte) (int, map[string]any) {
	return e.do("POST", "/v1/transcription-jobs?model=whisper-base-multilingual&language=en", token, audio,
		map[string]string{"Idempotency-Key": key, "X-Content-SHA256": sha256Hex(audio), "Content-Type": "audio/wav"})
}

func (e *env) waitStatus(token, id, want string) map[string]any {
	for i := 0; i < 200; i++ {
		_, j := e.do("GET", "/v1/transcription-jobs/"+id, token, nil, nil)
		if j["status"] == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("job %s never reached %s", id, want)
	return nil
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func TestAuthAndDocs(t *testing.T) {
	e := newEnv(t, t.TempDir(), Limits{})
	if s, _ := e.do("GET", "/v1/capabilities", "", nil, nil); s != 401 {
		t.Fatalf("no token: %d", s)
	}
	if s, _ := e.do("GET", "/v1/capabilities", "wrong", nil, nil); s != 401 {
		t.Fatalf("bad token: %d", s)
	}
	s, caps := e.do("GET", "/v1/capabilities", "alice-token", nil, nil)
	if s != 200 || caps["timebase"] != "audio_ms" || caps["translation"] != false {
		t.Fatalf("caps %d %v", s, caps)
	}
	if s, _ := e.do("GET", "/docs", "", nil, nil); s != 200 {
		t.Fatal("docs")
	}
	res, _ := http.Get(e.srv.URL + "/openapi.yaml")
	spec, _ := io.ReadAll(res.Body)
	if !strings.HasPrefix(string(spec), "openapi: 3.1.0") {
		t.Fatal("spec")
	}
}

func TestJobLifecycleAndContract(t *testing.T) {
	e := newEnv(t, t.TempDir(), Limits{})
	audio := wav(1000, 16000, 1)
	s, job := e.upload("alice-token", "key-0123456789abcdef", audio)
	if s != 202 || job["status"] != "queued" && job["status"] != "running" {
		t.Fatalf("create %d %v", s, job)
	}
	id := job["id"].(string)
	// Result is never an empty success before the job finished.
	if s, r := e.do("GET", "/v1/transcription-jobs/"+id+"/result", "alice-token", nil, nil); s != 409 || errCode(r) != "not_ready" {
		t.Fatalf("early result %d %v", s, r)
	}
	// Retry of the same upload returns the same job without a second run.
	s, again := e.upload("alice-token", "key-0123456789abcdef", audio)
	if s != 200 || again["id"] != id {
		t.Fatalf("idempotent retry %d %v", s, again)
	}
	_, found := e.do("GET", "/v1/transcription-jobs?idempotency_key=key-0123456789abcdef", "alice-token", nil, nil)
	if len(found["jobs"].([]any)) != 1 {
		t.Fatal("lookup by key")
	}
	// Same key, different audio: conflict.
	if s, r := e.upload("alice-token", "key-0123456789abcdef", wav(500, 16000, 1)); s != 409 || errCode(r) != "idempotency_conflict" {
		t.Fatalf("conflict %d %v", s, r)
	}
	// Other users cannot see the job.
	if s, _ := e.do("GET", "/v1/transcription-jobs/"+id, "bob-token", nil, nil); s != 404 {
		t.Fatal("isolation")
	}
	if s, _ := e.do("DELETE", "/v1/transcription-jobs/"+id, "alice-token", nil, nil); s != 409 {
		t.Fatal("active delete must be refused")
	}
	close(e.model.release)
	e.waitStatus("alice-token", id, "succeeded")
	s, result := e.do("GET", "/v1/transcription-jobs/"+id+"/result", "alice-token", nil, nil)
	if s != 200 || result["timebase"] != "audio_ms" || result["model_id"] != "whisper-base-multilingual" ||
		result["schema_version"].(float64) != 1 || len(result["windows"].([]any)) != 1 {
		t.Fatalf("result %d %v", s, result)
	}
	// Input is not kept after the job ended.
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "jobs", id, "input.wav")); !os.IsNotExist(err) {
		t.Fatal("input should be removed")
	}
	if s, _ := e.do("DELETE", "/v1/transcription-jobs/"+id, "alice-token", nil, nil); s != 204 {
		t.Fatal("delete")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "jobs", id)); !os.IsNotExist(err) {
		t.Fatal("job dir should be gone")
	}
	if e.model.calls != 1 {
		t.Fatalf("model ran %d times", e.model.calls)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, t.TempDir(), Limits{MaxUploadBytes: 64 << 10, MaxQueue: 1})
	audio := wav(1000, 16000, 1)
	cases := []struct {
		name    string
		headers map[string]string
		query   string
		body    []byte
		status  int
		code    string
	}{
		{"no key", map[string]string{"X-Content-SHA256": sha256Hex(audio)}, "model=whisper-base-multilingual", audio, 400, "idempotency_key_required"},
		{"bad sum", map[string]string{"Idempotency-Key": "k-0123456789abcdef", "X-Content-SHA256": sha256Hex([]byte("x"))}, "model=whisper-base-multilingual", audio, 400, "content_sha256_mismatch"},
		{"unknown model", map[string]string{"Idempotency-Key": "k-0123456789abcdef", "X-Content-SHA256": sha256Hex(audio)}, "model=nope", audio, 400, "model_unknown"},
		{"translate", map[string]string{"Idempotency-Key": "k-0123456789abcdef", "X-Content-SHA256": sha256Hex(audio)}, "model=whisper-base-multilingual&translate=1", audio, 400, "translation_not_supported"},
		{"stereo 44k", map[string]string{"Idempotency-Key": "k-0123456789abcdef", "X-Content-SHA256": sha256Hex(wav(100, 44100, 2))}, "model=whisper-base-multilingual", wav(100, 44100, 2), 415, "audio_format_unsupported"},
		{"too large", map[string]string{"Idempotency-Key": "k-0123456789abcdef", "X-Content-SHA256": sha256Hex(wav(3000, 16000, 1))}, "model=whisper-base-multilingual", wav(3000, 16000, 1), 413, "upload_too_large"},
	}
	for _, c := range cases {
		s, r := e.do("POST", "/v1/transcription-jobs?"+c.query, "alice-token", c.body, c.headers)
		if s != c.status || errCode(r) != c.code {
			t.Errorf("%s: %d %v", c.name, s, r)
		}
	}
	// Queue limit: first job runs (dequeued), second queues, third is refused.
	_, j1 := e.upload("alice-token", "q1-0123456789abcdef", audio)
	e.waitStatus("alice-token", j1["id"].(string), "running")
	e.upload("alice-token", "q2-0123456789abcdef", audio)
	if s, r := e.upload("alice-token", "q3-0123456789abcdef", audio); s != 503 || errCode(r) != "queue_full" {
		t.Fatalf("queue full %d %v", s, r)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, t.TempDir(), Limits{})
	audio := wav(1000, 16000, 1)
	_, running := e.upload("alice-token", "c1-0123456789abcdef", audio)
	e.waitStatus("alice-token", running["id"].(string), "running")
	_, queued := e.upload("alice-token", "c2-0123456789abcdef", audio)
	// Queued: cancelled immediately.
	s, q := e.do("POST", "/v1/transcription-jobs/"+queued["id"].(string)+"/cancel", "alice-token", nil, nil)
	if s != 200 || q["status"] != "cancelled" {
		t.Fatalf("queued cancel %d %v", s, q)
	}
	// Running: cancelling until the engine stops, then cancelled; no result.
	s, r := e.do("POST", "/v1/transcription-jobs/"+running["id"].(string)+"/cancel", "alice-token", nil, nil)
	if s != 202 && r["status"] != "cancelled" {
		t.Fatalf("running cancel %d %v", s, r)
	}
	e.waitStatus("alice-token", running["id"].(string), "cancelled")
	if s, _ := e.do("GET", "/v1/transcription-jobs/"+running["id"].(string)+"/result", "alice-token", nil, nil); s != 409 {
		t.Fatal("cancelled job must not have a result")
	}
	// Idempotent.
	if s, _ := e.do("POST", "/v1/transcription-jobs/"+running["id"].(string)+"/cancel", "alice-token", nil, nil); s != 200 {
		t.Fatal("repeat cancel")
	}
}

func TestRestartRequeuesAndTTL(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir, Limits{})
	_, job := e.upload("alice-token", "r1-0123456789abcdef", wav(1000, 16000, 1))
	id := job["id"].(string)
	e.waitStatus("alice-token", id, "running")
	e.stop() // service shutdown while running
	time.Sleep(50 * time.Millisecond)
	e.srv.Close()

	e2 := newEnv(t, dir, Limits{})
	close(e2.model.release)
	e2.waitStatus("alice-token", id, "succeeded")
	// TTL removes finished jobs.
	e2.store.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	e2.store.Cleanup()
	if s, _ := e2.do("GET", "/v1/transcription-jobs/"+id, "alice-token", nil, nil); s != 404 {
		t.Fatal("expired job should be removed")
	}
}

func TestConfigRejectsUnknownFieldsAndBadModel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"data_dir":"d","tokens_file":"t","models":[],"oops":1}`), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("unknown field accepted")
	}
	m := filepath.Join(dir, "m.bin")
	_ = os.WriteFile(m, []byte("x"), 0o600)
	if err := verifyModel(ModelConfig{ID: "m", Path: m, SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
}
