package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Config is read from a JSON file. Secrets are never stored in it: tokens
// live in a separate file holding only SHA-256 hashes.
type Config struct {
	Listen     string        `json:"listen"`
	DataDir    string        `json:"data_dir"`
	TokensFile string        `json:"tokens_file"`
	Models     []ModelConfig `json:"models"`
	Limits     Limits        `json:"limits"`
	// TLS (optional). Plain HTTP is meant for a reverse proxy or a LAN that
	// the App user explicitly opted into.
	TLSCert string `json:"tls_cert"`
	TLSKey  string `json:"tls_key"`
}

type ModelConfig struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type Limits struct {
	MaxUploadBytes int64 `json:"max_upload_bytes"`
	MaxAudioMs     int64 `json:"max_audio_ms"`
	MaxQueue       int   `json:"max_queue"`
	Threads        int   `json:"threads"`
	JobTimeoutSec  int   `json:"job_timeout_seconds"`
	TTLHours       int   `json:"ttl_hours"`
}

var modelID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{Listen: "127.0.0.1:8787"}
	dec := json.NewDecoder(bytesReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	return c, c.validate(filepath.Dir(path))
}

func (c *Config) applyDefaults() {
	l := &c.Limits
	if l.MaxUploadBytes == 0 {
		l.MaxUploadBytes = 200 << 20 // ~104 min of 16 kHz mono s16
	}
	if l.MaxAudioMs == 0 {
		l.MaxAudioMs = 3 * 3600 * 1000
	}
	if l.MaxQueue == 0 {
		l.MaxQueue = 8
	}
	if l.Threads == 0 {
		l.Threads = 4
	}
	if l.JobTimeoutSec == 0 {
		l.JobTimeoutSec = 4 * 3600
	}
	if l.TTLHours == 0 {
		l.TTLHours = 24
	}
}

func (c *Config) validate(base string) error {
	if c.DataDir == "" || c.TokensFile == "" {
		return errors.New("config: data_dir and tokens_file are required")
	}
	if len(c.Models) == 0 {
		return errors.New("config: at least one model is required")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if !modelID.MatchString(m.ID) || seen[m.ID] {
			return fmt.Errorf("config: bad or duplicate model id %q", m.ID)
		}
		seen[m.ID] = true
		if !filepath.IsAbs(m.Path) {
			m.Path = filepath.Join(base, m.Path)
		}
		if len(m.SHA256) != 64 {
			return fmt.Errorf("config: model %s needs a sha256", m.ID)
		}
	}
	if c.Limits.Threads < 1 || c.Limits.Threads > 16 || c.Limits.MaxQueue < 1 {
		return errors.New("config: limits out of range")
	}
	return nil
}

// verifyModel checks size and checksum before a model is loaded: a wrong
// or truncated file is refused instead of producing garbage.
func verifyModel(m ModelConfig) error {
	f, err := os.Open(m.Path)
	if err != nil {
		return fmt.Errorf("model %s: %w", m.ID, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if m.Bytes > 0 && n != m.Bytes {
		return fmt.Errorf("model %s: size %d, expected %d", m.ID, n, m.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("model %s: checksum mismatch", m.ID)
	}
	return nil
}

func (l Limits) jobTimeout() time.Duration { return time.Duration(l.JobTimeoutSec) * time.Second }
func (l Limits) ttl() time.Duration        { return time.Duration(l.TTLHours) * time.Hour }
