package auth

import (
	"context"
	"net/http"
)

type Identity struct {
	CredentialID uint
	AccessKey    string
	QuotaBytes   int64
	UsedBytes    int64
}

type Authenticator interface {
	Verify(r *http.Request) (*Identity, error)
}

type contextKey string

const identityContextKey contextKey = "auth.identity"

func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityContextKey, id)
}

func IdentityFromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityContextKey).(*Identity)
	return id, ok
}
