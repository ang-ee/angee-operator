package operator

import (
	"net/http"
	"strings"

	"github.com/fyltr/angee/api"
)

func (s *Server) edgeVerify(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	if service == "" {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
		return
	}
	raw := edgeToken(r)
	if raw == "" {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
		return
	}
	if _, err := s.tokens.Verify(raw, serviceAudience(service)); err != nil {
		writeJSON(w, http.StatusUnauthorized, api.ErrorResponse{Error: "unauthorized"})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func edgeToken(r *http.Request) string {
	if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		return token
	}
	if token, ok := parseBearer(r.Header.Get("Authorization")); ok && token != "" {
		return token
	}
	protocol := r.Header.Get("Sec-WebSocket-Protocol")
	token, _, _ := strings.Cut(protocol, ",")
	return strings.TrimSpace(token)
}
