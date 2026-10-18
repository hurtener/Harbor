package appoperation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/appoperation"
)

func TestArgumentsDigest_CanonicalAndExact(t *testing.T) {
	a, err := appoperation.ArgumentsDigest(json.RawMessage(`{"z":9007199254740993,"a":"<x>"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := appoperation.ArgumentsDigest(json.RawMessage(`{ "a":"\u003cx\u003e", "z":9007199254740993 }`))
	if err != nil || a != b {
		t.Fatalf("equivalent input changed digest: %v", err)
	}
	c, err := appoperation.ArgumentsDigest(json.RawMessage(`{"z":9007199254740992,"a":"<x>"}`))
	if err != nil || a == c {
		t.Fatalf("integer precision lost: %v", err)
	}
	for _, raw := range []string{`null`, `[]`, `{"a":1,"a":2}`, `{"a":{"x":1,"x":2}}`, `{}{}`, `{"a":NaN}`, `{"a":`, strings.Repeat("[", 40) + strings.Repeat("]", 40), strings.Repeat(" ", 1<<20) + "{}"} {
		if _, err := appoperation.ArgumentsDigest(json.RawMessage(raw)); !errors.Is(err, appoperation.ErrInvalid) {
			t.Errorf("invalid input accepted (%d bytes): %v", len(raw), err)
		}
	}
}

func TestBinding_ActualDispatchConfinement(t *testing.T) {
	ref := strings.Repeat("a", 64)
	ctx, err := appoperation.WithCall(context.Background(), ref, "agent", "server", "ui://report", "generation", "save", json.RawMessage(`{"n":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	b, ok := appoperation.From(ctx)
	if !ok {
		t.Fatal("binding missing")
	}
	args, err := appoperation.DecodeArguments(json.RawMessage(`{"n":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.CheckCall("server", "save", args); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"other", "save"}, {"server", "delete"}} {
		if err := b.CheckCall(pair[0], pair[1], args); !errors.Is(err, appoperation.ErrInvalid) {
			t.Fatal("retarget allowed")
		}
	}
	args["n"] = json.Number("9007199254740992")
	if err := b.CheckCall("server", "save", args); !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatal("changed arguments allowed")
	}
	if err := b.CheckResource("server", "ui://report"); !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatal("tool used as resource")
	}
	readCtx, err := appoperation.WithRead(context.Background(), ref, "agent", "server", "ui://report")
	if err != nil {
		t.Fatal(err)
	}
	read, _ := appoperation.From(readCtx)
	if err := read.CheckResource("server", "ui://report"); err != nil {
		t.Fatal(err)
	}
	if err := read.CheckResource("server", "ui://other"); !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatal("resource retarget allowed")
	}
	for _, ref := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 65)} {
		if _, err := appoperation.WithRead(context.Background(), ref, "agent", "server", "ui://report"); !errors.Is(err, appoperation.ErrInvalid) {
			t.Fatal("bad selector accepted")
		}
	}
}

func TestBinding_BoundsAndAcknowledgement(t *testing.T) {
	ref := strings.Repeat("a", 64)
	ctx, err := appoperation.WithRead(context.Background(), ref, "agent", "server", "ui://report")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := appoperation.From(ctx)
	b := a
	b.ResourceURI = "ui://other"
	if a.Digest() == b.Digest() {
		t.Fatal("acknowledgement does not bind the resource")
	}
	for _, agent := range []string{"", strings.Repeat("a", 257), "agent\n", string([]byte{0xff})} {
		if _, err := appoperation.WithRead(context.Background(), ref, agent, "server", "ui://report"); !errors.Is(err, appoperation.ErrInvalid) {
			t.Fatal("invalid agent admitted")
		}
	}
	if _, err := appoperation.WithCall(context.Background(), ref, "agent", "server", "ui://report", "", "save", nil); !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatal("missing generation admitted")
	}
	if _, err := appoperation.WithCall(context.Background(), ref, "agent", "server", "ui://report", "gen", "save", json.RawMessage("null")); !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatal("invalid arguments admitted")
	}
	values, err := appoperation.DecodeArguments(json.RawMessage(`{"nested":[true,false,null,{"x":1.00}]}`))
	if err != nil || len(values) != 1 {
		t.Fatal("valid nested values refused")
	}
	_, err = appoperation.DecodeArguments(json.RawMessage(`{"x":[1,}`))
	if !errors.Is(err, appoperation.ErrInvalid) {
		t.Fatal("malformed array admitted")
	}
	if _, err := appoperation.ArgumentsDigest(nil); err != nil {
		t.Fatal("absent arguments refused")
	}
}

func FuzzArgumentsDigest_BoundedCanonicalObject(f *testing.F) {
	for _, raw := range []string{`{}`, `{"n":9007199254740993}`, `{"a":[true,null,1.0]}`, `{"x":1,"x":2}`, `null`, `{"x":`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		values, err := appoperation.DecodeArguments(json.RawMessage(raw))
		if err != nil {
			return
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		before, err := appoperation.ArgumentsDigest(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		after, err := appoperation.ArgumentsDigest(encoded)
		if err != nil || before != after {
			t.Fatal("canonical object changed digest on encoding")
		}
	})
}
