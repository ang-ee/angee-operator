package operator

import (
	"net/http"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

// File read/write REST handlers. Auth-gated identically to every other
// protected route via s.auth() at mux registration time. source and path are
// query parameters rather than path segments because a file path holds slashes.
// The mutating handler uses the request logger so deployments without a
// backend-native audit still have a paper trail, mirroring the secrets routes.

func (s *Server) fileGet(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	path := r.URL.Query().Get("path")
	content, err := s.platform.FileRead(r.Context(), source, path)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, content)
}

func (s *Server) filePut(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	path := r.URL.Query().Get("path")
	req, err := decode[api.FileWriteRequest](r)
	if err != nil {
		auditFileAttempt(r, "write", source, path, "decode")
		writeBadRequest(w, err)
		return
	}
	ref, err := s.platform.FileWrite(r.Context(), source, path, req.Content, req.Etag)
	if err != nil {
		auditFileAttempt(r, "write", source, path, err.Error())
		writeError(w, err)
		return
	}
	auditFileMutation(r, "write", source, path)
	writeJSON(w, http.StatusOK, ref)
}

// auditFileMutation logs a single line per successful file write so an operator
// without a backend-native audit still has a paper trail. The request-scoped
// logger adds the request ID.
func auditFileMutation(r *http.Request, op, source, path string) {
	logctx.From(r.Context()).InfoContext(r.Context(), "file mutation",
		"operation", op,
		"source", source,
		"path", path,
		"remote", r.RemoteAddr,
	)
}

// auditFileAttempt logs failed file writes so the audit trail reflects rejected
// attempts (traversal, oversized or non-UTF-8 content, etag conflicts) —
// security-relevant signal a success-only log would miss.
func auditFileAttempt(r *http.Request, op, source, path, reason string) {
	logctx.From(r.Context()).WarnContext(r.Context(), "file mutation failed",
		"operation", op,
		"source", source,
		"path", path,
		"remote", r.RemoteAddr,
		"reason", reason,
	)
}
