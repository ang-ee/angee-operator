package api

// SourceCheckoutRequest selects a complete Git repository at a branch, tag or
// commit. It never represents a local source's uncommitted working tree.
type SourceCheckoutRequest struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

// SourceCheckout is available only for the lifetime of its response or callback.
// Path is relative to the stack root: consumers must share that filesystem with
// the operator and resolve it against their own mount of the same stack root.
type SourceCheckout struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
}
