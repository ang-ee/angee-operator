package operator

import (
	"fmt"
	"net/http"
	"os"

	"github.com/ang-ee/angee-operator/api"
)

// File read/write REST handlers. Auth-gated identically to every other
// protected route via s.auth() at mux registration time. source and path are
// query parameters rather than path segments because a file path holds slashes.
// The mutating handler logs the operation to stderr so deployments without a
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
// without a backend-native audit still has a paper trail in stderr. RemoteAddr
// and the client-supplied source/path are %q'd for defense-in-depth against any
// middleware that might inject control characters into the fields.
func auditFileMutation(r *http.Request, op, source, path string) {
	fmt.Fprintf(os.Stderr, "operator: file %s source=%q path=%q remote=%q\n", op, source, path, r.RemoteAddr)
}

// auditFileAttempt logs failed file writes so the audit trail reflects rejected
// attempts (traversal, oversized or non-UTF-8 content, etag conflicts) —
// security-relevant signal a success-only log would miss.
func auditFileAttempt(r *http.Request, op, source, path, reason string) {
	fmt.Fprintf(os.Stderr, "operator: file %s attempt failed source=%q path=%q remote=%q reason=%q\n", op, source, path, r.RemoteAddr, reason)
}
