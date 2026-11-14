package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"sync"
	"time"
)

// Staged audio: an HTTP upload that is not yet a job. It lets a tool call
// (MCP create_transcription) refer to audio by a short reference instead of
// carrying the file. A reference is bound to the uploading user, the audio's
// SHA-256, size and duration, and expires; it is never a path or URL.

const (
	stagedTTL     = time.Hour
	stagedPerUser = 4
)

type stagedAudio struct {
	User    string
	Path    string
	SHA256  string
	AudioMs int64
	Size    int64
	Expires time.Time
}

type stagedUploads struct {
	mu   sync.Mutex
	refs map[string]*stagedAudio
	now  func() time.Time
}

func newStagedUploads() *stagedUploads {
	return &stagedUploads{refs: map[string]*stagedAudio{}, now: time.Now}
}

// sweepLocked deletes expired entries and their files.
func (s *stagedUploads) sweepLocked() {
	now := s.now()
	for ref, a := range s.refs {
		if !now.Before(a.Expires) {
			_ = os.Remove(a.Path)
			delete(s.refs, ref)
		}
	}
}

func (s *stagedUploads) put(a *stagedAudio) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	count := 0
	for _, x := range s.refs {
		if x.User == a.User {
			count++
		}
	}
	if count >= stagedPerUser {
		return "", &apiError{429, "too_many_staged_uploads"}
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	ref := "aud_" + hex.EncodeToString(b)
	a.Expires = s.now().Add(stagedTTL)
	s.refs[ref] = a
	return ref, nil
}

// get returns a copy for its owner; other users see not found.
func (s *stagedUploads) get(user, ref string) (stagedAudio, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	a, ok := s.refs[ref]
	if !ok || a.User != user {
		return stagedAudio{}, false
	}
	return *a, true
}

// consume drops a reference once a job owns its audio.
func (s *stagedUploads) consume(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.refs[ref]; ok {
		_ = os.Remove(a.Path)
		delete(s.refs, ref)
	}
}

// stage: POST /v1/audio-uploads. Same checks as job creation; returns an
// audio_ref for create_transcription.
func (a *API) stage(w http.ResponseWriter, r *http.Request, user string) {
	sum := r.Header.Get("X-Content-SHA256")
	if !sha256Re.MatchString(sum) {
		fail(w, 400, "content_sha256_required")
		return
	}
	path, ms, e := a.receiveWAV(w, r, sum)
	if e != nil {
		failErr(w, e)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		fail(w, 500, "storage_failed")
		return
	}
	entry := &stagedAudio{User: user, Path: path, SHA256: sum, AudioMs: ms, Size: info.Size()}
	ref, err := a.staged.put(entry)
	if err != nil {
		_ = os.Remove(path)
		failErr(w, err.(*apiError))
		return
	}
	writeJSON(w, 201, map[string]any{
		"audio_ref": ref, "sha256": sum, "audio_ms": ms, "size_bytes": entry.Size,
		"expires_at": entry.Expires.UTC().Format(time.RFC3339),
	})
}

// submitStaged creates a job from staged audio. The staged file is linked,
// not moved, so a retryable failure (queue full) keeps the reference usable.
func (a *API) submitStaged(user, key, ref, model, lang string) (Job, bool, *apiError) {
	if existing := a.store.FindByKey(user, key); len(existing) > 0 {
		// Retried operation: the reference may already be consumed.
		j := existing[0]
		if j.Model != model || j.Language != lang {
			return Job{}, false, &apiError{409, "idempotency_conflict"}
		}
		if st, ok := a.staged.get(user, ref); ok && st.SHA256 != j.SHA256 {
			return Job{}, false, &apiError{409, "idempotency_conflict"}
		}
		return j, false, nil
	}
	st, ok := a.staged.get(user, ref)
	if !ok {
		return Job{}, false, &apiError{404, "audio_ref_not_found"}
	}
	f, err := os.CreateTemp(a.tmpDir, "job-*.wav")
	if err != nil {
		return Job{}, false, &apiError{500, "storage_failed"}
	}
	link := f.Name()
	_ = f.Close()
	_ = os.Remove(link)
	if err := os.Link(st.Path, link); err != nil {
		return Job{}, false, &apiError{500, "storage_failed"}
	}
	j, created, e := a.submit(user, key, st.SHA256, model, lang, st.AudioMs, link)
	if e == nil {
		a.staged.consume(ref)
	}
	return j, created, e
}
