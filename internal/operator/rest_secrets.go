package operator

import (
	"fmt"
	"net/http"
	"os"

	"github.com/ang-ee/angee-operator/api"
)

// Secrets CRUD REST handlers. Auth-gated identically to every other
// protected route via s.auth() at mux registration time. Mutating
// handlers log the operation to stderr so env-file deployments (no
// backend-native audit) still have a paper trail; OpenBao keeps its
// own audit log.

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
// so an operator running against env-file (no backend-native audit)
// still has a paper trail in stderr. RemoteAddr is %q'd for
// defense-in-depth against any future middleware that might inject
// control characters into the field (stdlib net/http produces a safe
// host:port form by default).
func auditSecretMutation(r *http.Request, op, name string) {
	fmt.Fprintf(os.Stderr, "operator: secret %s name=%q remote=%q\n", op, name, r.RemoteAddr)
}

// auditSecretAttempt logs failed mutating calls so the audit trail
// reflects rejected attempts (oversized values, malformed names,
// validation failures) — security-relevant signal that a successful
// log alone would miss.
func auditSecretAttempt(r *http.Request, op, name, reason string) {
	fmt.Fprintf(os.Stderr, "operator: secret %s attempt failed name=%q remote=%q reason=%q\n", op, name, r.RemoteAddr, reason)
}
