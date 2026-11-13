//go:build !asr_native

package main

// Built without the native library (e.g. `go vet`, unit tests with a fake
// engine). The server starts but reports recognition as unavailable.
type stubEngine struct{}

func newNativeEngine() Engine { return stubEngine{} }

func (stubEngine) Info() EngineInfo {
	return EngineInfo{Available: false, Engine: "whisper.cpp"}
}

func (stubEngine) OpenModel(string) (Model, error) { return nil, errEngineUnavailable }
