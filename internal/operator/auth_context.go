package operator

import "context"

// actorScope carries the authenticated principal resolved from a minted
// operator-API token. It is absent for the admin-bearer (full-access) path and
// for unauthenticated dev mode, which both bypass per-actor scoping.
type actorScope struct {
	Actor string
	Scope []string
}

type actorScopeKey struct{}

// withActorScope returns a child context carrying the actor and capability
// scope from a verified token's claims.
func withActorScope(ctx context.Context, claims Claims) context.Context {
	return context.WithValue(ctx, actorScopeKey{}, actorScope{
		Actor: claims.Subject,
		Scope: claims.Scope,
	})
}

// actorScopeFromContext returns the actor/scope attached by withActorScope.
// ok is false when the request authenticated via the admin bearer or ran
// without authentication — both of which are treated as full access.
func actorScopeFromContext(ctx context.Context) (actorScope, bool) {
	scope, ok := ctx.Value(actorScopeKey{}).(actorScope)
	return scope, ok
}
