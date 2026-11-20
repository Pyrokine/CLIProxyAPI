package logging

import (
	"context"
	"net/http"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

type clientRequestMetadataKey struct{}

// ClientRequestMetadata stores immutable downstream request metadata for asynchronous consumers.
type ClientRequestMetadata struct {
	ClientIP         string
	ResolvedClientIP string
	XForwardedFor    string
	UserAgent        string
	SessionID        string
	ParentSessionID  string
	NodeKind         string
	IsFork           bool
	IsCompaction     bool
}

// ExtractClientRequestMetadata captures the bounded request metadata allowed in usage records.
func ExtractClientRequestMetadata(request *http.Request) ClientRequestMetadata {
	if request == nil {
		return ClientRequestMetadata{}
	}
	client, ok := access.ClientAddressFromContext(request.Context())
	if !ok {
		client = access.ResolveClientAddress(request, nil)
	}
	return ClientRequestMetadata{
		ClientIP:         client.IP,
		ResolvedClientIP: client.IP,
		XForwardedFor:    strings.TrimSpace(strings.Join(request.Header.Values("X-Forwarded-For"), ", ")),
		UserAgent:        strings.TrimSpace(request.UserAgent()),
	}
}

// WithClientRequestMetadata stores a snapshot of downstream request metadata in ctx.
func WithClientRequestMetadata(ctx context.Context, metadata ClientRequestMetadata) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if metadata.SessionID != "" || GetClientRequestMetadata(ctx).SessionID != "" {
		ctx = util.WithInferredSessionID(ctx, metadata.SessionID)
	}
	return context.WithValue(ctx, clientRequestMetadataKey{}, metadata)
}

// GetClientRequestMetadata returns downstream request metadata stored in ctx.
func GetClientRequestMetadata(ctx context.Context) ClientRequestMetadata {
	if ctx == nil {
		return ClientRequestMetadata{}
	}
	metadata, _ := ctx.Value(clientRequestMetadataKey{}).(ClientRequestMetadata)
	return metadata
}
