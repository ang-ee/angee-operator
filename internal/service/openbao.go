package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

func (p *Platform) bootstrapOpenBao(ctx context.Context, stack *manifest.Stack, stdout io.Writer, stderr io.Writer) error {
	if stack.SecretsBackend.Type != "openbao" {
		return nil
	}
	service, ok := stack.Services["openbao"]
	if !ok || service.Runtime != manifest.RuntimeContainer {
		return nil
	}
	if openBaoReady(ctx, stack.SecretsBackend.Address, stack.SecretsBackend.Token) {
		return nil
	}
	if stderr != nil {
		_, _ = fmt.Fprintln(stderr, "OpenBao is not reachable; starting the openbao service first...")
	}
	bootstrap := *stack
	bootstrap.Secrets = nil
	bootstrap.Jobs = nil
	bootstrap.Services = map[string]manifest.Service{"openbao": service}
	compiled, err := Compile(&bootstrap, p.root, nil)
	if err != nil {
		return err
	}
	if err := p.writeCompiled(compiled); err != nil {
		return err
	}
	target := runtime.Target{Root: p.root, Services: []string{"openbao"}}
	finishStarting := logctx.Step(ctx, "starting OpenBao")
	var startErr error
	if stdout != nil || stderr != nil {
		startErr = p.composeBackend.UpForeground(ctx, target, stdout, stderr)
	} else {
		startErr = p.composeBackend.Up(ctx, target)
	}
	finishStarting(startErr)
	if startErr != nil {
		return startErr
	}
	if stderr != nil {
		_, _ = fmt.Fprintln(stderr, "Waiting for OpenBao to accept secret requests...")
	}
	finishWaiting := logctx.Step(ctx, "waiting for OpenBao")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if openBaoReady(ctx, stack.SecretsBackend.Address, stack.SecretsBackend.Token) {
			if stderr != nil {
				_, _ = fmt.Fprintln(stderr, "OpenBao is ready; resolving stack secrets...")
			}
			finishWaiting(nil)
			return nil
		}
		select {
		case <-ctx.Done():
			err := ctx.Err()
			finishWaiting(err)
			return err
		case <-time.After(500 * time.Millisecond):
		}
	}
	finishWaiting(nil)
	return nil
}

func openBaoReady(ctx context.Context, address string, token string) bool {
	if address == "" {
		return false
	}
	endpoint := strings.TrimRight(address, "/") + "/v1/sys/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	client := &http.Client{Timeout: time.Second}
	trace := logctx.TraceHTTP(ctx, http.MethodGet, endpoint)
	resp, err := client.Do(req)
	if err != nil {
		trace(0, err)
		return false
	}
	defer resp.Body.Close()
	trace(resp.StatusCode, nil)
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}
