# Proposal: reshape the operator GraphQL to the nestjs-query shape

**Status:** Implemented (collections + service/secret CRUD) · **Area:** operator GraphQL, service API · **Surfaces:** GraphQL (operator) + refine.dev consoles

> ## Implementation notes (what shipped)
>
> The reshape landed as a **clean cut** (no backward compat). Concretely:
>
> - **`internal/query`** — the generic in-memory filter/sort/offset-paging engine
>   ([`query.go`](../../internal/query/query.go), [`value.go`](../../internal/query/value.go)),
>   with unit tests for every operator, and/or nesting, multi-key + nulls sort,
>   and paging.
> - **All six collections** (`services`, `jobs`, `sources`, `workspaces`,
>   `templates`, `secrets`) now take `filter` / `sorting` / `paging` and return an
>   offset `*Connection` (`nodes` + `totalCount` + `pageInfo`); every entity
>   exposes `id: ID!` (aliasing `name`, or `ref` for templates); single lookups
>   are `<entity>(id: ID!)`.
> - **CRUD** for service, secret, and workspace: `createOne`/`updateOne`/
>   `deleteOne` for each (`deleteOneWorkspace` carries an optional `purge`). The
>   old verbs (`serviceCreate`/`Update`/`Destroy`, `secretSet`/`secretDelete`,
>   `workspaceCreate`/`Update`/`Destroy`) were **removed**.
> - **Sources stay read-only + git verbs.** The operator has no source
>   create/delete primitive (only list/status/fetch/pull/push/diff), so source
>   CRUD is gated on the [global source registry](global-source-registry.md);
>   sources got the read connection + `id` only.
> - **Custom ops kept** (not backward-compat, no CRUD analog): stack lifecycle,
>   `serviceInit`/`serviceUp`/`…`, source git verbs, `workspaceCreatePreflight`/
>   `workspacePush`/`workspaceSyncBase` and the `workspaceSource*` git ops,
>   token minting, logs, diffs, subscriptions.
> - **Schema export for frontends:** the SDL is emitted to
>   `docs/public/angee.graphql` (`make graphql-schema`; also run by
>   `make generate` and verified by `make check-generated`) via `cmd/gqlschema`,
>   formatted from the same executable schema the operator serves.
>
> **One deliberate divergence from "Where the work lives" below:** the engine is
> applied at the **resolver layer** over the slice returned by `service.API`, not
> pushed into the `service.API` signatures. `service.API` is shared by the
> REST-backed `RemoteClient`, so pushing the query args down is inseparable from
> "REST filter parity" — which stays the documented follow-up. `service.API`,
> REST, CLI, and the `StackSnapshot` aggregate (kept as bare arrays) are
> unchanged. The remaining open items: source CRUD (needs the source registry
> backend), relations, aggregations, and REST filter parity.

## Summary

Reshape the operator's **collection** GraphQL surface to match
[nestjs-query](https://doug-martin.github.io/nestjs-query/) **exactly**, so the
off-the-shelf [`@refinedev/nestjs-query`](https://www.npmjs.com/package/@refinedev/nestjs-query)
data provider drives an Angee console with **zero custom mappers**. Concretely:

1. List queries gain `filter` / `sorting` / `paging` arguments and return an
   **offset connection** (`{ nodes, totalCount, pageInfo }`) instead of a bare
   `[T!]!`.
2. Every entity exposes a stable `id: ID!` (aliasing its natural key, `name`),
   and single lookups become `entity(id: ID!)`.
3. Genuinely-CRUD entities get nestjs-query mutations:
   `createOne` / `updateOne` / `deleteOne` (+ `*Many` where useful) with the
   `input: { entity }` / `input: { id, update }` / `input: { id }` wrappers.

What this proposal **does not** do: contort the operator's lifecycle verbs
(`stackUp`, `serviceRestart`, `workspaceSourceMerge`, git ops, token minting)
into fake CRUD. nestjs-query is a CRUD-over-collections convention; those verbs
are RPC and stay RPC, consumed by refine via `useCustomMutation` /
`meta.gqlMutation`. The reshape is a **hybrid**: a nestjs-query-shaped read +
CRUD surface, plus the existing action mutations left as custom operations.

## Principle: shape the collections, not the verbs

> nestjs-query models *records in a collection* — filter them, page them, sort
> them, create/update/delete them by id. Angee's lists (`services`, `secrets`,
> `sources`, `workspaces`, `templates`) **are** collections and should speak that
> shape exactly. Angee's lifecycle operations are **not** records and must not be
> dressed as `updateOne`. Match the convention where it fits; keep custom
> operations honest where it doesn't.

Matching nestjs-query *exactly* (not "flavored") is the load-bearing choice: it
is the difference between `dataProvider(graphqlClient)` working untouched and
maintaining a per-resource `dataMapper` / `buildFilters` shim on the client
forever.

## Problem

The operator already exposes a complete GraphQL surface
([`schema.graphql`](../../internal/operator/schema.graphql)), but its collection
shape is incompatible with the dominant React admin convention (refine over
nestjs-query):

- **No filtering, sorting, or paging.** Every list returns the full slice
  unordered: `services: [ServiceState!]!`, `secrets: [SecretRef!]!`, etc.
  ([`schema.graphql:284`](../../internal/operator/schema.graphql),
  [`:301`](../../internal/operator/schema.graphql)). The service layer takes no
  query parameters at all — `ServiceList(ctx)`, `SecretsList(ctx)`
  ([`api.go:79`](../../internal/service/api.go),
  [`:140`](../../internal/service/api.go)).
- **No connection envelope.** refine reads `data.<resource>.nodes` and
  `.totalCount`; Angee returns a top-level array, so refine's list views can't
  page or report counts.
- **By-name lookups, no `id`.** Single fetches are `source(name:)`,
  `secret(name:)`, `workspace(name:)`
  ([`schema.graphql:287`](../../internal/operator/schema.graphql)). refine keys
  every record on `id`; no entity exposes one.
- **Verb mutations, not CRUD.** `secretSet` / `secretDelete`
  ([`schema.graphql:344`](../../internal/operator/schema.graphql)),
  `serviceCreate` / `serviceUpdate` / `serviceDestroy`
  ([`:317`](../../internal/operator/schema.graphql)) carry CRUD *meaning* under
  non-CRUD *names* and no input wrappers. refine's create/update/delete hooks
  expect `createOneSecret(input:{secret:…})` etc.

The result: a console built on the standard refine + nestjs-query stack needs a
hand-written data provider for Angee. Reshaping the schema removes that custom
provider entirely.

## Current behavior (the machinery to reuse)

The reshape is concentrated in the SDL and one new service-layer helper; the
dispatch wiring is already right.

- **gqlgen, SDL-first.** The schema is hand-authored SDL
  ([`schema.graphql`](../../internal/operator/schema.graphql)); gqlgen generates
  executors and binds GraphQL types **directly** to the REST DTOs via
  [`gqlgen.yml`](../../internal/operator/gqlgen.yml) (`ServiceState` →
  `api.ServiceState`, etc.). New `filter`/`sort`/`paging` input types and
  `*Connection` types are pure-additive SDL that gqlgen will scaffold.
- **Resolvers are thin pass-throughs.** `r.Platform.<method>` and done —
  `Secrets` → `SecretsList` ([`schema.resolvers.go:499`](../../internal/operator/gql/schema.resolvers.go)),
  `Services` → `ServiceList` ([`:358`](../../internal/operator/gql/schema.resolvers.go)),
  `SecretSet` ([`:320`](../../internal/operator/gql/schema.resolvers.go)),
  `SecretDelete` ([`:329`](../../internal/operator/gql/schema.resolvers.go)). The
  reshape changes signatures, not the dispatch model.
- **Small, in-memory collections.** Lists are derived from manifest/runtime
  state, not a database — so filter/sort/page can be a generic in-process pass
  over a `[]T`, not query-language translation. No new persistence.
- **REST/GraphQL parity is the open design question.** Today both surfaces call
  identical `service.API` methods with no adapters. Pushing query semantics down
  to `service.API` keeps that parity (REST gets filtering too); doing it only in
  resolvers forks the two. This proposal recommends pushing it down — see
  "Where the work lives".

## The target contract (nestjs-query, exact)

The shapes `@refinedev/nestjs-query` assumes — these are the spec we match.

**Shared types** (defined once, reused by every resource):

```graphql
input OffsetPaging { limit: Int, offset: Int }
type OffsetPageInfo { hasNextPage: Boolean, hasPreviousPage: Boolean }

enum SortDirection { ASC DESC }
enum SortNulls { NULLS_FIRST NULLS_LAST }

input StringFieldComparison {
  is: Boolean  isNot: Boolean
  eq: String   neq: String
  gt: String   gte: String   lt: String   lte: String
  like: String notLike: String iLike: String notILike: String
  in: [String!] notIn: [String!]
}
input BooleanFieldComparison { is: Boolean  isNot: Boolean }
# IntFieldComparison / DateFieldComparison follow the same pattern.

type UpdateManyResponse { updatedCount: Int! }
type DeleteManyResponse { deletedCount: Int! }
```

**Per resource**, nestjs-query derives: a `<T>Filter` (per-field comparison +
recursive `and`/`or`), a `<T>Sort` + `<T>SortFields` enum, a `<T>Connection`
(`nodes` + `totalCount` + `pageInfo`), and six mutations with wrapped inputs.
refine's response-key convention is `data.<plural>` for lists and
`data.{createOne,updateOne,deleteOne}<Pascal>` for mutations — so the SDL names
below are not cosmetic, they are the contract.

## Worked spike: `secrets` end-to-end

Secrets are the cleanest exact-match — a named collection with a clear write DTO
(`value` is write-only; it never appears on the read type). Here is the full
reshape, SDL + Go sketch.

### SDL

```graphql
# --- read type: add `id` aliasing the natural key ---
type SecretRef {
  id: ID!                # NEW — aliases `name`; refine's record key
  name: String!
  declared: Boolean!
  hasValue: Boolean!
  required: Boolean
  generated: Boolean
  import: String
  envVar: String
}

# --- list: filter + sorting + paging -> offset connection ---
type Query {
  secrets(
    filter: SecretRefFilter = {}
    sorting: [SecretRefSort!] = []
    paging: OffsetPaging = {}
  ): SecretRefConnection!
  secret(id: ID!): SecretRef          # getOne by id (was: secret(name:))
}

type SecretRefConnection {
  nodes: [SecretRef!]!
  totalCount: Int!
  pageInfo: OffsetPageInfo!
}

input SecretRefFilter {
  and: [SecretRefFilter!]
  or: [SecretRefFilter!]
  id: StringFieldComparison
  name: StringFieldComparison
  declared: BooleanFieldComparison
  hasValue: BooleanFieldComparison
  required: BooleanFieldComparison
  generated: BooleanFieldComparison
  envVar: StringFieldComparison
}

input SecretRefSort { field: SecretRefSortFields!, direction: SortDirection!, nulls: SortNulls }
enum SecretRefSortFields { id name declared hasValue required generated envVar }

# --- mutations: nestjs-query CRUD wrappers ---
type Mutation {
  createOneSecret(input: CreateOneSecretInput!): SecretRef!
  createManySecrets(input: CreateManySecretsInput!): [SecretRef!]!
  updateOneSecret(input: UpdateOneSecretInput!): SecretRef!
  updateManySecrets(input: UpdateManySecretsInput!): UpdateManyResponse!
  deleteOneSecret(input: DeleteOneSecretInput!): SecretRef!
  deleteManySecrets(input: DeleteManySecretsInput!): DeleteManyResponse!
}

# write DTO is distinct from the read type: `value` in, never out
input SecretCreateInput { name: String!, value: String! }
input SecretUpdateInput { value: String }      # name is the id; only value mutates

input CreateOneSecretInput  { secret: SecretCreateInput! }
input CreateManySecretsInput { secrets: [SecretCreateInput!]! }
input UpdateOneSecretInput  { id: ID!, update: SecretUpdateInput! }
input UpdateManySecretsInput { filter: SecretRefFilter!, update: SecretUpdateInput! }
input DeleteOneSecretInput  { id: ID! }
input DeleteManySecretsInput { filter: SecretRefFilter! }
```

The example query refine would generate, and Angee would now satisfy:

```graphql
query {
  secrets(
    filter: { hasValue: { is: false }, name: { iLike: "%API%" } }
    sorting: [{ field: name, direction: ASC }]
    paging: { limit: 20, offset: 0 }
  ) {
    nodes { id name declared hasValue }
    totalCount
    pageInfo { hasNextPage hasPreviousPage }
  }
}
```

### Go sketch

A single generic helper applies the query over any in-memory slice; resolvers
stay thin. The engine (`query.Apply` + `FieldMap`) is detailed in
[The `internal/query` primitive](#the-internalquery-primitive) below — here the
resolver just supplies a `FieldMap` and binds the GraphQL args. `id` aliasing is
one field resolver.

```go
// internal/operator/gql/schema.resolvers.go — list resolver, reshaped.
func (r *queryResolver) Secrets(ctx context.Context, args model.SecretsArgs) (*model.SecretRefConnection, error) {
    all, err := r.Platform.SecretsList(ctx)        // unchanged service call
    if err != nil {
        return nil, err
    }
    page, total := query.Apply(all, args.ToQuery(), secretFields)  // secretFields: name/declared/hasValue/...
    return secretConnection(page, total, args.Paging), nil
}

// id aliases name — refine's stable record key.
func (r *secretRefResolver) ID(ctx context.Context, obj *api.SecretRef) (string, error) {
    return obj.Name, nil
}
```

```go
// Mutations map onto the EXISTING service methods — no new business logic.
func (r *mutationResolver) CreateOneSecret(ctx context.Context, input model.CreateOneSecretInput) (*api.SecretRef, error) {
    ref, err := r.Platform.SecretSet(ctx, input.Secret.Name, input.Secret.Value) // service api.go:143
    return &ref, err
}
func (r *mutationResolver) UpdateOneSecret(ctx context.Context, input model.UpdateOneSecretInput) (*api.SecretRef, error) {
    ref, err := r.Platform.SecretSet(ctx, input.ID, *input.Update.Value)          // id == name
    return &ref, err
}
func (r *mutationResolver) DeleteOneSecret(ctx context.Context, input model.DeleteOneSecretInput) (*api.SecretRef, error) {
    prev, _ := r.Platform.SecretGet(ctx, input.ID)                                 // service api.go:141
    err := r.Platform.SecretDelete(ctx, input.ID)                                  // service api.go:144
    return &prev, err
}
```

The next spike applies the same template to `services` — a richer entity where
read-only runtime fields and the surviving lifecycle verbs make the hybrid
concrete. The remaining collections follow identically: `sources` and
`workspaces` gain a connection + `id` + CRUD where it fits, and `templates`
stays read-only (connection + `id`, no CRUD mutations).

## Worked spike: `services`

Services show the parts secrets don't: a read type whose fields are **computed
runtime state** (`status`, `health` — filterable/sortable but absent from any
write DTO), a richer create/update DTO, **no existing single-service getter**,
and lifecycle verbs (`serviceUp`/`Start`/`Stop`/`Restart`) that stay custom
alongside the new CRUD. This is the canonical hybrid.

### SDL

```graphql
type ServiceState {
  id: ID!                # NEW — aliases `name`
  name: String!
  runtime: String!
  status: String!        # computed runtime state — filterable, never written
  health: String         # nullable — { is: false } matches "has a health value"
}

type Query {
  services(
    filter: ServiceStateFilter = {}
    sorting: [ServiceStateSort!] = []
    paging: OffsetPaging = {}
  ): ServiceStateConnection!
  service(id: ID!): ServiceState        # NEW getOne — no single-service query exists today
}

type ServiceStateConnection {
  nodes: [ServiceState!]!
  totalCount: Int!
  pageInfo: OffsetPageInfo!
}

input ServiceStateFilter {
  and: [ServiceStateFilter!]
  or: [ServiceStateFilter!]
  id: StringFieldComparison
  name: StringFieldComparison
  runtime: StringFieldComparison
  status: StringFieldComparison
  health: StringFieldComparison
}

input ServiceStateSort { field: ServiceStateSortFields!, direction: SortDirection!, nulls: SortNulls }
enum ServiceStateSortFields { id name runtime status health }

type Mutation {
  createOneService(input: CreateOneServiceInput!): ServiceState!
  updateOneService(input: UpdateOneServiceInput!): ServiceState!
  deleteOneService(input: DeleteOneServiceInput!): ServiceState!
  # serviceUp / serviceStart / serviceStop / serviceRestart stay as-is — custom ops, not CRUD.
}

# write DTOs are distinct from the read type — no status/health here.
input CreateOneServiceInput { service: ServiceCreateInput! }   # ServiceCreateInput already exists (schema.graphql:266)
input UpdateOneServiceInput { id: ID!, update: ServiceUpdateInput! }
input ServiceUpdateInput {                                     # existing ServiceInput minus name (name == id)
  runtime: String
  image: String
  command: [String!]
  mounts: [String!]
  env: [KeyValueInput!]
  ports: [String!]
  workdir: String
  start: Boolean
}
input DeleteOneServiceInput { id: ID! }
```

### Go sketch

```go
// list — identical to secrets, different FieldMap.
func (r *queryResolver) Services(ctx context.Context, args model.ServicesArgs) (*model.ServiceStateConnection, error) {
    all, err := r.Platform.ServiceList(ctx)                 // service api.go:79 — unchanged
    if err != nil {
        return nil, err
    }
    page, total := query.Apply(all, args.ToQuery(), serviceFields)
    return serviceConnection(page, total, args.Paging), nil
}

// serviceFields — the FieldMap: status/health are runtime state, filterable yet write-only-absent.
var serviceFields = query.FieldMap[api.ServiceState]{
    "id":      func(s api.ServiceState) query.Value { return query.Str(s.Name) },
    "name":    func(s api.ServiceState) query.Value { return query.Str(s.Name) },
    "runtime": func(s api.ServiceState) query.Value { return query.Str(s.Runtime) },
    "status":  func(s api.ServiceState) query.Value { return query.Str(s.Status) },
    "health":  func(s api.ServiceState) query.Value { return query.StrPtr(s.Health) }, // nil => null
}

func (r *serviceStateResolver) ID(ctx context.Context, obj *api.ServiceState) (string, error) {
    return obj.Name, nil
}

// mutations map onto existing service methods. Note ServiceUpdate/ServiceDestroy
// return only error, and there is NO single-service getter today — so getOne and
// the update/delete return values share one small helper, serviceByID.
func (r *mutationResolver) CreateOneService(ctx context.Context, input model.CreateOneServiceInput) (*api.ServiceState, error) {
    st, err := r.Platform.ServiceCreate(ctx, serviceCreateRequestFrom(input.Service)) // api.go:83 — returns the entity
    return &st, err
}
func (r *mutationResolver) UpdateOneService(ctx context.Context, input model.UpdateOneServiceInput) (*api.ServiceState, error) {
    if err := r.Platform.ServiceUpdate(ctx, serviceInitRequestFrom(input.ID, input.Update)); err != nil { // api.go:81 — returns error
        return nil, err
    }
    return r.serviceByID(ctx, input.ID)                     // re-read to return the updated record
}
func (r *mutationResolver) DeleteOneService(ctx context.Context, input model.DeleteOneServiceInput) (*api.ServiceState, error) {
    prev, err := r.serviceByID(ctx, input.ID)               // capture the pre-delete record to return
    if err != nil {
        return nil, err
    }
    return prev, r.Platform.ServiceDestroy(ctx, input.ID, true) // api.go:82
}
```

The one genuinely new service-layer affordance services need is a
**single-service getter** for `service(id:)` and the update/delete return paths —
either a small `ServiceGet(ctx, name)` on `service.API` (preferred, mirrors
`SecretGet` at [`api.go:141`](../../internal/service/api.go)) or the `serviceByID`
helper deriving it from `ServiceList`. Secrets already have `SecretGet`, so the
secrets spike needed no new method; services do. Everything else is binding.

## The `internal/query` primitive

The reusable engine the spikes call. One package, generic over the record type,
fed a `FieldMap` per resource. It is the normalized internal form of
nestjs-query's typed filter/sort/paging — the gqlgen layer binds the typed
`StringFieldComparison` / `BooleanFieldComparison` / `IntFieldComparison` inputs
down into this single shape.

```go
// internal/query — generic in-memory filter / sort / page for operator collections.
package query

// Args is the bound form of the GraphQL (filter, sorting, paging) arguments.
type Args struct {
    Filter  Filter
    Sorting []Sort
    Paging  Paging
}

// Filter is a recursive AND/OR tree of per-field comparisons — the internal
// normalization of nestjs-query's <T>Filter { and, or, <field>: Comparison }.
type Filter struct {
    And, Or []Filter
    Fields  map[string]Comparison
}

// Comparison is one field's operators; a nil pointer / empty slice means unset.
// All the typed *FieldComparison inputs collapse into this one struct.
type Comparison struct {
    Is, IsNot        *bool   // null checks: { is: false } => field is non-null
    Eq, Neq          *Value
    Gt, Gte, Lt, Lte *Value
    Like, NotLike    *string
    ILike, NotILike  *string
    In, NotIn        []Value
}

// Value is a normalized cell — at most one field set; all nil means the record's
// field is null. Keeps the comparators type-aware without reflection.
type Value struct {
    Str  *string
    Num  *float64
    Bool *bool
}

type Sort struct {
    Field     string
    Desc      bool // direction == DESC
    NullsLast bool // nulls == NULLS_LAST
}

type Paging struct{ Limit, Offset int } // Limit <= 0 => unbounded

// FieldMap exposes, per filterable/sortable field name, an accessor returning
// that field's normalized Value. Constructors Str/StrPtr/Num/Bool build Values.
type FieldMap[T any] map[string]func(T) Value

// Apply filters, then sorts, then pages — returning the page and the pre-page
// total (what refine reads as totalCount).
func Apply[T any](items []T, a Args, fm FieldMap[T]) (page []T, total int) {
    var matched []T
    for _, it := range items {
        if match(a.Filter, it, fm) {
            matched = append(matched, it)
        }
    }
    sortBy(matched, a.Sorting, fm) // stable, multi-key, nulls-aware
    total = len(matched)
    return pageOf(matched, a.Paging), total
}

// match: an empty Filter (no And/Or/Fields) matches everything — refine's
// "no filter" default. Otherwise every populated Fields op AND every And[]
// subfilter must hold; any Or[] subfilter satisfies.
func match[T any](f Filter, item T, fm FieldMap[T]) bool { /* and / or / per-field ops */ }
```

Binding is the only per-resource glue. Sorting and paging bind generically
(`bindSorts`, `bindPaging`); the recursive typed filter needs one small mapper
per resource (gqlgen can't auto-collapse a typed `<T>Filter` into the generic
`Comparison`):

```go
// the per-resource shim from generated gqlgen inputs to query.Args
func (a ServicesArgs) ToQuery() query.Args {
    return query.Args{
        Filter:  bindServiceFilter(a.Filter), // typed FieldComparisons -> query.Comparison (the only boilerplate)
        Sorting: bindSorts(a.Sorting),        // shared
        Paging:  bindPaging(a.Paging),        // shared; default limit, offset 0
    }
}
```

So per reshaped resource the new code is: a `FieldMap`, an `ID` field resolver, a
`bind<T>Filter`, and the CRUD mutation mappers — everything else is the shared
`internal/query` engine and gqlgen scaffolding.

## The action-mutation boundary

The operator's verbs have **no** CRUD analog and must stay custom:

- **Lifecycle:** `stackUp` / `stackDown` / `serviceRestart` / `serviceStop` …
- **Git ops:** `workspaceSourceMerge` / `Rebase` / `RebaseContinue` / `Publish` …
- **Tokens:** `mintConnectionToken` / `mintRouteToken`.
- **Snapshots/diffs/logs:** `gitOpsTopology`, `sourceDiff`, `serviceLogs`, the
  `Subscription` fields.

refine consumes these via `useCustomMutation` / `useCustom` with an explicit
`meta.gqlMutation` — the supported escape hatch. They are **left exactly as they
are**; the reshape touches only the collection read + CRUD surface. Forcing
`serviceRestart` into `updateOneService(input:{id,update:{restart:true}})` would
corrupt the model and is explicitly rejected.

## Design options

### A. Exact nestjs-query match on collections; verbs stay custom (recommended)

Reshape `secrets`/`services`/`sources`/`workspaces`/`templates` to the precise
nestjs-query SDL (offset connection, `id`, field-comparison filters, `createOne`/
`updateOne`/`deleteOne`); leave every lifecycle/git/token/log operation as a
custom op. `@refinedev/nestjs-query` drives the list/show/create/edit screens
untouched; custom operations are wired per-screen with `meta.gqlMutation`. One
generic `internal/query` helper, pushed into `service.API` so REST benefits too.
**This is the proposal.**

### B. "nestjs-query-flavored" (rejected)

Adopt filter/sort/paging conventions but allow deviations (e.g. keep `name`
lookups, skip `totalCount`, partial connections). Less SDL discipline now, but it
forfeits the entire prize: the client needs a custom data provider with per-
resource `dataMapper`/`buildFilters` overrides maintained forever. The user
chose exact match precisely to avoid this.

### C. Resolver-only reshape, REST untouched (rejected as the boundary)

Implement filter/sort/paging in the GraphQL resolvers over the full slice and
leave `service.API` returning bare lists. Works, but forks REST and GraphQL
(today they are at parity on identical methods) and buries reusable query logic
in the GraphQL layer against the project's "dispatch through `service.Platform`"
rule (CLAUDE.md). Prefer pushing the query primitives down (option A).

### D. Relay cursor connections instead of offset (rejected)

nestjs-query's *default* is cursor connections (`edges`/`node`/`cursor`/
`PageInfo`). `@refinedev/nestjs-query` uses **offset** paging (`nodes` +
`totalCount`). Since the target is that provider, match offset. Cursor paging
adds opaque-cursor machinery the in-memory collections don't need.

## Where the work lives

The schema is the easy part; the real cost is the query primitive and the
parity decision.

1. **`internal/query` (new).** Generic filter-AST evaluation (field comparisons
   + `and`/`or`), multi-key sort, offset+limit with pre-page total, over a
   `[]T` + per-type `FieldMap` — types and `Apply` sketched in
   [The `internal/query` primitive](#the-internalquery-primitive). ~one small,
   well-tested package.
2. **`service.API` signatures.** Add an optional query argument to the list
   methods so both REST and GraphQL filter/sort/page through one path
   ([`api.go:79`](../../internal/service/api.go),
   [`:140`](../../internal/service/api.go)). Keep a no-arg/empty default so
   existing callers are unchanged.
3. **SDL + gqlgen.** Add shared types once; add per-resource filter/sort/
   connection/input types; regenerate. Bind new args/connection models in
   [`gqlgen.yml`](../../internal/operator/gqlgen.yml).
4. **Resolvers.** Swap list resolvers to return connections; add `id` field
   resolvers; add `createOne`/`updateOne`/`deleteOne` mapping onto existing
   service methods ([`schema.resolvers.go:320`](../../internal/operator/gql/schema.resolvers.go),
   [`:499`](../../internal/operator/gql/schema.resolvers.go)).
5. **`StackSnapshot`.** Its embedded lists
   ([`schema.graphql:362`](../../internal/operator/schema.graphql)) either keep
   the bare-array shape (snapshot is a console aggregate, not a refine resource)
   or move to connections — a deliberate call, not automatic.

## Security

- No new auth surface: all reshaped fields ride the existing `s.auth(...)`
  admin-bearer gate, exactly as the verbs they replace.
- `SecretValue` stays a separate, explicit query
  ([`schema.graphql:303`](../../internal/operator/schema.graphql)); the reshaped
  `SecretRef`/`SecretCreateInput` keep `value` write-only (in via
  `SecretCreateInput`, never out on the read type) — the reshape must not leak
  secret values into list/getOne responses.
- Filter inputs are evaluated in-process over already-authorized data; there is
  no query-language passthrough to a backend, so no injection surface beyond the
  comparison operators themselves.

## Migration (clean cut — no backward compatibility)

GraphQL is pre-1.0 here and there is no external API contract to preserve, so
the reshape is a **clean cut** — no deprecation window, no parallel field names,
no compatibility shims.

- **List/getOne change shape outright.** `services: [ServiceState!]!` →
  `services(...): ServiceStateConnection!` and `secret(name:)` → `secret(id:)`.
  The in-repo GraphQL clients (the web console, `StackSnapshot` subscribers) are
  updated in the same change.
- **CRUD verbs are replaced, not kept alongside.** `secretSet`/`secretDelete`
  ([`schema.graphql:344`](../../internal/operator/schema.graphql)) and
  `serviceCreate`/`serviceUpdate`/`serviceDestroy`
  ([`:317`](../../internal/operator/schema.graphql)) are removed in favour of
  their `createOne`/`updateOne`/`deleteOne` equivalents. Only the **non-CRUD**
  verbs (lifecycle, git ops, tokens, logs) survive untouched.
- **CLI/REST.** Query primitives are pushed to `service.API` with empty-default
  args (option A), so the CLI and REST keep working and gain filtering opt-in;
  exposing the same filter/sort/paging on the REST list endpoints is a clean
  follow-up.

## Out of scope

- **Relations / nested filtered connections** (nestjs-query's `@Relation`).
  Angee's graph (workspace→sources, stack→services) is already exposed via
  `GitOpsTopology`/`WorkspaceStatus`; reshaping those into nestjs-query relation
  connections is a follow-up, not part of the first cut.
- **Aggregations** (`<T>Aggregate` count/min/max) — defer until a console needs
  them.
- **The action mutations themselves** — untouched by design (see boundary).
- **A generated TypeScript SDK / refine resource wiring** — that lives in the
  console repo, downstream of this contract.

## Acceptance

- `secrets(filter, sorting, paging)` returns `SecretRefConnection` with correct
  `nodes`, `totalCount` (pre-page), and `pageInfo`; an empty
  `filter/sorting/paging` returns all secrets, name-sorted by default.
- `secret(id:)`, `createOneSecret`, `updateOneSecret`, `deleteOneSecret`
  (+ the `*Many` forms) behave per the SDL above and map onto the existing
  `SecretSet`/`SecretGet`/`SecretDelete` service methods with no new business
  logic; secret values never appear on `SecretRef`.
- `@refinedev/nestjs-query` configured against the operator endpoint drives a
  list + show + create + edit + delete screen for secrets with **no** custom
  `dataMapper`/`buildFilters`/`buildSorters` overrides.
- The same reshape applied to `services` passes the equivalent checks; the
  lifecycle/git/token mutations are unchanged and still callable.
- `internal/query` has unit tests for each comparison operator, `and`/`or`
  nesting, multi-key sort, and offset/limit + total.
- `docs/reference/operator-api.md` documents the connection shape, the filter/
  sort/paging arguments, and the CRUD-vs-custom-operation split.

## See also

- [`internal/operator/schema.graphql`](../../internal/operator/schema.graphql) —
  `SecretRef` (`:412`), `ServiceState` (`:8`), list queries (`:284`, `:301`),
  secret mutations (`:344`), `StackSnapshot` (`:362`).
- [`internal/operator/gql/schema.resolvers.go`](../../internal/operator/gql/schema.resolvers.go) —
  `Secrets` (`:499`), `Services` (`:358`), `SecretSet` (`:320`),
  `SecretDelete` (`:329`): the thin dispatch the reshape preserves.
- [`internal/service/api.go`](../../internal/service/api.go) — `ServiceList`
  (`:79`), `SecretsList` (`:140`), `SecretGet` (`:141`), `SecretSet` (`:143`),
  `SecretDelete` (`:144`): where query args and the parity decision land.
- [`internal/operator/gqlgen.yml`](../../internal/operator/gqlgen.yml) — DTO
  binding the new connection/input models extend.
- [`api/types.go`](../../api/types.go) — `ServiceState` (`:55`), `SecretRef`
  (`:384`): the read DTOs that gain `id`.
- nestjs-query docs:
  [queries](https://doug-martin.github.io/nestjs-query/docs/graphql/queries/),
  [mutations](https://doug-martin.github.io/nestjs-query/docs/graphql/mutations/),
  [paging](https://doug-martin.github.io/nestjs-query/docs/graphql/paging/),
  [types](https://doug-martin.github.io/nestjs-query/docs/graphql/types/).
- refine data providers:
  [@refinedev/nestjs-query](https://www.npmjs.com/package/@refinedev/nestjs-query),
  [GraphQL data provider](https://refine.dev/docs/data/packages/graphql/).
- Sibling proposals (same "operator is a generic mechanism" framing):
  [`global-source-registry.md`](global-source-registry.md),
  [`ephemeral-workspace-pool.md`](ephemeral-workspace-pool.md).
