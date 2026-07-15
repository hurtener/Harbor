package integration

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/harbortest/devstack"
	protocolclient "github.com/hurtener/Harbor/internal/protocol/client"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/conversation"
)

func TestE2E_TUIControl_AuthenticatedCanonicalInspectionAndScopeFailures(t *testing.T) {
	stack := devstack.Assemble(t, runtimePostureConfig(t), devstack.AssembleOpts{})
	defer stack.Close()
	server := httptest.NewServer(stack.Handler)
	defer server.Close()
	id := types.IdentityScope{Tenant: devstack.DefaultDevTenant, User: devstack.DefaultDevUser, Session: devstack.DefaultDevSession}
	updates := make(chan conversation.Update, 64)
	controller, err := conversation.NewController(server.URL, conversation.NewTokenSource("", stack.Token), id, func(update conversation.Update) { updates <- update })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = controller.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err = controller.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	started, err := controller.Start(ctx, "inspect canonical runtime control", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	awaitTaskTerminalProjection(t, ctx, updates, started.TaskID)
	data := controller.Inspect(ctx, conversation.InspectionRequest{Identity: id, RequestEpoch: 1, TaskID: started.TaskID})
	if data.Posture.Info.ProtocolVersion == "" || data.Posture.Info.InstanceID == "" {
		t.Fatalf("posture=%#v errors=%v", data.Posture.Info, data.Errors)
	}
	if len(data.Tasks.Rows) == 0 || data.Tasks.Rows[0].Identity.Session != id.Session {
		t.Fatalf("tasks=%#v errors=%v", data.Tasks.Rows, data.Errors)
	}
	detail, err := controller.TaskDetail(ctx, started.TaskID)
	if err != nil || detail.Task.ID != started.TaskID {
		t.Fatalf("detail=%#v err=%v", detail, err)
	}
	uploaded, err := controller.Upload(ctx, "control.txt", "text/plain", []byte("bounded artifact"))
	if err != nil {
		t.Fatal(err)
	}
	data = controller.Inspect(ctx, conversation.InspectionRequest{Identity: id, RequestEpoch: 2, TaskID: started.TaskID, ArtifactID: uploaded.Ref.ID})
	found := false
	for _, row := range data.Artifacts.Rows {
		found = found || row.Ref.ID == uploaded.Ref.ID
	}
	if !found {
		t.Fatalf("uploaded artifact absent from canonical list: %#v errors=%v", data.Artifacts.Rows, data.Errors)
	}
	if _, err = controller.Control(ctx, methods.MethodCancel, "missing-run", "session_user", nil); err == nil {
		t.Fatal("missing run control unexpectedly accepted")
	} else {
		var wire *protocolclient.ProtocolError
		if !errors.As(err, &wire) || wire.Status != 404 || wire.Code != protoerrors.CodeNotFound {
			t.Fatalf("control error=%v", err)
		}
	}

	limitedToken := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "harbor-test", "sub": id.User, "aud": "harbor", "exp": time.Now().Add(time.Hour).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(), "iat": time.Now().Unix(), "tenant": id.Tenant, "user": id.User, "session": id.Session})
	limitedToken.Header["kid"] = stack.KID
	signed, err := limitedToken.SignedString(stack.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := conversation.NewController(server.URL, conversation.NewTokenSource("", signed), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limited.Close() }()
	if err = limited.Attach(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = limited.DeleteArtifact(ctx, uploaded.Ref.ID); err == nil {
		t.Fatal("scopeless destructive action unexpectedly accepted")
	} else {
		var wire *protocolclient.ProtocolError
		if !errors.As(err, &wire) || wire.Status != 403 {
			t.Fatalf("scope error=%v", err)
		}
	}
}
