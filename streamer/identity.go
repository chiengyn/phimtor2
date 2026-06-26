package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// loadOrCreateControlToken returns the streamer's persistent identity token,
// generating and storing it under <dataDir>/identity on first boot. This token
// is the streamer's control-plane credential: the manager presents it on every
// internal call, and the manager pins its fingerprint when an operator approves
// the streamer. Persisting it on the data volume keeps the streamer approved
// across restarts and redeploys.
func loadOrCreateControlToken(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "identity")
	if data, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}
