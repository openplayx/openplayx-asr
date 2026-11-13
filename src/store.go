package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Job states. "cancelling" lasts until native work really stops.
const (
	stQueued     = "queued"
	stRunning    = "running"
	stCancelling = "cancelling"
	stSucceeded  = "succeeded"
	stFailed     = "failed"
	stCancelled  = "cancelled"
)

func terminal(s string) bool { return s == stSucceeded || s == stFailed || s == stCancelled }

type Job struct {
	ID          string    `json:"id"`
	User        string    `json:"-"`
	IdemKey     string    `json:"-"`
	SHA256      string    `json:"input_sha256"`
	Model       string    `json:"model"`
	Language    string    `json:"language"`
	Status      string    `json:"status"`
	AudioMs     int64     `json:"audio_ms"`
	ProcessedMs int64     `json:"processed_ms"`
	ErrorCode   string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	FinishedAt  time.Time `json:"-"`
}

// persisted form keeps the private fields.
type jobRecord struct {
	Job
	User      string    `json:"user"`
	IdemKey   string    `json:"idempotency_key"`
	ErrorCode string    `json:"error_code,omitempty"`
	Finished  time.Time `json:"finished_at"`
}

func (j *Job) view(position int) map[string]any {
	v := map[string]any{
		"id": j.ID, "status": j.Status, "model": j.Model, "language": j.Language,
		"audio_ms": j.AudioMs, "processed_ms": j.ProcessedMs,
		"created_at": j.CreatedAt, "updated_at": j.UpdatedAt,
		"result_available": j.Status == stSucceeded,
	}
	if position > 0 {
		v["queue_position"] = position
	}
	if j.ErrorCode != "" {
		v["error"] = map[string]string{"code": j.ErrorCode}
	}
	return v
}

var (
	errQueueFull = errors.New("queue_full")
	errConflict  = errors.New("idempotency_conflict")
	errActive    = errors.New("job_active")
	errNotFound  = errors.New("not_found")
)

// Store keeps jobs on disk (<data>/jobs/<id>/{job.json,input.wav,result.json})
// and runs them one at a time. Inputs are removed once a job is terminal.
type Store struct {
	dir     string
	limits  Limits
	models  map[string]*loadedModel
	mu      sync.Mutex
	jobs    map[string]*Job
	queue   []string
	cancels map[string]context.CancelFunc
	wake    chan struct{}
	now     func() time.Time
}

type loadedModel struct {
	cfg   ModelConfig
	model Model
	info  ModelInfo
}

func newStore(dir string, limits Limits, models map[string]*loadedModel) (*Store, error) {
	s := &Store{
		dir: filepath.Join(dir, "jobs"), limits: limits, models: models,
		jobs: map[string]*Job{}, cancels: map[string]context.CancelFunc{},
		wake: make(chan struct{}, 1), now: time.Now,
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, err
	}
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name(), "job.json"))
		if err != nil {
			_ = os.RemoveAll(filepath.Join(s.dir, e.Name()))
			continue
		}
		var r jobRecord
		if json.Unmarshal(data, &r) != nil || r.ID != e.Name() {
			continue
		}
		j := r.Job
		j.User, j.IdemKey, j.ErrorCode, j.FinishedAt = r.User, r.IdemKey, r.ErrorCode, r.Finished
		switch j.Status {
		case stRunning, stQueued:
			// Restart: the stored input is processed again from the start.
			j.Status, j.ProcessedMs = stQueued, 0
		case stCancelling:
			j.Status, j.ErrorCode, j.FinishedAt = stCancelled, "cancelled", s.now()
		}
		s.jobs[j.ID] = &j
	}
	var queued []*Job
	for _, j := range s.jobs {
		if j.Status == stQueued {
			queued = append(queued, j)
		}
		_ = s.save(j)
	}
	sort.Slice(queued, func(a, b int) bool { return queued[a].CreatedAt.Before(queued[b].CreatedAt) })
	for _, j := range queued {
		s.queue = append(s.queue, j.ID)
	}
	return s, nil
}

func (s *Store) jobDir(id string) string { return filepath.Join(s.dir, id) }

func (s *Store) save(j *Job) error {
	data, _ := json.Marshal(jobRecord{Job: *j, User: j.User, IdemKey: j.IdemKey, ErrorCode: j.ErrorCode, Finished: j.FinishedAt})
	tmp := filepath.Join(s.jobDir(j.ID), "job.json.part")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.jobDir(j.ID), "job.json"))
}

func newJobID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "job_" + hex.EncodeToString(b)
}

// Create registers an uploaded input (already validated, at tmpInput).
// The same user + idempotency key returns the existing job.
func (s *Store) Create(user, key, sha, model, lang string, audioMs int64, tmpInput string) (Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.User == user && j.IdemKey == key {
			_ = os.Remove(tmpInput)
			if j.SHA256 != sha || j.Model != model || j.Language != lang {
				return Job{}, false, errConflict
			}
			return *j, false, nil
		}
	}
	if len(s.queue) >= s.limits.MaxQueue {
		_ = os.Remove(tmpInput)
		return Job{}, false, errQueueFull
	}
	now := s.now()
	j := &Job{ID: newJobID(), User: user, IdemKey: key, SHA256: sha, Model: model, Language: lang,
		Status: stQueued, AudioMs: audioMs, CreatedAt: now, UpdatedAt: now}
	if err := os.MkdirAll(s.jobDir(j.ID), 0o700); err != nil {
		return Job{}, false, err
	}
	if err := os.Rename(tmpInput, filepath.Join(s.jobDir(j.ID), "input.wav")); err != nil {
		_ = os.RemoveAll(s.jobDir(j.ID))
		return Job{}, false, err
	}
	if err := s.save(j); err != nil {
		_ = os.RemoveAll(s.jobDir(j.ID))
		return Job{}, false, err
	}
	s.jobs[j.ID] = j
	s.queue = append(s.queue, j.ID)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return *j, true, nil
}

// Get returns a copy, only for its owner (other users get not found, so
// job ids cannot be probed).
func (s *Store) Get(user, id string) (Job, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.User != user {
		return Job{}, 0, errNotFound
	}
	pos := 0
	for i, q := range s.queue {
		if q == id {
			pos = i + 1
		}
	}
	return *j, pos, nil
}

func (s *Store) FindByKey(user, key string) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Job
	for _, j := range s.jobs {
		if j.User == user && j.IdemKey == key {
			out = append(out, *j)
		}
	}
	return out
}

func (s *Store) resultPath(id string) string { return filepath.Join(s.jobDir(id), "result.json") }

func (s *Store) Cancel(user, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.User != user {
		return Job{}, errNotFound
	}
	switch j.Status {
	case stQueued:
		for i, q := range s.queue {
			if q == id {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				break
			}
		}
		s.finishLocked(j, stCancelled, "cancelled")
	case stRunning:
		j.Status, j.UpdatedAt = stCancelling, s.now()
		_ = s.save(j)
		if c := s.cancels[id]; c != nil {
			c()
		}
	}
	return *j, nil
}

func (s *Store) Delete(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.User != user {
		return errNotFound
	}
	if !terminal(j.Status) {
		return errActive
	}
	delete(s.jobs, id)
	return os.RemoveAll(s.jobDir(id))
}

func (s *Store) finishLocked(j *Job, status, code string) {
	j.Status, j.ErrorCode = status, code
	j.UpdatedAt, j.FinishedAt = s.now(), s.now()
	_ = os.Remove(filepath.Join(s.jobDir(j.ID), "input.wav"))
	_ = s.save(j)
}

// Run is the single worker: business concurrency is 1 per process.
func (s *Store) Run(ctx context.Context) {
	for {
		s.mu.Lock()
		var j *Job
		if len(s.queue) > 0 {
			j = s.jobs[s.queue[0]]
			s.queue = s.queue[1:]
		}
		var jobCtx context.Context
		var cancel context.CancelFunc
		if j != nil {
			jobCtx, cancel = context.WithTimeout(ctx, s.limits.jobTimeout())
			s.cancels[j.ID] = cancel
			j.Status, j.UpdatedAt = stRunning, s.now()
			_ = s.save(j)
		}
		s.mu.Unlock()
		if j == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			case <-time.After(time.Minute):
				s.Cleanup()
				continue
			}
		}
		s.process(jobCtx, j)
		cancel()
		s.mu.Lock()
		delete(s.cancels, j.ID)
		s.mu.Unlock()
	}
}

func (s *Store) process(ctx context.Context, j *Job) {
	m := s.models[j.Model]
	if m == nil {
		s.mu.Lock()
		s.finishLocked(j, stFailed, "model_unknown")
		s.mu.Unlock()
		return
	}
	start := time.Now()
	raw, err := m.model.Transcribe(ctx, TranscribeRequest{
		Input: filepath.Join(s.jobDir(j.ID), "input.wav"), Language: j.Language,
		Threads: s.limits.Threads, WindowMs: 30000, OverlapMs: 1000,
	}, func(done, total int64) {
		s.mu.Lock()
		if j.Status == stRunning {
			j.ProcessedMs, j.UpdatedAt = done, s.now()
		}
		s.mu.Unlock()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		code := engineCode(err)
		switch {
		case j.Status == stRunning && ctx.Err() == context.Canceled:
			// Service shutdown, not a user cancel: run again after restart.
			j.Status, j.ProcessedMs, j.UpdatedAt = stQueued, 0, s.now()
			_ = s.save(j)
		case j.Status == stCancelling || code == "cancelled" && ctx.Err() == context.Canceled:
			s.finishLocked(j, stCancelled, "cancelled")
		case ctx.Err() == context.DeadlineExceeded:
			s.finishLocked(j, stFailed, "timeout")
		default:
			s.finishLocked(j, stFailed, code)
		}
		return
	}
	if j.Status == stCancelling {
		// Cancelled while finishing: never publish the late result.
		s.finishLocked(j, stCancelled, "cancelled")
		return
	}
	var native map[string]any
	if json.Unmarshal(raw, &native) != nil {
		s.finishLocked(j, stFailed, "result_invalid")
		return
	}
	native["schema_version"] = 1
	native["timebase"] = "audio_ms"
	native["model_id"] = m.cfg.ID
	native["model_revision"] = m.cfg.SHA256[:12]
	native["timestamp_level"] = "segment"
	info := engineInfo
	native["engine"], native["engine_version"] = info.Engine, info.Version
	data, _ := json.Marshal(native)
	tmp := s.resultPath(j.ID) + ".part"
	if os.WriteFile(tmp, data, 0o600) != nil || os.Rename(tmp, s.resultPath(j.ID)) != nil {
		s.finishLocked(j, stFailed, "storage_failed")
		return
	}
	j.ProcessedMs = j.AudioMs
	s.finishLocked(j, stSucceeded, "")
	log.Printf("job %s succeeded in %s (audio %d ms)", j.ID, time.Since(start).Round(time.Millisecond), j.AudioMs)
}

// Cleanup removes terminal jobs (input and output) after the TTL.
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-s.limits.ttl())
	for id, j := range s.jobs {
		if terminal(j.Status) && j.FinishedAt.Before(cutoff) {
			delete(s.jobs, id)
			_ = os.RemoveAll(s.jobDir(id))
		}
	}
}

// engineInfo is filled at startup (also used in results).
var engineInfo EngineInfo
