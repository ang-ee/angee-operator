package operator

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ang-ee/angee-operator/api"
)

func (s *Server) sourceCheckout(w http.ResponseWriter, r *http.Request) {
	req, err := decodeStrict[api.SourceCheckoutRequest](r)
	if err != nil {
		writeBadRequest(w, err)
		return
	}
	started := false
	err = s.platform.WithSourceCheckout(r.Context(), req, func(ctx context.Context, checkout api.SourceCheckout) error {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		started = true
		if err := json.NewEncoder(w).Encode(checkout); err != nil {
			return err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return err
		}
		// The open response is the checkout scope, independent of any build.
		// The request and daemon lifecycle both cancel ctx.
		<-ctx.Done()
		return nil
	})
	if err != nil {
		if !started {
			writeError(w, err)
		} else {
			recordFailure(w, classifyServiceError(err))
		}
	}
}
