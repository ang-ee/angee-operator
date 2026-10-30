package platformclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ang-ee/angee-operator/api"
)

// WithSourceCheckout holds the streaming response open while use consumes its
// root-relative repository path. Closing the body releases the remote checkout;
// a cancelled request or terminated operator cancels the callback's context.
func (p *RemoteClient) WithSourceCheckout(ctx context.Context, req api.SourceCheckoutRequest, use func(context.Context, api.SourceCheckout) error) error {
	if use == nil {
		return fmt.Errorf("checkout consumer is required")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	body, err := jsonBody(req)
	if err != nil {
		return err
	}
	request, err := p.newRequest(ctx, http.MethodPost, p.endpoint("/source-checkouts", nil), body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.streamingHTTPClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		return operatorHTTPError(response.StatusCode, data)
	}
	var checkout api.SourceCheckout
	if err := json.NewDecoder(response.Body).Decode(&checkout); err != nil {
		return err
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = io.Copy(io.Discard, response.Body)
		cancel()
	}()
	err = use(ctx, checkout)
	scopeErr := ctx.Err()
	cancel()
	_ = response.Body.Close()
	<-finished
	if err != nil {
		return err
	}
	return scopeErr
}
