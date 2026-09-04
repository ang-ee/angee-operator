package service

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

type stalledOpenBaoTransport struct{}

func (stalledOpenBaoTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestBootstrapOpenBaoReturnsReadinessTimeout(t *testing.T) {
	oldTimeout := openBaoReadinessTimeout
	oldInterval := openBaoReadinessPollInterval
	oldProbeTimeout := openBaoProbeTimeout
	oldHTTPClient := openBaoHTTPClient
	openBaoReadinessTimeout = 30 * time.Millisecond
	openBaoReadinessPollInterval = time.Millisecond
	openBaoProbeTimeout = 5 * time.Millisecond
	openBaoHTTPClient = func() *http.Client {
		return &http.Client{Timeout: openBaoProbeTimeout, Transport: stalledOpenBaoTransport{}}
	}
	t.Cleanup(func() {
		openBaoReadinessTimeout = oldTimeout
		openBaoReadinessPollInterval = oldInterval
		openBaoProbeTimeout = oldProbeTimeout
		openBaoHTTPClient = oldHTTPClient
	})

	platform, err := NewWithBackends(t.TempDir(), stubStatusBackend{}, stubStatusBackend{})
	if err != nil {
		t.Fatalf("NewWithBackends() error = %v", err)
	}
	stack := &manifest.Stack{
		Version: manifest.VersionCurrent,
		Kind:    manifest.KindStack,
		Name:    "openbao-test",
		SecretsBackend: manifest.SecretsBackend{
			Type:    "openbao",
			Address: "http://openbao.invalid",
		},
		Services: map[string]manifest.Service{
			"openbao": {Runtime: manifest.RuntimeContainer, Image: "openbao/openbao:latest"},
		},
	}

	started := time.Now()
	err = platform.bootstrapOpenBao(t.Context(), stack, io.Discard, io.Discard)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bootstrapOpenBao() took %s, want shortened timeout", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "OpenBao did not become ready within 30ms") {
		t.Fatalf("bootstrapOpenBao() error = %v, want readiness timeout", err)
	}
	if !strings.Contains(err.Error(), "last probe:") {
		t.Fatalf("bootstrapOpenBao() error = %v, want last probe error", err)
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("bootstrapOpenBao() error = %v, want stalled probe timeout", err)
	}
}
