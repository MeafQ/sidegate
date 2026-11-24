package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

// ProofPath is the root-only file that holds what the nginx hook sends as ProofHeader.
func ProofPath(dataDir string) string { return filepath.Join(dataDir, "nginx-proof") }

// loadProof returns nil when no usable proof exists. A file others could read proves
// nothing, so it counts as none.
func loadProof(dataDir string) []byte {
	proof, err := readProof(dataDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("nginx proof: %v", err)
		}
		return nil
	}
	return proof
}

func readProof(dataDir string) ([]byte, error) {
	path := ProofPath(dataDir)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %o; only root may read it", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	proof := bytes.TrimSpace(raw)
	if len(proof) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return proof, nil
}

// VouchLocally is how root on the gateway asks the daemon directly: it can read the proof
// file, so it can already do anything the hook can.
func VouchLocally(dataDir string, h http.Header) error {
	proof, err := readProof(dataDir)
	if err != nil {
		return err
	}
	h.Set(AuthHeader, "local")
	h.Set(ProofHeader, string(proof))
	return nil
}
