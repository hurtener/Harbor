package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/sdk/identity"
	"github.com/hurtener/Harbor/sdk/state"
	"github.com/hurtener/Harbor/sdk/tools"
	"github.com/hurtener/Harbor/sdk/tools/inproc"
)

const documentKind = "example.portable-context.document"
const documentID = "sample-document"
const maxSourceBytes = 128 * 1024

var errVersionConflict = errors.New("document changed: read its current version before editing; do not retry stale writes")

type document struct {
	ResourceID string `json:"resource_id"`
	Version    uint64 `json:"version"`
	Source     string `json:"source"`
	More       bool   `json:"more"`
}

type readInput struct {
	ResourceID string `json:"resource_id"`
}

type replaceInput struct {
	ResourceID      string `json:"resource_id"`
	ExpectedVersion uint64 `json:"expected_version"`
	Old             string `json:"old"`
	New             string `json:"new"`
}

type saveReceipt struct {
	ResourceID          string `json:"resource_id"`
	Version             uint64 `json:"version"`
	Saved               bool   `json:"saved"`
	RenderingVerified   bool   `json:"rendering_verified"`
	InteractionVerified bool   `json:"interaction_verified"`
}

// One small application-owned document, not a new Harbor resource subsystem.
// Context identity, never a model argument, chooses the document's state slot.
func loadDocument(ctx context.Context, store state.StateStore) (document, state.StateRecord, error) {
	id, ok := identity.From(ctx)
	if !ok || identity.Validate(id) != nil {
		return document{}, state.StateRecord{}, identity.ErrIdentityMissing
	}
	record, err := store.Load(ctx, identity.Quadruple{Identity: id}, documentKind)
	if err != nil {
		return document{}, state.StateRecord{}, err
	}
	var doc document
	if err := json.Unmarshal(record.Bytes, &doc); err != nil || doc.ResourceID != documentID || doc.Version == 0 || doc.More || len(doc.Source) > maxSourceBytes || !utf8.ValidString(doc.Source) {
		return document{}, state.StateRecord{}, errors.New("invalid sample document state")
	}
	return doc, record, nil
}

func saveDocument(ctx context.Context, store state.StateStore, previous state.EventID, doc document) error {
	id, ok := identity.From(ctx)
	if !ok || identity.Validate(id) != nil {
		return identity.ErrIdentityMissing
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	q := identity.Quadruple{Identity: id}
	return store.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: documentKind, ExpectedEventID: previous}}, state.StateRecord{
		ID: state.NewEventID(), Identity: q, Kind: documentKind, Version: 1, Bytes: body,
	})
}

func seedDocument(ctx context.Context, store state.StateStore) error {
	if _, _, err := loadDocument(ctx, store); !errors.Is(err, state.ErrNotFound) {
		return err
	}
	start := "<!doctype html><html><body><h1>Draft</h1><nav>Overview</nav><!--"
	end := "--><footer>Keep this footer</footer></body></html>"
	doc := document{ResourceID: documentID, Version: 7, Source: start + strings.Repeat("x", 14660-len(start)-len(end)) + end}
	if err := saveDocument(ctx, store, "", doc); errors.Is(err, state.ErrConditionFailed) {
		// Another process seeded the same slot. Do not overwrite its edits.
		_, _, err = loadDocument(ctx, store)
		return err
	} else {
		return err
	}
}

func replaceDocument(ctx context.Context, store state.StateStore, in replaceInput) (saveReceipt, error) {
	if in.ResourceID != documentID || in.Old == "" || !utf8.ValidString(in.Old) || !utf8.ValidString(in.New) || len(in.Old) > maxSourceBytes || len(in.New) > maxSourceBytes {
		return saveReceipt{}, errors.New("supply the sample resource ID and bounded exact UTF-8 replacement strings")
	}
	doc, record, err := loadDocument(ctx, store)
	if err != nil {
		return saveReceipt{}, err
	}
	if doc.Version != in.ExpectedVersion || doc.Version == ^uint64(0) {
		return saveReceipt{}, errVersionConflict
	}
	if strings.Count(doc.Source, in.Old) != 1 || len(doc.Source)-len(in.Old)+len(in.New) > maxSourceBytes {
		return saveReceipt{}, errors.New("old must occur exactly once and the resulting document must fit the sample limit")
	}
	doc.Source = strings.Replace(doc.Source, in.Old, in.New, 1)
	doc.Version++
	if err := saveDocument(ctx, store, record.ID, doc); errors.Is(err, state.ErrConditionFailed) {
		return saveReceipt{}, errVersionConflict
	} else if err != nil {
		// An ambiguous persistence acknowledgement is not a failed save.
		return saveReceipt{}, fmt.Errorf("save acknowledgement unavailable; inspect current state before another edit: %w", err)
	}
	return saveReceipt{ResourceID: doc.ResourceID, Version: doc.Version, Saved: true}, nil
}

func registerDocuments(store state.StateStore, catalog tools.ToolCatalog) error {
	if err := inproc.RegisterFunc(catalog, "document_read", func(ctx context.Context, in readInput) (document, error) {
		if in.ResourceID != documentID {
			return document{}, errors.New("unknown sample resource; use sample-document")
		}
		doc, _, err := loadDocument(ctx, store)
		return doc, err
	}, tools.WithLoading(tools.LoadingAlways), tools.WithSideEffect(tools.SideEffectRead),
		tools.WithDescription("Read sample-document completely, including its exact version. more=false means all source was returned.")); err != nil {
		return err
	}
	return inproc.RegisterFunc(catalog, "document_replace", func(ctx context.Context, in replaceInput) (saveReceipt, error) {
		return replaceDocument(ctx, store, in)
	}, tools.WithLoading(tools.LoadingAlways), tools.WithSideEffect(tools.SideEffectWrite),
		tools.WithPolicy(tools.ToolPolicy{TimeoutMS: 10000, RetryOn: []tools.ErrorClass{}}),
		tools.WithDescription("Replace one exact source fragment in sample-document using its observed version. A save does not verify rendering or interaction. Never blindly repeat a stale or uncertain write."))
}
