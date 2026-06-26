package api

// This file holds the REST wire contract for filtered list requests/responses
// (a generic, transport-neutral filter/sort/paging JSON, independent of the
// GraphQL Hasura dialect). It is stdlib-only by design: the api package must not
// depend on internal/query. The api.ListQuery <-> query.Args conversion lives in
// internal/queryfields, the one place allowed to import both.

// ListQuery is the filter/sort/paging spec a REST list endpoint accepts as a
// url-encoded JSON `query` parameter. The empty value means match-all.
type ListQuery struct {
	Filter  *ListFilter `json:"filter,omitempty"`
	Sorting []ListSort  `json:"sorting,omitempty"`
	Paging  *ListPaging `json:"paging,omitempty"`
}

// ListFilter is a recursive AND/OR tree of per-field comparisons, mirroring the
// GraphQL <T>Filter shape.
type ListFilter struct {
	And    []ListFilter               `json:"and,omitempty"`
	Or     []ListFilter               `json:"or,omitempty"`
	Fields map[string]FieldComparison `json:"fields,omitempty"`
}

// FieldComparison is one field's operators. Value-bearing operators are strings
// on the wire (the engine normalizes by field type); is/isNot are booleans.
type FieldComparison struct {
	Is       *bool    `json:"is,omitempty"`
	IsNot    *bool    `json:"isNot,omitempty"`
	Eq       *string  `json:"eq,omitempty"`
	Neq      *string  `json:"neq,omitempty"`
	Gt       *string  `json:"gt,omitempty"`
	Gte      *string  `json:"gte,omitempty"`
	Lt       *string  `json:"lt,omitempty"`
	Lte      *string  `json:"lte,omitempty"`
	Like     *string  `json:"like,omitempty"`
	NotLike  *string  `json:"notLike,omitempty"`
	ILike    *string  `json:"iLike,omitempty"`
	NotILike *string  `json:"notILike,omitempty"`
	In       []string `json:"in,omitempty"`
	NotIn    []string `json:"notIn,omitempty"`
}

// ListSort is one ordering key. Direction is "ASC" (default) or "DESC"; Nulls is
// "NULLS_FIRST" (default) or "NULLS_LAST".
type ListSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction,omitempty"`
	Nulls     string `json:"nulls,omitempty"`
}

// ListPaging is offset paging. A zero Limit means unbounded.
type ListPaging struct {
	Limit  int `json:"limit,omitempty"`
	Offset int `json:"offset,omitempty"`
}

// Per-entity list responses carry the page plus the pre-page total, mirroring
// the GraphQL offset connection so REST and GraphQL convey the same shape.

type ServiceListResponse struct {
	Nodes      []ServiceState `json:"nodes"`
	TotalCount int            `json:"total_count"`
}

type JobListResponse struct {
	Nodes      []JobState `json:"nodes"`
	TotalCount int        `json:"total_count"`
}

type SourceListResponse struct {
	Nodes      []SourceState `json:"nodes"`
	TotalCount int           `json:"total_count"`
}

type WorkspaceListResponse struct {
	Nodes      []WorkspaceRef `json:"nodes"`
	TotalCount int            `json:"total_count"`
}

type SecretListResponse struct {
	Nodes      []SecretRef `json:"nodes"`
	TotalCount int         `json:"total_count"`
}

type TemplateListResponse struct {
	Nodes      []TemplateDescriptor `json:"nodes"`
	TotalCount int                  `json:"total_count"`
}
