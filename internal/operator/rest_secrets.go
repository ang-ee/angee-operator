package operator

import (
	"net/http"

	"github.com/ang-ee/angee-operator/api"
	"github.com/ang-ee/angee-operator/internal/logctx"
)

// Secrets CRUD REST handlers. Auth-gated identically to every other
// protected route via s.auth() at mux registration time. Mutating
// handlers log the operation through the request logger so env-file
// deployments (no backend-native audit) still have a paper trail; OpenBao
// keeps its own audit log.

func (s *Server) secretsList(w http.ResponseWriter, r *http.Request) {
	q, err := parseListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	nodes, total, err := s.platform.SecretsList(r.Context(), q)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.SecretListResponse{Nodes: nodes, TotalCount: total})
}

func (s *Server) secretGet(w http.ResponseWriter, r *http.Request) {
	ref, err := s.platform.SecretGet(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) secretValue(w http.ResponseWriter, r *http.Request) {
	resp, err := s.platform.SecretValue(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) secretSet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	req, err := decode[api.SecretSetRequest](r)
	if err != nil {
		auditSecretAttempt(r, "set", name, "decode")
		writeBadRequest(w, err)
		return
	}
	ref, err := s.platform.SecretSet(r.Context(), name, req.Value)
	if err != nil {
		auditSecretAttempt(r, "set", name, err.Error())
		writeError(w, err)
		return
	}
	auditSecretMutation(r, "set", name)
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) secretDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.platform.SecretDelete(r.Context(), name); err != nil {
		auditSecretAttempt(r, "delete", name, err.Error())
		writeError(w, err)
		return
	}
	auditSecretMutation(r, "delete", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
}

// auditSecretMutation logs a single line per successful mutating call
// so an operator running against env-file (no backend-native audit) still has
// a paper trail. The request-scoped logger adds the request ID.
func auditSecretMutation(r *http.Request, op, name string) {
	logctx.From(r.Context()).InfoContext(r.Context(), "secret mutation",
		"operation", op,
		"name", name,
		"remote", r.RemoteAddr,
	)
}

// auditSecretAttempt logs failed mutating calls so the audit trail
// reflects rejected attempts (oversized values, malformed names,
// validation failures) — security-relevant signal that a successful
// log alone would miss.
func auditSecretAttempt(r *http.Request, op, name, reason string) {
	logctx.From(r.Context()).WarnContext(r.Context(), "secret mutation failed",
		"operation", op,
		"name", name,
		"remote", r.RemoteAddr,
		"reason", reason,
	)
}
