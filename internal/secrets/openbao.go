package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

type OpenBaoConfig struct {
	Address string
	Mount   string
	Path    string
	Token   string
}

type OpenBaoBackend struct {
	config OpenBaoConfig
	client *http.Client
}

func NewOpenBaoBackend(config OpenBaoConfig) *OpenBaoBackend {
	if config.Address == "" {
		config.Address = os.Getenv("OPENBAO_ADDR")
	}
	if config.Token == "" {
		config.Token = os.Getenv("OPENBAO_TOKEN")
	}
	if config.Mount == "" {
		config.Mount = "secret"
	}
	if config.Path == "" {
		config.Path = "angee"
	}
	return &OpenBaoBackend{config: config, client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *OpenBaoBackend) Get(ctx context.Context, key string) (string, bool, error) {
	var resp struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	status, err := b.request(ctx, http.MethodGet, b.dataPath(key), nil, &resp)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	value, ok := resp.Data.Data["value"]
	return value, ok, nil
}

func (b *OpenBaoBackend) Set(ctx context.Context, key, value string) error {
	body := map[string]any{"data": map[string]string{"value": value}}
	_, err := b.request(ctx, http.MethodPost, b.dataPath(key), body, nil)
	return err
}

func (b *OpenBaoBackend) Delete(ctx context.Context, key string) error {
	_, err := b.request(ctx, http.MethodDelete, b.dataPath(key), nil, nil)
	return err
}

func (b *OpenBaoBackend) List(ctx context.Context) ([]string, error) {
	return nil, fmt.Errorf("openbao list is not implemented")
}

func (b *OpenBaoBackend) dataPath(key string) string {
	parts := []string{strings.Trim(b.config.Mount, "/"), "data", strings.Trim(b.config.Path, "/"), key}
	return "/v1/" + strings.Join(parts, "/")
}

func (b *OpenBaoBackend) request(ctx context.Context, method, path string, body any, out any) (int, error) {
	if b.config.Address == "" {
		return 0, fmt.Errorf("openbao address is required")
	}
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	endpoint := strings.TrimRight(b.config.Address, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, err
	}
	if b.config.Token != "" {
		req.Header.Set("X-Vault-Token", b.config.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	trace := logctx.TraceHTTP(ctx, method, endpoint)
	resp, err := b.client.Do(req)
	if err != nil {
		trace(0, err)
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		trace(resp.StatusCode, nil)
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("openbao request failed with status %d", resp.StatusCode)
		trace(resp.StatusCode, err)
		return resp.StatusCode, err
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			trace(resp.StatusCode, err)
			return resp.StatusCode, err
		}
	}
	trace(resp.StatusCode, nil)
	return resp.StatusCode, nil
}
