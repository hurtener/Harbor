package llm_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
)

func TestLiveKey_InitGetRotate(t *testing.T) {
	k := llm.NewLiveKey()
	if k.Get() != "" {
		t.Errorf("fresh holder Get = %q, want empty", k.Get())
	}
	k.Init("sk-boot")
	if k.Get() != "sk-boot" {
		t.Errorf("after Init Get = %q, want sk-boot", k.Get())
	}
	if err := k.Rotate("sk-new"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if k.Get() != "sk-new" {
		t.Errorf("after Rotate Get = %q, want sk-new", k.Get())
	}
}

func TestLiveKey_RotateEmpty_FailsLoud(t *testing.T) {
	k := llm.NewLiveKey()
	k.Init("sk-boot")
	if err := k.Rotate(""); !errors.Is(err, llm.ErrEmptyKey) {
		t.Errorf("Rotate(\"\") = %v, want ErrEmptyKey", err)
	}
	if k.Get() != "sk-boot" {
		t.Errorf("an empty rotate must not change the key; got %q", k.Get())
	}
}

func TestLiveKey_Fingerprint_NonReversible(t *testing.T) {
	if got := llm.Fingerprint(""); got != "sha256:" {
		t.Errorf("empty fingerprint = %q, want sha256:", got)
	}
	fp := llm.Fingerprint("sk-secret-value")
	if !strings.HasPrefix(fp, "sha256:") {
		t.Errorf("fingerprint %q missing sha256: prefix", fp)
	}
	if strings.Contains(fp, "sk-secret-value") {
		t.Errorf("fingerprint %q leaks the key", fp)
	}
	// Deterministic + same length.
	if llm.Fingerprint("sk-secret-value") != fp {
		t.Error("fingerprint not deterministic")
	}
	if llm.Fingerprint("other") == fp {
		t.Error("distinct keys collided")
	}
}

func TestProviderKeyRotator_RotateKey(t *testing.T) {
	k := llm.NewLiveKey()
	k.Init("sk-boot")
	r := llm.NewProviderKeyRotator("openrouter", k)

	// Empty provider targets the bound one.
	prov, fp, err := r.RotateKey("", "sk-new")
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if prov != "openrouter" {
		t.Errorf("resolved provider = %q, want openrouter", prov)
	}
	if fp != llm.Fingerprint("sk-new") {
		t.Errorf("fingerprint mismatch")
	}
	if k.Get() != "sk-new" {
		t.Errorf("holder not swapped: %q", k.Get())
	}

	// Matching provider is accepted.
	if _, _, err := r.RotateKey("openrouter", "sk-2"); err != nil {
		t.Errorf("matching provider rotate: %v", err)
	}

	// Wrong provider rejected, no swap.
	if _, _, err := r.RotateKey("openai", "sk-3"); !errors.Is(err, llm.ErrUnknownProvider) {
		t.Errorf("wrong provider = %v, want ErrUnknownProvider", err)
	}
	if k.Get() != "sk-2" {
		t.Errorf("rejected rotate must not swap; got %q", k.Get())
	}

	// Empty key rejected.
	if _, _, err := r.RotateKey("", ""); !errors.Is(err, llm.ErrEmptyKey) {
		t.Errorf("empty key = %v, want ErrEmptyKey", err)
	}
}

// TestLiveKey_ConcurrentReuse runs N≥100 concurrent Get (the read hot
// path) interleaved with Rotate (the admin write) against ONE shared
// holder under -race. No torn reads; every Get returns a valid value from
// the allowed set. CLAUDE.md §5/§11 concurrent-reuse contract (D-025).
func TestLiveKey_ConcurrentReuse(t *testing.T) {
	k := llm.NewLiveKey()
	k.Init("k0")
	allowed := map[string]struct{}{"k0": {}, "k1": {}, "k2": {}, "k3": {}}

	const readers = 100
	const writers = 16
	var readerWG, writerWG sync.WaitGroup
	stop := make(chan struct{})

	readerWG.Add(readers)
	for range readers {
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := k.Get()
				if _, ok := allowed[v]; !ok {
					t.Errorf("torn/unexpected read: %q", v)
					return
				}
			}
		}()
	}
	writerWG.Add(writers)
	for i := range writers {
		go func(i int) {
			defer writerWG.Done()
			keys := []string{"k1", "k2", "k3"}
			for j := range 200 {
				if err := k.Rotate(keys[(i+j)%len(keys)]); err != nil {
					t.Errorf("rotate: %v", err)
					return
				}
			}
		}(i)
	}
	// Writers do bounded work; once they drain, stop the readers.
	writerWG.Wait()
	close(stop)
	readerWG.Wait()
}
