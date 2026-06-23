// Package queryfields hosts the per-entity FieldMaps that map api.* DTOs onto
// the generic internal/query engine, plus the api.ListQuery <-> query.Args wire
// conversion. It is the single place allowed to depend on both api and
// internal/query, so service.Platform, the GraphQL resolvers, and the remote
// client can all reach the same field accessors without import cycles and
// without api depending on internal/query.
package queryfields

import (
	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/query"
)

// strOrNull maps an omitempty string field to a null Value when empty, so
// filtering and sorting treat "" as absent.
func strOrNull(s string) query.Value {
	if s == "" {
		return query.Value{}
	}
	return query.Str(s)
}

// Service is the filterable/sortable field set for api.ServiceState.
var Service = query.FieldMap[api.ServiceState]{
	"id":      func(s api.ServiceState) query.Value { return query.Str(s.Name) },
	"name":    func(s api.ServiceState) query.Value { return query.Str(s.Name) },
	"runtime": func(s api.ServiceState) query.Value { return query.Str(s.Runtime) },
	"status":  func(s api.ServiceState) query.Value { return query.Str(s.Status) },
	"health":  func(s api.ServiceState) query.Value { return strOrNull(s.Health) },
}

// Job is the field set for api.JobState.
var Job = query.FieldMap[api.JobState]{
	"id":      func(j api.JobState) query.Value { return query.Str(j.Name) },
	"name":    func(j api.JobState) query.Value { return query.Str(j.Name) },
	"runtime": func(j api.JobState) query.Value { return query.Str(j.Runtime) },
}

// Source is the field set for api.SourceState.
var Source = query.FieldMap[api.SourceState]{
	"id":     func(s api.SourceState) query.Value { return query.Str(s.Name) },
	"name":   func(s api.SourceState) query.Value { return query.Str(s.Name) },
	"kind":   func(s api.SourceState) query.Value { return query.Str(s.Kind) },
	"state":  func(s api.SourceState) query.Value { return strOrNull(s.State) },
	"branch": func(s api.SourceState) query.Value { return strOrNull(s.Branch) },
	"exists": func(s api.SourceState) query.Value { return query.Bool(s.Exists) },
	"dirty":  func(s api.SourceState) query.Value { return query.Bool(s.Dirty) },
	"pushed": func(s api.SourceState) query.Value { return query.Bool(s.Pushed) },
}

// SourceNumeric exposes numeric reducers for source aggregations.
var SourceNumeric = query.NumericFieldMap[api.SourceState]{
	"ahead":  func(s api.SourceState) (float64, bool) { return float64(s.Ahead), true },
	"behind": func(s api.SourceState) (float64, bool) { return float64(s.Behind), true },
}

// Workspace is the field set for api.WorkspaceRef.
var Workspace = query.FieldMap[api.WorkspaceRef]{
	"id":       func(w api.WorkspaceRef) query.Value { return query.Str(w.Name) },
	"name":     func(w api.WorkspaceRef) query.Value { return query.Str(w.Name) },
	"template": func(w api.WorkspaceRef) query.Value { return query.Str(w.Template) },
}

// Template is the field set for api.TemplateDescriptor (id aliases ref).
var Template = query.FieldMap[api.TemplateDescriptor]{
	"id":   func(t api.TemplateDescriptor) query.Value { return query.Str(t.Ref) },
	"ref":  func(t api.TemplateDescriptor) query.Value { return query.Str(t.Ref) },
	"kind": func(t api.TemplateDescriptor) query.Value { return query.Str(t.Kind) },
	"name": func(t api.TemplateDescriptor) query.Value { return strOrNull(t.Name) },
}

// Secret is the field set for api.SecretRef.
var Secret = query.FieldMap[api.SecretRef]{
	"id":        func(s api.SecretRef) query.Value { return query.Str(s.Name) },
	"name":      func(s api.SecretRef) query.Value { return query.Str(s.Name) },
	"envVar":    func(s api.SecretRef) query.Value { return strOrNull(s.EnvVar) },
	"declared":  func(s api.SecretRef) query.Value { return query.Bool(s.Declared) },
	"hasValue":  func(s api.SecretRef) query.Value { return query.Bool(s.HasValue) },
	"required":  func(s api.SecretRef) query.Value { return query.Bool(s.Required) },
	"generated": func(s api.SecretRef) query.Value { return query.Bool(s.Generated) },
}
