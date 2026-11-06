package projection

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// catalogStampFormat versions the stamp's input encoding. Changing what the
// digest covers, or how, changes this value so no stamp minted by an older
// encoding can ever compare equal to a newer one.
const catalogStampFormat = "harbor.tools.catalog-stamp.v1"

// registryGenerationReporter is the optional owner-resolver surface the
// stamp needs (implemented by the MCP registry).
type registryGenerationReporter interface {
	RegistryEpoch() string
	RegistryGeneration() uint64
}

// CatalogStamp computes the input-addressed revision of the catalog
// projection for (id, agentID, view) without building the projection. The
// revision is a SHA-256 over every input the projection reads, so equal
// revisions imply equal structural projections. sessionScoped reports that a
// session overlay record exists for the request's session, so a structural
// cache must key the entry by session as well. The primitive result shape
// lets internal/tools/protocol satisfy its stamper seam structurally without
// importing this package.
//
// The digest covers:
//   - the stamp format;
//   - view and the principal (tenant, user, agent, and the session when an
//     overlay exists), so a revision is never meaningful across principals;
//   - the process-local catalog epoch and generation, and the MCP registry
//     epoch and generation (owner, logical-name and live-entry maps). Epochs
//     are random per instance, so a restart or another replica never repeats a
//     stamp;
//   - the agent active pointer (the lifecycle slot), every activation-fence
//     record, and the acting user's active pointer, as write identities;
//   - the session overlay record's write identity, or its absence.
//
// It deliberately excludes per-request annotations (approval policy, OAuth
// status, last-used, display modes): they are never cached and never part of
// a revision.
//
// The cost is four small StateStore reads. ok=false (with a nil error) means
// one of the wired dependencies cannot report generations; the caller must
// then build the projection on every request and report no revision. Errors
// are returned, never folded into ok=false.
//
// CatalogStamp performs no authorization; callers admit the request first.
func (a CatalogViewResolver) CatalogStamp(ctx context.Context, id identity.Identity, agentID, view string) (revision string, sessionScoped, ok bool, err error) {
	if agentID == "" || a.Catalog == nil || a.OwnerResolver == nil || a.Registry == nil {
		return "", false, false, errors.New("agentcfg/projection: catalog stamp requires agent, registry, catalog and source owner resolver")
	}
	if err := identity.Validate(id); err != nil {
		return "", false, false, err
	}
	catalogGen, isReporter := a.Catalog.(tools.CatalogGenerationReporter)
	if !isReporter {
		return "", false, false, nil
	}
	registryGen, isReporter := a.OwnerResolver.(registryGenerationReporter)
	if !isReporter {
		return "", false, false, nil
	}
	inputs, isReader := a.Registry.(agentcfg.CatalogInputReader)
	if !isReader {
		return "", false, false, nil
	}
	var overlayGen sessionoverlay.SlotGenerationReader
	if a.SessionOverlay != nil {
		var isOverlayReader bool
		if overlayGen, isOverlayReader = a.SessionOverlay.(sessionoverlay.SlotGenerationReader); !isOverlayReader {
			return "", false, false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", false, false, err
	}

	// Process-local generations are read first; the durable reads follow.
	// Order does not affect correctness: a structural cache re-stamps after
	// building and only inserts when both stamps agree.
	catEpoch, catGeneration := catalogGen.Epoch(), catalogGen.Generation()
	regEpoch, regGeneration := registryGen.RegistryEpoch(), registryGen.RegistryGeneration()

	q := identity.Quadruple{Identity: id}
	durable, inputsErr := inputs.CatalogInputs(ctx, q, agentID)
	if inputsErr != nil {
		return "", false, false, inputsErr
	}
	var overlayID string
	overlayPresent := false
	if overlayGen != nil {
		eventID, present, overlayErr := overlayGen.OverlaySlotGeneration(ctx, q, agentID)
		if overlayErr != nil {
			return "", false, false, overlayErr
		}
		overlayID, overlayPresent = string(eventID), present
	}

	h := sha256.New()
	field := func(v string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		h.Write(n[:])
		h.Write([]byte(v))
	}
	field(catalogStampFormat)
	field(view)
	field(id.TenantID)
	field(id.UserID)
	field(agentID)
	if overlayPresent {
		field("overlay")
		field(id.SessionID)
		field(overlayID)
	} else {
		field("no-overlay")
	}
	field(catEpoch)
	field(strconv.FormatUint(catGeneration, 10))
	field(regEpoch)
	field(strconv.FormatUint(regGeneration, 10))
	field(string(durable.AgentActive))
	field(string(durable.UserActive))
	field(strconv.Itoa(len(durable.Fences)))
	for _, fence := range durable.Fences {
		field(fence.Kind)
		field(string(fence.EventID))
	}
	return hex.EncodeToString(h.Sum(nil)), overlayPresent, true, nil
}
