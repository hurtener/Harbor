// Phase 111d (D-201) — the canonical skills surface, end-to-end with
// REAL drivers (§17.3): localdb SkillStore (FTS5 ladder), inmem
// events bus, patterns redactor, real ToolCatalog + the PRODUCTION
// builtin registration (the Phase-38/41 delegations).
//
// The E2E walks the acceptance-criteria pipeline:
//
//	importer.ImportAndStore (the surface `harbor skill import` wraps)
//	  → skill_search through the production registration (discovery)
//	  → skill_get (budgeted, redacted, normalised content)
//	  → skills.Directory.View (the `<skills_context>` producer) with
//	    operator pinning, projected via runctx.ProjectSkillsDirectory.
//
// Identity is asserted throughout (§6 rule 10): a skill imported
// under tenant A never surfaces in tenant B's search, get, or
// directory view. Failure modes (§17.3): a frontmatter-invalid
// import fails loud with the validator's sentinel; rm (Delete) of a
// missing name errors. Concurrency: parallel import-vs-search against
// one shared store + catalog under -race.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skilltools "github.com/hurtener/Harbor/internal/skills/tools"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

const phase111dFixture = `---
name: summarise-paper
title: Summarise a research paper
trigger: when the user asks for a compact paper summary
---
Compact a 10-page paper to a 3-bullet summary plus a verdict.

## Steps

- Read abstract and conclusion.
- Note the three most-cited prior works.
- Output bullet points, then the verdict.

## Preconditions

- The paper text is available as an artifact in the session store, fetched via artifact ref, with the full body readable end to end before any summarisation step begins, including figures, tables, citations, appendices, and supplementary material sections.
`

var (
	phase111dIDA = identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"}
	phase111dIDB = identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"}
)

type phase111dStack struct {
	bus     events.EventBus
	store   skills.SkillStore
	catalog tools.ToolCatalog
	rc      builtin.RegistryContext
}

func phase111dSetup(t *testing.T) *phase111dStack {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := localdb.New(skills.ConfigSnapshot{
		Driver: "localdb",
		DSN:    filepath.Join(t.TempDir(), "skills.sqlite"),
	}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	cat := tools.NewCatalog()
	// One always-visible fixture tool so the capability envelope is
	// non-empty (the imported skill declares no required tools, so it
	// passes the default-deny filter regardless).
	type ea struct{}
	type eo struct{}
	if err := inproc.RegisterFunc[ea, eo](cat, "kb_search",
		func(context.Context, ea) (eo, error) { return eo{}, nil },
		tools.WithDescription("fixture")); err != nil {
		t.Fatalf("register fixture tool: %v", err)
	}

	rc := builtin.RegistryContext{
		Catalog:    cat,
		SkillStore: store,
		Bus:        bus,
		Redactor:   auditpatterns.New(),
	}
	// The PRODUCTION registration path — the same call assemble makes.
	if err := builtin.RegisterWith(rc, []string{"skill_search", "skill_get", "skill_list", "skill_propose"}); err != nil {
		t.Fatalf("builtin.RegisterWith: %v", err)
	}
	return &phase111dStack{bus: bus, store: store, catalog: cat, rc: rc}
}

func phase111dWriteFixture(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summarise-paper.skill.md")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func phase111dCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func phase111dImporterDeps(t *testing.T) importer.Deps {
	t.Helper()
	// The importer demands an artifact store even for attachment-less
	// sources — the real inmem driver, per §17.3 (no mocks on seams).
	store, err := artinmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return importer.Deps{Store: store}
}

// TestE2E_Phase111d_ImportDiscoverGetInject is the acceptance-criteria
// pipeline end-to-end.
func TestE2E_Phase111d_ImportDiscoverGetInject(t *testing.T) {
	t.Parallel()
	stack := phase111dSetup(t)
	ctxA := phase111dCtx(t, phase111dIDA)

	// 1. Ingest — the surface `harbor skill import` wraps.
	rep, err := importer.ImportAndStore(ctxA, phase111dIDA, stack.store, phase111dImporterDeps(t),
		phase111dWriteFixture(t, phase111dFixture))
	if err != nil {
		t.Fatalf("ImportAndStore: %v", err)
	}
	if rep.Name != "summarise-paper" || rep.Steps != 3 {
		t.Fatalf("report = %+v, want summarise-paper with 3 steps", rep)
	}

	// 2. Discover — skill_search through the production registration.
	searchDesc, ok := stack.catalog.Resolve("skill_search")
	if !ok {
		t.Fatal("skill_search not registered")
	}
	rawSearch, _ := json.Marshal(builtin.SkillSearchArgs{Query: "paper summary"})
	searchRes, err := searchDesc.Invoke(ctxA, rawSearch)
	if err != nil {
		t.Fatalf("skill_search invoke: %v", err)
	}
	var sr skilltools.SearchResult
	srBody, _ := json.Marshal(searchRes.Value)
	if err := json.Unmarshal(srBody, &sr); err != nil {
		t.Fatalf("unmarshal search result: %v", err)
	}
	if len(sr.Skills) == 0 || sr.Skills[0].Skill.Name != "summarise-paper" {
		t.Fatalf("skill_search = %+v, want the imported skill ranked", sr.Skills)
	}
	if sr.Path == "" {
		t.Error("search Path empty — the localdb ladder branch must be surfaced")
	}

	// 3. Fetch — skill_get returns budgeted content; a tight budget
	// provably runs the ladder (Summarized=true → preconditions
	// dropped), the redaction/normalisation pipeline included.
	getDesc, _ := stack.catalog.Resolve("skill_get")
	rawGet, _ := json.Marshal(builtin.SkillGetArgs{Names: []string{"summarise-paper"}, MaxTokens: 100})
	getRes, err := getDesc.Invoke(ctxA, rawGet)
	if err != nil {
		t.Fatalf("skill_get invoke: %v", err)
	}
	var gr skilltools.GetResult
	grBody, _ := json.Marshal(getRes.Value)
	if err := json.Unmarshal(grBody, &gr); err != nil {
		t.Fatalf("unmarshal get result: %v", err)
	}
	if len(gr.Skills) != 1 || gr.Skills[0].Name != "summarise-paper" {
		t.Fatalf("skill_get = %+v, want the imported skill", gr.Skills)
	}
	if !gr.Summarized || len(gr.Skills[0].Preconditions) != 0 {
		t.Errorf("budgeter did not run: Summarized=%v Preconditions=%v", gr.Summarized, gr.Skills[0].Preconditions)
	}

	// 4. Inject — the Directory view (the `<skills_context>` producer)
	// lists the skill; operator pinning renders; the projection
	// carries the pinned-first compact shape.
	dir, err := skills.NewDirectory(stack.store, skills.Deps{Bus: stack.bus}, skills.DirectoryConfig{
		Pinned:     []string{"summarise-paper"},
		MaxEntries: 5,
	})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	views, err := dir.View(ctxA, skills.DirectoryCapability{
		AllowedTools: tools.VisibleNames(stack.catalog, tools.CatalogFilter{
			TenantID: phase111dIDA.TenantID, UserID: phase111dIDA.UserID, SessionID: phase111dIDA.SessionID,
		}),
	})
	if err != nil {
		t.Fatalf("Directory.View: %v", err)
	}
	if len(views) != 1 || views[0].Name != "summarise-paper" || !views[0].Pinned {
		t.Fatalf("Directory.View = %+v, want the imported skill pinned first", views)
	}
	injected := runctx.ProjectSkillsDirectory(views)
	if len(injected) != 1 {
		t.Fatalf("ProjectSkillsDirectory = %d entries, want 1", len(injected))
	}
	// Pin the prompt-block shape (the 111d prompt-delta mitigation):
	// compact SkillView keys only — no steps / no description.
	blob, _ := json.Marshal(injected)
	if !strings.Contains(string(blob), `"pinned":true`) || strings.Contains(string(blob), `"steps"`) {
		t.Errorf("injected block shape drifted: %s", blob)
	}

	// 5. Isolation (§6 rule 10): tenant B sees nothing anywhere.
	ctxB := phase111dCtx(t, phase111dIDB)
	resB, err := searchDesc.Invoke(ctxB, rawSearch)
	if err != nil {
		t.Fatalf("tenant-B skill_search: %v", err)
	}
	var srB skilltools.SearchResult
	srBBody, _ := json.Marshal(resB.Value)
	_ = json.Unmarshal(srBBody, &srB)
	if len(srB.Skills) != 0 {
		t.Errorf("tenant-B search leaked %d skills", len(srB.Skills))
	}
	viewsB, err := dir.View(ctxB, skills.DirectoryCapability{})
	if err != nil {
		t.Fatalf("tenant-B Directory.View: %v", err)
	}
	if len(viewsB) != 0 {
		t.Errorf("tenant-B directory view leaked %d skills", len(viewsB))
	}

	// 6. Failure modes (§17.3): invalid frontmatter fails loud with
	// the validator's sentinel; Delete of a missing name errors.
	_, err = importer.ImportAndStore(ctxA, phase111dIDA, stack.store, phase111dImporterDeps(t),
		phase111dWriteFixture(t, "not a skills file\n"))
	if !errors.Is(err, importer.ErrMissingFrontmatter) {
		t.Errorf("invalid import err = %v, want ErrMissingFrontmatter", err)
	}
	if err := stack.store.Delete(ctxA, identity.Quadruple{Identity: phase111dIDA}, "no-such-skill", skills.ScopeSession); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Errorf("Delete(missing) err = %v, want ErrSkillNotFound", err)
	}
}

// TestE2E_Phase111d_ConcurrentImportVsSearch — §17.3 concurrency
// stress: N concurrent importers + searchers + directory readers over
// ONE shared store/catalog under -race; per-goroutine identity, no
// cross-talk, no error.
func TestE2E_Phase111d_ConcurrentImportVsSearch(t *testing.T) {
	t.Parallel()
	stack := phase111dSetup(t)
	deps := phase111dImporterDeps(t)
	searchDesc, _ := stack.catalog.Resolve("skill_search")
	dir, err := skills.NewDirectory(stack.store, skills.Deps{Bus: stack.bus}, skills.DirectoryConfig{})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	const n = 24
	var wg sync.WaitGroup
	errCh := make(chan error, n*2)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%4),
				UserID:    "user",
				SessionID: fmt.Sprintf("sess-%d", i),
			}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("run-%d", i))
			if err != nil {
				errCh <- err
				return
			}
			fixture := strings.Replace(phase111dFixture, "name: summarise-paper",
				fmt.Sprintf("name: skill-%d", i), 1)
			if _, err := importer.ImportAndStore(ctx, id, stack.store, deps,
				phase111dWriteFixture(t, fixture)); err != nil {
				errCh <- fmt.Errorf("import[%d]: %w", i, err)
				return
			}
			raw, _ := json.Marshal(builtin.SkillSearchArgs{Query: "paper"})
			if _, err := searchDesc.Invoke(ctx, raw); err != nil {
				errCh <- fmt.Errorf("search[%d]: %w", i, err)
				return
			}
			views, err := dir.View(ctx, skills.DirectoryCapability{})
			if err != nil {
				errCh <- fmt.Errorf("view[%d]: %w", i, err)
				return
			}
			// Session-scoped isolation: this session imported exactly
			// one skill; the view must show exactly that one.
			if len(views) != 1 || views[0].Name != fmt.Sprintf("skill-%d", i) {
				errCh <- fmt.Errorf("view[%d]: cross-talk — got %+v", i, views)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
