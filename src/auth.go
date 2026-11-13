package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

// Tokens maps SHA-256(token) → user. The plain tokens are never stored.
type Tokens struct{ byHash map[[32]byte]string }

type tokensFile struct {
	Tokens []struct {
		User   string `json:"user"`
		SHA256 string `json:"sha256"`
	} `json:"tokens"`
}

func loadTokens(path string) (*Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f tokensFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	t := &Tokens{byHash: map[[32]byte]string{}}
	for _, e := range f.Tokens {
		raw, err := hex.DecodeString(e.SHA256)
		if err != nil || len(raw) != 32 || e.User == "" {
			return nil, errors.New("tokens: each entry needs user and a 64-hex sha256")
		}
		t.byHash[[32]byte(raw)] = e.User
	}
	if len(t.byHash) == 0 {
		return nil, errors.New("tokens: no tokens configured (anonymous access is not supported)")
	}
	return t, nil
}

func (t *Tokens) user(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	for hash, user := range t.byHash {
		if subtle.ConstantTimeCompare(hash[:], sum[:]) == 1 {
			return user, true
		}
	}
	return "", false
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
