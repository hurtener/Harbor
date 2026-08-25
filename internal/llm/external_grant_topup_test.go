package llm

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func validTopUpPair() (ExternalGrant, ExternalGrant) {
	issued := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	current := ExternalGrant{
		Version: 1, KeyID: "key-1", Audience: "harbor-runtime", GrantID: "grant-1",
		RouteMode:      ExternalGrantRouteCoordinatorBound,
		OrganizationID: "org-1", RuntimeID: "runtime-1", TenantID: "tenant-1",
		UserID: "user-1", SessionID: "session-1", LogicalRunID: "run-1",
		LogicalCallID: "call-1", AttemptNonce: "nonce-1", Provider: "provider-1",
		ProviderModelID: "model-1", ProviderConnectionID: "connection-1",
		ProviderConnectionGeneration: 2, RouteID: "route-1",
		CredentialBindingHandle: "binding-1", CredentialAssetGeneration: 3,
		PolicyGeneration: 4, MaxReasoning: ReasoningMedium, MaxOutputTokens: 100,
		Lease: ComputeLease{
			LeaseID: "lease-1", Epoch: 7, TokenUnits: 10,
			ExpiresAt: issued.Add(10 * time.Minute),
		},
		IssuedAt: issued, ExpiresAt: issued.Add(5 * time.Minute), Signature: "signature-1",
	}
	successor := current
	successor.KeyID = "key-2"
	successor.Lease.Epoch++
	successor.Lease.TokenUnits = 100
	successor.IssuedAt = issued.Add(time.Minute)
	successor.ExpiresAt = issued.Add(6 * time.Minute)
	successor.Lease.ExpiresAt = issued.Add(11 * time.Minute)
	successor.Signature = "signature-2"
	return current, successor
}

func TestValidateExternalGrantTopUpSuccessor_AcceptsOnlyBoundedLeaseAndSigningRotation(t *testing.T) {
	current, successor := validTopUpPair()
	if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); err != nil {
		t.Fatalf("valid successor: %v", err)
	}

	// Renewal may retain either deadline when the shortened lifetime remains
	// positive and within the predecessor's signed window.
	successor.ExpiresAt = current.ExpiresAt
	successor.Lease.ExpiresAt = current.Lease.ExpiresAt
	if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); err != nil {
		t.Fatalf("unchanged deadlines: %v", err)
	}

	// A runtime-default predecessor keeps its exact empty route boundary.
	current.RouteMode = ExternalGrantRouteRuntimeDefault
	successor.RouteMode = ExternalGrantRouteRuntimeDefault
	current.Provider, successor.Provider = "", ""
	current.ProviderModelID, successor.ProviderModelID = "", ""
	current.ProviderConnectionID, successor.ProviderConnectionID = "", ""
	current.ProviderConnectionGeneration, successor.ProviderConnectionGeneration = 0, 0
	current.RouteID, successor.RouteID = "", ""
	current.CredentialBindingHandle, successor.CredentialBindingHandle = "", ""
	current.CredentialAssetGeneration, successor.CredentialAssetGeneration = 0, 0
	if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); err != nil {
		t.Fatalf("runtime-default successor: %v", err)
	}
	successor.Provider = "injected-provider"
	if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); !errors.Is(err, ErrExternalGrantInvalid) {
		t.Fatalf("runtime-default route injection = %v, want ErrExternalGrantInvalid", err)
	}

	// The legacy blank spelling is signed coordinator-bound authority. It is
	// valid only when the successor preserves that exact raw spelling.
	current, successor = validTopUpPair()
	current.RouteMode = ""
	successor.RouteMode = ""
	if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); err != nil {
		t.Fatalf("legacy blank route successor: %v", err)
	}
	successor.RouteMode = ExternalGrantRouteCoordinatorBound
	if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); !errors.Is(err, ErrExternalGrantInvalid) {
		t.Fatalf("legacy blank route normalization = %v, want ErrExternalGrantInvalid", err)
	}
}

func TestValidateExternalGrantTopUpSuccessor_RejectsEveryImmutableClaimDrift(t *testing.T) {
	tests := map[string]func(*ExternalGrant){
		"version":                        func(g *ExternalGrant) { g.Version++ },
		"audience":                       func(g *ExternalGrant) { g.Audience = "other-audience" },
		"grant id":                       func(g *ExternalGrant) { g.GrantID = "other-grant" },
		"route mode":                     func(g *ExternalGrant) { g.RouteMode = ExternalGrantRouteRuntimeDefault },
		"organization":                   func(g *ExternalGrant) { g.OrganizationID = "other-org" },
		"runtime":                        func(g *ExternalGrant) { g.RuntimeID = "other-runtime" },
		"tenant":                         func(g *ExternalGrant) { g.TenantID = "other-tenant" },
		"user":                           func(g *ExternalGrant) { g.UserID = "other-user" },
		"session":                        func(g *ExternalGrant) { g.SessionID = "other-session" },
		"logical run":                    func(g *ExternalGrant) { g.LogicalRunID = "other-run" },
		"logical call":                   func(g *ExternalGrant) { g.LogicalCallID = "other-call" },
		"attempt nonce":                  func(g *ExternalGrant) { g.AttemptNonce = "other-nonce" },
		"provider":                       func(g *ExternalGrant) { g.Provider = "other-provider" },
		"provider model":                 func(g *ExternalGrant) { g.ProviderModelID = "other-model" },
		"provider connection":            func(g *ExternalGrant) { g.ProviderConnectionID = "other-connection" },
		"provider connection generation": func(g *ExternalGrant) { g.ProviderConnectionGeneration++ },
		"route":                          func(g *ExternalGrant) { g.RouteID = "other-route" },
		"credential binding":             func(g *ExternalGrant) { g.CredentialBindingHandle = "other-binding" },
		"credential asset generation":    func(g *ExternalGrant) { g.CredentialAssetGeneration++ },
		"policy generation":              func(g *ExternalGrant) { g.PolicyGeneration++ },
		"reasoning ceiling":              func(g *ExternalGrant) { g.MaxReasoning = ReasoningHigh },
		"output ceiling":                 func(g *ExternalGrant) { g.MaxOutputTokens++ },
		"lease id":                       func(g *ExternalGrant) { g.Lease.LeaseID = "other-lease" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			current, successor := validTopUpPair()
			mutate(&successor)
			if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); !errors.Is(err, ErrExternalGrantInvalid) {
				t.Fatalf("Validate = %v, want ErrExternalGrantInvalid", err)
			}
		})
	}
}

func TestValidateExternalGrantTopUpSuccessor_RejectsNonMonotonicOverflowAndExcess(t *testing.T) {
	tests := map[string]func(*ExternalGrant, *ExternalGrant) int64{
		"zero requested units": func(_, _ *ExternalGrant) int64 { return 0 },
		"epoch unchanged": func(current, successor *ExternalGrant) int64 {
			successor.Lease.Epoch = current.Lease.Epoch
			return 100
		},
		"epoch skipped": func(current, successor *ExternalGrant) int64 {
			successor.Lease.Epoch = current.Lease.Epoch + 2
			return 100
		},
		"epoch overflow": func(current, successor *ExternalGrant) int64 {
			current.Lease.Epoch = math.MaxUint64
			successor.Lease.Epoch = 0
			return 100
		},
		"capacity unchanged": func(current, successor *ExternalGrant) int64 {
			successor.Lease.TokenUnits = current.Lease.TokenUnits
			return 10
		},
		"capacity excessive": func(_, successor *ExternalGrant) int64 {
			successor.Lease.TokenUnits = 111
			return 100
		},
		"capacity excessive despite matching remaining growth": func(_, successor *ExternalGrant) int64 {
			successor.Lease.TokenUnits = 111
			successor.Lease.ConsumedUnits = 1
			return 100
		},
		"remaining insufficient": func(_, successor *ExternalGrant) int64 {
			successor.Lease.TokenUnits = 99
			return 100
		},
		"consumption decreased": func(current, successor *ExternalGrant) int64 {
			current.Lease.ConsumedUnits = 2
			successor.Lease.ConsumedUnits = 1
			successor.Lease.TokenUnits = 101
			return 99
		},
		"consumption exceeds capacity": func(_, successor *ExternalGrant) int64 {
			successor.Lease.ConsumedUnits = successor.Lease.TokenUnits + 1
			return 100
		},
		"issued at rewound": func(current, successor *ExternalGrant) int64 {
			successor.IssuedAt = current.IssuedAt.Add(-time.Second)
			return 100
		},
		"grant expiry rewound": func(current, successor *ExternalGrant) int64 {
			successor.ExpiresAt = current.ExpiresAt.Add(-time.Nanosecond)
			return 100
		},
		"lease expiry rewound": func(current, successor *ExternalGrant) int64 {
			successor.Lease.ExpiresAt = current.Lease.ExpiresAt.Add(-time.Nanosecond)
			return 100
		},
		"grant lifetime widened": func(_, successor *ExternalGrant) int64 {
			successor.ExpiresAt = successor.IssuedAt.Add(5*time.Minute + time.Nanosecond)
			return 100
		},
		"lease lifetime widened": func(_, successor *ExternalGrant) int64 {
			successor.Lease.ExpiresAt = successor.IssuedAt.Add(10*time.Minute + time.Nanosecond)
			return 100
		},
		"grant lifetime arithmetic overflow": func(current, successor *ExternalGrant) int64 {
			current.IssuedAt = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
			current.ExpiresAt = time.Date(9998, 1, 1, 0, 0, 0, 0, time.UTC)
			current.Lease.ExpiresAt = current.ExpiresAt
			successor.IssuedAt = time.Date(2, 1, 1, 0, 0, 0, 0, time.UTC)
			successor.ExpiresAt = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			successor.Lease.ExpiresAt = successor.ExpiresAt
			return 100
		},
		"missing key id": func(_, successor *ExternalGrant) int64 {
			successor.KeyID = ""
			return 100
		},
		"missing signature": func(_, successor *ExternalGrant) int64 {
			successor.Signature = ""
			return 100
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			current, successor := validTopUpPair()
			requested := mutate(&current, &successor)
			if err := ValidateExternalGrantTopUpSuccessor(current, successor, requested); !errors.Is(err, ErrExternalGrantInvalid) {
				t.Fatalf("Validate = %v, want ErrExternalGrantInvalid", err)
			}
		})
	}
}

func TestValidateExternalGrantTopUpSuccessor_ReplayAndConcurrentValidation(t *testing.T) {
	current, successor := validTopUpPair()
	for range 2 {
		if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); err != nil {
			t.Fatalf("same response replay: %v", err)
		}
	}
	if err := ValidateExternalGrantTopUpSuccessor(successor, successor, 100); !errors.Is(err, ErrExternalGrantInvalid) {
		t.Fatalf("stale successor replay = %v, want ErrExternalGrantInvalid", err)
	}

	const workers = 100
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- ValidateExternalGrantTopUpSuccessor(current, successor, 100)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent validation: %v", err)
		}
	}
}

func FuzzValidateExternalGrantTopUpSuccessorImmutableStrings(f *testing.F) {
	f.Add(uint8(0), "changed")
	f.Add(uint8(12), "")
	f.Fuzz(func(t *testing.T, field uint8, value string) {
		current, successor := validTopUpPair()
		values := []*string{
			&successor.Audience, &successor.GrantID, &successor.OrganizationID,
			&successor.RuntimeID, &successor.TenantID, &successor.UserID,
			&successor.SessionID, &successor.LogicalRunID, &successor.LogicalCallID,
			&successor.AttemptNonce, &successor.Provider, &successor.ProviderModelID,
			&successor.ProviderConnectionID, &successor.RouteID,
			&successor.CredentialBindingHandle, &successor.Lease.LeaseID,
		}
		target := values[int(field)%len(values)]
		if value == *target {
			value += "-different"
		}
		*target = value
		if err := ValidateExternalGrantTopUpSuccessor(current, successor, 100); !errors.Is(err, ErrExternalGrantInvalid) {
			t.Fatalf("immutable field %d accepted value %q", field, value)
		}
	})
}
