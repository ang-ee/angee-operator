package operator

import (
	"net/http"
	"net/url"
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
	// Through Caddy forward_auth the client's original URI (carrying ?token=)
	// arrives in X-Forwarded-Uri; r.URL is the /edge/verify subrequest itself,
	// so its query holds ?service=, not the client token (spike-validated).
	// Prefer X-Forwarded-Uri, then the direct request URL (non-forward_auth
	// callers), then Authorization, then Sec-WebSocket-Protocol.
	if xfu := r.Header.Get("X-Forwarded-Uri"); xfu != "" {
		if u, err := url.Parse(xfu); err == nil {
			if token := strings.TrimSpace(u.Query().Get("token")); token != "" {
				return token
			}
		}
	}
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
