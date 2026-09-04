package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
	"github.com/ang-ee/angee-operator/internal/manifest"
	"github.com/ang-ee/angee-operator/internal/runtime"
)

var (
	openBaoReadinessTimeout      = 30 * time.Second
	openBaoReadinessPollInterval = 500 * time.Millisecond
	openBaoProbeTimeout          = time.Second
	openBaoHTTPClient            = func() *http.Client { return &http.Client{Timeout: openBaoProbeTimeout} }
)

func (p *Platform) bootstrapOpenBao(ctx context.Context, stack *manifest.Stack, stdout io.Writer, stderr io.Writer) error {
	if stack.SecretsBackend.Type != "openbao" {
		return nil
	}
	service, ok := stack.Services["openbao"]
	if !ok || service.Runtime != manifest.RuntimeContainer {
		return nil
	}
	if ready, _ := openBaoReady(ctx, stack.SecretsBackend.Address, stack.SecretsBackend.Token); ready {
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
	waitCtx, cancel := context.WithTimeout(ctx, openBaoReadinessTimeout)
	defer cancel()
	var lastProbeErr error
	for {
		ready, probeErr := openBaoReady(waitCtx, stack.SecretsBackend.Address, stack.SecretsBackend.Token)
		if ready {
			if stderr != nil {
				_, _ = fmt.Fprintln(stderr, "OpenBao is ready; resolving stack secrets...")
			}
			finishWaiting(nil)
			return nil
		}
		if probeErr != nil && waitCtx.Err() == nil {
			lastProbeErr = probeErr
		}
		timer := time.NewTimer(openBaoReadinessPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			err := ctx.Err()
			finishWaiting(err)
			return err
		case <-waitCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctx.Err() != nil {
				err := ctx.Err()
				finishWaiting(err)
				return err
			}
			err := openBaoReadinessError(lastProbeErr)
			finishWaiting(err)
			return err
		case <-timer.C:
		}
	}
}

func openBaoReadinessError(lastProbeErr error) error {
	err := fmt.Errorf("OpenBao did not become ready within %s", openBaoReadinessTimeout)
	if lastProbeErr != nil {
		return fmt.Errorf("%w: last probe: %v", err, lastProbeErr)
	}
	return err
}

func openBaoReady(ctx context.Context, address string, token string) (bool, error) {
	if address == "" {
		return false, errors.New("OpenBao address is empty")
	}
	endpoint := strings.TrimRight(address, "/") + "/v1/sys/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	client := openBaoHTTPClient()
	trace := logctx.TraceHTTP(ctx, http.MethodGet, endpoint)
	resp, err := client.Do(req)
	if err != nil {
		trace(0, err)
		return false, err
	}
	defer resp.Body.Close()
	trace(resp.StatusCode, nil)
	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return true, nil
	}
	return false, fmt.Errorf("OpenBao health probe returned %s", resp.Status)
}
