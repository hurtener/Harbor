package client

import (
	"context"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ArtifactsPrepareImport admits an exact destination-owned copy without bytes.
func (c *client) ArtifactsPrepareImport(ctx context.Context, r types.ArtifactsTransferRequest) (types.ArtifactTransferReceipt, error) {
	return c.transfer(ctx, methods.MethodArtifactsPrepareImport, r)
}

// ArtifactsTransfer starts a source-owned direct copy; no bytes reach this client.
func (c *client) ArtifactsTransfer(ctx context.Context, r types.ArtifactsTransferRequest) (types.ArtifactTransferReceipt, error) {
	return c.transfer(ctx, methods.MethodArtifactsTransfer, r)
}
func (c *client) transfer(ctx context.Context, m methods.Method, r types.ArtifactsTransferRequest) (types.ArtifactTransferReceipt, error) {
	id := c.scope()
	r.Scope = types.ArtifactScope{Tenant: id.Tenant, User: id.User, Session: id.Session}
	var out types.ArtifactTransferReceipt
	err := c.callMethod(ctx, m, r, &out)
	return out, err
}

// ArtifactsTransferStatus reads a durable owner-scoped receipt.
func (c *client) ArtifactsTransferStatus(ctx context.Context, r types.ArtifactsTransferStatusRequest) (types.ArtifactTransferReceipt, error) {
	return c.transferStatus(ctx, methods.MethodArtifactsTransferStatus, r)
}

// ArtifactsRevokeTransfer revokes an operation only before dispatch begins.
func (c *client) ArtifactsRevokeTransfer(ctx context.Context, r types.ArtifactsTransferStatusRequest) (types.ArtifactTransferReceipt, error) {
	return c.transferStatus(ctx, methods.MethodArtifactsRevokeTransfer, r)
}
func (c *client) transferStatus(ctx context.Context, m methods.Method, r types.ArtifactsTransferStatusRequest) (types.ArtifactTransferReceipt, error) {
	id := c.scope()
	r.Scope = types.ArtifactScope{Tenant: id.Tenant, User: id.User, Session: id.Session}
	var out types.ArtifactTransferReceipt
	err := c.callMethod(ctx, m, r, &out)
	return out, err
}

// ArtifactsExportAnswer materializes the selected sealed answer inside its runtime.
func (c *client) ArtifactsExportAnswer(ctx context.Context, r types.ArtifactsExportAnswerRequest) (types.ArtifactsExportAnswerResponse, error) {
	id := c.scope()
	r.Scope = types.ArtifactScope{Tenant: id.Tenant, User: id.User, Session: id.Session}
	var out types.ArtifactsExportAnswerResponse
	err := c.callMethod(ctx, methods.MethodArtifactsExportAnswer, r, &out)
	return out, err
}
