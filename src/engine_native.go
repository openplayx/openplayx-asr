//go:build asr_native

package main

/*
// Search paths come from CGO_CFLAGS/CGO_LDFLAGS (scripts/build.sh points
// them at whisper-package's opx_asr.h, libopx_asr.a and the pinned
// whisper.cpp static libraries).
#cgo LDFLAGS: -lopx_asr -lwhisper -lggml -lggml-cpu -lggml-base -lm
#cgo darwin LDFLAGS: -framework Accelerate -framework Foundation -lc++
#cgo linux LDFLAGS: -lstdc++ -lpthread
#include <stdlib.h>
#include "opx_asr.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"time"
	"unsafe"
)

type nativeEngine struct{}

func newNativeEngine() Engine { return nativeEngine{} }

func takeString(p *C.char) string {
	if p == nil {
		return ""
	}
	defer C.opx_asr_string_free(p)
	return C.GoString(p)
}

func nativeCode(code C.int32_t) string {
	switch code {
	case C.OPX_ASR_E_INVALID_ARGUMENT:
		return "invalid_argument"
	case C.OPX_ASR_E_INPUT_UNAVAILABLE:
		return "audio_unavailable"
	case C.OPX_ASR_E_INPUT_UNSUPPORTED:
		return "audio_format_unsupported"
	case C.OPX_ASR_E_MODEL_UNAVAILABLE:
		return "model_unavailable"
	case C.OPX_ASR_E_LANGUAGE:
		return "language_unsupported"
	case C.OPX_ASR_E_CANCELLED:
		return "cancelled"
	case C.OPX_ASR_E_NO_MEMORY:
		return "out_of_memory"
	case C.OPX_ASR_E_UNAVAILABLE:
		return "engine_unavailable"
	default:
		return "recognition_failed"
	}
}

func (nativeEngine) Info() EngineInfo {
	if C.opx_asr_abi_version() != 1 {
		return EngineInfo{Engine: "whisper.cpp"}
	}
	var info EngineInfo
	_ = json.Unmarshal([]byte(takeString(C.opx_asr_info())), &info)
	return info
}

type nativeModel struct {
	handle *C.opx_asr_model
	info   ModelInfo
}

func (nativeEngine) OpenModel(path string) (Model, error) {
	opts, _ := json.Marshal(map[string]string{"path": path})
	c := C.CString(string(opts))
	defer C.free(unsafe.Pointer(c))
	var m *C.opx_asr_model
	if code := C.opx_asr_model_open(c, &m); code != 0 {
		return nil, &EngineError{nativeCode(code)}
	}
	var info ModelInfo
	_ = json.Unmarshal([]byte(takeString(C.opx_asr_model_info(m))), &info)
	return &nativeModel{handle: m, info: info}, nil
}

func (m *nativeModel) Info() ModelInfo { return m.info }
func (m *nativeModel) Close()          { C.opx_asr_model_close(m.handle) }

func (m *nativeModel) Transcribe(ctx context.Context, req TranscribeRequest, progress func(int64, int64)) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{
		"input": req.Input, "language": req.Language, "threads": req.Threads,
		"window_ms": req.WindowMs, "overlap_ms": req.OverlapMs, "translate": false,
	})
	c := C.CString(string(body))
	defer C.free(unsafe.Pointer(c))
	var job *C.opx_asr_job
	if code := C.opx_asr_job_create(m.handle, c, &job); code != 0 {
		return nil, &EngineError{nativeCode(code)}
	}
	if code := C.opx_asr_job_start(job); code != 0 {
		C.opx_asr_job_destroy(job)
		return nil, &EngineError{nativeCode(code)}
	}
	var ev C.opx_asr_event
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	cancelled := false
	for {
		if C.opx_asr_job_poll(job, &ev) > 0 && progress != nil && ev.state == C.OPX_ASR_STATE_RUNNING {
			progress(int64(ev.processed_ms), int64(ev.total_ms))
		}
		if ev.state >= C.OPX_ASR_STATE_SUCCEEDED {
			break
		}
		select {
		case <-ctx.Done():
			if !cancelled {
				// Cooperative: stays running until whisper observes it.
				C.opx_asr_job_cancel(job)
				cancelled = true
			}
			<-ticker.C
		case <-ticker.C:
		}
	}
	C.opx_asr_job_join(job)
	defer C.opx_asr_job_destroy(job)
	if ev.state != C.OPX_ASR_STATE_SUCCEEDED {
		return nil, &EngineError{nativeCode(ev.error)}
	}
	var out *C.char
	if code := C.opx_asr_job_result(job, &out); code != 0 {
		return nil, &EngineError{nativeCode(code)}
	}
	return []byte(takeString(out)), nil
}
