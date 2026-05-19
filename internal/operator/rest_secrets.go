package operator

import (
	"fmt"
	"net/http"
	"os"

	"github.com/fyltr/angee/api"
)

// Secrets CRUD REST handlers. Auth-gated identically to every other
// protected route via s.auth() at mux registration time. Mutating
// handlers log the operation to stderr so env-file deployments (no
// backend-native audit) still have a paper trail; OpenBao keeps its
// own audit log.

func (s *Server) secretsList(w http.ResponseWriter, r *http.Request) {
	refs, err := s.platform.SecretsList(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, refs)
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
		writeBadRequest(w, err)
		return
	}
	ref, err := s.platform.SecretSet(r.Context(), name, req.Value)
	if err != nil {
		writeError(w, err)
		return
	}
	auditSecretMutation(r, "set", name)
	writeJSON(w, http.StatusOK, ref)
}

func (s *Server) secretDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.platform.SecretDelete(r.Context(), name); err != nil {
		writeError(w, err)
		return
	}
	auditSecretMutation(r, "delete", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
}

// auditSecretMutation logs a single line per mutating call so an
// operator running against env-file (no backend-native audit) still
// has a paper trail in stderr. Includes the remote address as the only
// caller-identity hint available today; per-actor tokens would replace
// this with the JWT subject in a future iteration.
func auditSecretMutation(r *http.Request, op, name string) {
	fmt.Fprintf(os.Stderr, "operator: secret %s name=%q remote=%s\n", op, name, r.RemoteAddr)
}
