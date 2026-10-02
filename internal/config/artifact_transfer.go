package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"net"
	"net/url"
	"time"
)

// ArtifactTransferConfig pins transfer verification and direct recipient peers.
// PublicKeys are base64 Ed25519 public keys; this surface stores no private key.
// Changing Epoch revokes prior unexecuted authority. All fields require restart.
type ArtifactTransferConfig struct {
	// LegacyWritersDrained confirms all writers sharing these stores honor fences.
	LegacyWritersDrained bool              `yaml:"legacy_writers_drained"`
	Audience             string            `yaml:"audience"`
	Epoch                uint64            `yaml:"epoch"`
	PublicKeys           map[string]string `yaml:"public_keys"`
	Peers                map[string]string `yaml:"peers,omitempty"`
	MaxBytes             int64             `yaml:"max_bytes"`
	Timeout              time.Duration     `yaml:"timeout"`
	AllowLoopbackHTTP    bool              `yaml:"allow_loopback_http,omitempty"`
}

func (c *Config) validateArtifactTransfer() error {
	x := c.Artifacts.Transfer
	if x == nil {
		return nil
	}
	switch c.Artifacts.Driver {
	case "inmem", "sqlite", "postgres":
	default:
		return fieldError("artifacts.transfer", "requires an atomic scope-fencing artifact driver: inmem, sqlite, or postgres; fs and s3 are unsupported")
	}
	if !x.LegacyWritersDrained {
		return fieldError("artifacts.transfer.legacy_writers_drained", "must explicitly confirm every shared-store writer supports atomic scope fencing or has stopped")
	}
	if x.Audience == "" || x.Epoch == 0 || len(x.PublicKeys) == 0 || x.MaxBytes <= 0 || x.MaxBytes > 64<<20 || x.Timeout <= 0 || x.Timeout > time.Minute {
		return fieldError("artifacts.transfer", "requires audience, positive epoch, keys, max_bytes (1..67108864), timeout (0..1m]")
	}
	for id, value := range x.PublicKeys {
		k, e := base64.StdEncoding.DecodeString(value)
		if id == "" || e != nil || len(k) != ed25519.PublicKeySize {
			return fieldError("artifacts.transfer.public_keys", "requires named base64 Ed25519 public keys")
		}
	}
	for audience, origin := range x.Peers {
		u, e := url.Parse(origin)
		if e != nil || audience == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fieldError("artifacts.transfer.peers", "requires exact trusted origins without path, userinfo, query, or fragment")
		}
		if u.Scheme != "https" {
			ip := net.ParseIP(u.Hostname())
			if u.Scheme != "http" || !x.AllowLoopbackHTTP || ip == nil || !ip.IsLoopback() {
				return fieldError("artifacts.transfer.peers", "requires HTTPS (explicit loopback HTTP is development-only)")
			}
		}
	}
	return nil
}
