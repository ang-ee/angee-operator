package operator

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBackupRoutesRequireConfiguredAdminBearer(t *testing.T) {
	root := t.TempDir()
	writeTestStack(t, root, "version: 1\nkind: stack\nname: test\n")
	server, err := NewServer(Config{Root: root, Bind: "127.0.0.1", Port: 9000, Token: "backup-admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	minted, err := server.tokens.MintConnection("browser-user", []string{"*", "backup", "admin"}, "1h")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{{http.MethodPost, "/backups/preview"}, {http.MethodPost, "/backups"}, {http.MethodGet, "/backups/example/archive"}} {
		for _, credential := range []struct {
			value  string
			status int
		}{{"", http.StatusUnauthorized}, {"Bearer bad", http.StatusUnauthorized}, {"Bearer " + minted.Token, http.StatusForbidden}} {
			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{"version":1,"selection":{"version":1}}`))
			req.Header.Set("Authorization", credential.value)
			response := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(response, req)
			if response.Code != credential.status {
				t.Fatalf("%s %s status=%d expected=%d", route.method, route.path, response.Code, credential.status)
			}
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/backups/preview", bytes.NewBufferString(`{"version":1,"selection":{"version":1}}`))
	req.Header.Set("Authorization", "Bearer backup-admin")
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("admin preview=%d %s", response.Code, response.Body.String())
	}
	// Strict transport decoding catches a stale client spelling before any capture.
	req = httptest.NewRequest(http.MethodPost, "/backups/preview", bytes.NewBufferString(`{"version":1,"selection":{"services":{"mode":"all"}}}`))
	req.Header.Set("Authorization", "Bearer backup-admin")
	response = httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status=%d %s", response.Code, response.Body.String())
	}
}
