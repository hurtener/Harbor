package serve

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts/transfer"
)

func buildArtifactTransfer(in MuxInput) (*transfer.Service, error) {
	c := in.Cfg.Artifacts.Transfer
	if c == nil {
		return nil, nil
	}
	keys := map[string]ed25519.PublicKey{}
	for id, encoded := range c.PublicKeys {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("artifact transfer public key: %w", err)
		}
		keys[id] = ed25519.PublicKey(key)
	}
	sender, err := transfer.NewHTTPSender(transfer.HTTPConfig{Peers: c.Peers, Timeout: c.Timeout, AllowLoopbackHTTP: c.AllowLoopbackHTTP})
	if err != nil {
		return nil, fmt.Errorf("artifact transfer peer config: %w", err)
	}
	svc, err := transfer.New(transfer.Config{LegacyWritersDrained: c.LegacyWritersDrained, Audience: c.Audience, Epoch: c.Epoch, MaxBytes: c.MaxBytes, Keys: keys, Artifacts: in.Artifacts, State: in.State, Bus: in.Bus, Sender: sender, Clock: time.Now})
	if err != nil {
		sender.Close()
		return nil, fmt.Errorf("artifact transfer assembly: %w", err)
	}
	return svc, nil
}

// artifactTransferMux mounts the exact signed import edge outside ordinary
// Protocol authentication. This is not anonymous artifact upload: the service
// independently verifies signed authority AND the recipient's prior owner-
// authenticated durable admission before it reads a byte body.
func artifactTransferMux(s *transfer.Service, next http.Handler) http.Handler {
	if s == nil {
		return next
	}
	edge := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == transfer.ImportPath {
			edge.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
