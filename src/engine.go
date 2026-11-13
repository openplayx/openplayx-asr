package main

import (
	"context"
	"errors"
)

// Engine is the recognition library linked into this process. The native
// implementation (build tag asr_native) calls the opx_asr C ABI shared with
// the App; no whisper-cli, whisper-server or ffmpeg process is ever started.
type Engine interface {
	Info() EngineInfo
	OpenModel(path string) (Model, error)
}

type EngineInfo struct {
	Available bool   `json:"available"`
	Engine    string `json:"engine"`
	Version   string `json:"engine_version"`
}

type ModelInfo struct {
	Type         string `json:"type"`
	Multilingual bool   `json:"multilingual"`
}

type TranscribeRequest struct {
	Input     string // normalised 16 kHz mono s16 WAV
	Language  string
	Threads   int
	WindowMs  int
	OverlapMs int
}

// Model is one loaded model; Transcribe calls are serialised by the worker.
type Model interface {
	Info() ModelInfo
	// Transcribe returns the native result document (audio_ms windows).
	Transcribe(ctx context.Context, req TranscribeRequest, progress func(processedMs, totalMs int64)) ([]byte, error)
	Close()
}

// EngineError carries a short, stable code (never a raw native log).
type EngineError struct{ Code string }

func (e *EngineError) Error() string { return e.Code }

var errEngineUnavailable = &EngineError{"engine_unavailable"}

func engineCode(err error) string {
	var e *EngineError
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "recognition_failed"
}
