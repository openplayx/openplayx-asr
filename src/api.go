package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

//go:embed openapi.yaml
var openapiSpec []byte

//go:embed docs.html
var docsPage []byte

type API struct {
	store  *Store
	tokens *Tokens
	limits Limits
	tmpDir string
}

var (
	idemKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	langRe    = regexp.MustCompile(`^[a-z]{2,3}$`)
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapiSpec)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		_, _ = w.Write(docsPage)
	})
	auth := func(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			user, ok := a.tokens.user(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				fail(w, 401, "unauthorized")
				return
			}
			h(w, r, user)
		}
	}
	mux.HandleFunc("GET /v1/capabilities", auth(a.capabilities))
	mux.HandleFunc("POST /v1/transcription-jobs", auth(a.create))
	mux.HandleFunc("GET /v1/transcription-jobs", auth(a.list))
	mux.HandleFunc("GET /v1/transcription-jobs/{id}", auth(a.get))
	mux.HandleFunc("GET /v1/transcription-jobs/{id}/result", auth(a.result))
	mux.HandleFunc("POST /v1/transcription-jobs/{id}/cancel", auth(a.cancel))
	mux.HandleFunc("DELETE /v1/transcription-jobs/{id}", auth(a.delete))
	return mux
}

func (a *API) capabilities(w http.ResponseWriter, _ *http.Request, _ string) {
	var models []map[string]any
	for _, m := range a.store.models {
		models = append(models, map[string]any{
			"id": m.cfg.ID, "revision": m.cfg.SHA256[:12], "sha256": m.cfg.SHA256,
			"type": m.info.Type, "multilingual": m.info.Multilingual,
			"languages": map[bool]string{true: "auto|iso-639-1", false: "en"}[m.info.Multilingual],
		})
	}
	writeJSON(w, 200, map[string]any{
		"service": "openplayx-asr", "api_version": 1,
		"engine": engineInfo.Engine, "engine_version": engineInfo.Version,
		"models": models, "timestamp_level": "segment", "timebase": "audio_ms",
		"translation": false,
		"audio":       map[string]any{"format": "wav", "codec": "pcm_s16le", "sample_rate": 16000, "channels": 1},
		"limits": map[string]any{
			"max_upload_bytes": a.limits.MaxUploadBytes, "max_audio_ms": a.limits.MaxAudioMs,
			"max_queue": a.limits.MaxQueue, "concurrency": 1, "gpu": false,
		},
	})
}

func (a *API) create(w http.ResponseWriter, r *http.Request, user string) {
	key := r.Header.Get("Idempotency-Key")
	sum := r.Header.Get("X-Content-SHA256")
	model := r.URL.Query().Get("model")
	lang := r.URL.Query().Get("language")
	if lang == "" {
		lang = "auto"
	}
	switch {
	case !idemKeyRe.MatchString(key):
		fail(w, 400, "idempotency_key_required")
		return
	case !sha256Re.MatchString(sum):
		fail(w, 400, "content_sha256_required")
		return
	case r.URL.Query().Has("translate"):
		fail(w, 400, "translation_not_supported")
		return
	}
	m := a.store.models[model]
	if m == nil {
		fail(w, 400, "model_unknown")
		return
	}
	if lang != "auto" && (!langRe.MatchString(lang) || (!m.info.Multilingual && lang != "en")) {
		fail(w, 400, "language_unsupported")
		return
	}
	// Retried request for an existing job: answer without reading the body.
	if existing := a.store.FindByKey(user, key); len(existing) > 0 {
		j := existing[0]
		if j.SHA256 != sum || j.Model != model || j.Language != lang {
			fail(w, 409, "idempotency_conflict")
			return
		}
		writeJSON(w, 200, j.view(0))
		return
	}
	if r.ContentLength > a.limits.MaxUploadBytes {
		fail(w, 413, "upload_too_large")
		return
	}
	tmp, err := os.CreateTemp(a.tmpDir, "upload-*.wav")
	if err != nil {
		fail(w, 500, "storage_failed")
		return
	}
	defer os.Remove(tmp.Name()) // no-op once moved into the job directory
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, a.limits.MaxUploadBytes))
	_ = tmp.Close()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "upload_too_large")
		} else {
			fail(w, 400, "upload_incomplete")
		}
		return
	}
	if n == 0 || hex.EncodeToString(h.Sum(nil)) != sum {
		fail(w, 400, "content_sha256_mismatch")
		return
	}
	ms, err := wavDurationMs(tmp.Name())
	if err != nil {
		fail(w, 415, "audio_format_unsupported")
		return
	}
	if ms > a.limits.MaxAudioMs {
		fail(w, 413, "audio_too_long")
		return
	}
	j, created, err := a.store.Create(user, key, sum, model, lang, ms, tmp.Name())
	switch {
	case errors.Is(err, errConflict):
		fail(w, 409, "idempotency_conflict")
	case errors.Is(err, errQueueFull):
		w.Header().Set("Retry-After", "30")
		fail(w, 503, "queue_full")
	case err != nil:
		fail(w, 500, "storage_failed")
	case created:
		w.Header().Set("Location", "/v1/transcription-jobs/"+j.ID)
		writeJSON(w, 202, j.view(0))
	default:
		writeJSON(w, 200, j.view(0))
	}
}

func (a *API) list(w http.ResponseWriter, r *http.Request, user string) {
	key := r.URL.Query().Get("idempotency_key")
	if !idemKeyRe.MatchString(key) {
		fail(w, 400, "idempotency_key_required")
		return
	}
	jobs := []map[string]any{}
	for _, j := range a.store.FindByKey(user, key) {
		jobs = append(jobs, j.view(0))
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

func (a *API) get(w http.ResponseWriter, r *http.Request, user string) {
	j, pos, err := a.store.Get(user, r.PathValue("id"))
	if err != nil {
		fail(w, 404, "not_found")
		return
	}
	writeJSON(w, 200, j.view(pos))
}

func (a *API) result(w http.ResponseWriter, r *http.Request, user string) {
	j, _, err := a.store.Get(user, r.PathValue("id"))
	if err != nil {
		fail(w, 404, "not_found")
		return
	}
	if j.Status != stSucceeded {
		// Never an empty "success" for an unfinished job.
		fail(w, 409, "not_ready")
		return
	}
	f, err := os.Open(a.store.resultPath(j.ID))
	if err != nil {
		fail(w, 500, "storage_failed")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}

func (a *API) cancel(w http.ResponseWriter, r *http.Request, user string) {
	j, err := a.store.Cancel(user, r.PathValue("id"))
	if err != nil {
		fail(w, 404, "not_found")
		return
	}
	status := 200
	if !terminal(j.Status) {
		status = 202
	}
	writeJSON(w, status, j.view(0))
}

func (a *API) delete(w http.ResponseWriter, r *http.Request, user string) {
	switch err := a.store.Delete(user, r.PathValue("id")); {
	case errors.Is(err, errNotFound):
		fail(w, 404, "not_found")
	case errors.Is(err, errActive):
		fail(w, 409, "job_active")
	case err != nil:
		fail(w, 500, "storage_failed")
	default:
		w.WriteHeader(204)
	}
}

func uploadDir(data string) (string, error) {
	d := filepath.Join(data, "uploads")
	if err := os.RemoveAll(d); err != nil { // partial uploads from a crash
		return "", err
	}
	return d, os.MkdirAll(d, 0o700)
}
