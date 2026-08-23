package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/plugin"
	"github.com/gin-gonic/gin"
)

func newPluginHandlerTestRouter(manager *plugin.Manager) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.ErrorHandler())
	handler := NewPluginHandler(manager)
	router.GET("/plugins", handler.List)
	router.GET("/plugins/:id", handler.Get)
	router.GET("/plugins/:id/audit", handler.ListAudit)
	router.POST("/plugins/:id/restart", handler.Restart)
	return router
}

func newPluginHandlerTestManager(t *testing.T) *plugin.Manager {
	t.Helper()
	root := t.TempDir()
	pluginDir := filepath.Join(root, "files")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("create plugin directory: %v", err)
	}
	manifest := `apiVersion: weknora.plugin/v1
kind: Plugin
metadata:
  id: files
  name: Files
  version: 1.0.0
  description: safe description
spec:
  extensionType: datasource
  weknoraVersion: ">=1.0.0"
  entrypoint:
    type: process
    command: ["/private/plugin-binary"]
    grpcAddress: unix:///private/plugin.sock
  permissions:
    network:
      enabled: true
    filesystem:
      readOnly: ["/private/data"]
`
	if err := os.WriteFile(filepath.Join(pluginDir, plugin.ManifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	manager := plugin.NewManager(root)
	if err := manager.Discover(); err != nil {
		t.Fatalf("discover plugins: %v", err)
	}
	return manager
}

func TestPluginHandlerListAndGetRedactDeploymentDetails(t *testing.T) {
	router := newPluginHandlerTestRouter(newPluginHandlerTestManager(t))
	for _, requestPath := range []string{"/plugins", "/plugins/files"} {
		writer := httptest.NewRecorder()
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if writer.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d body=%s", requestPath, writer.Code, writer.Body.String())
		}
		body := writer.Body.String()
		for _, secret := range []string{"/private/plugin-binary", "plugin.sock", "/private/data"} {
			if strings.Contains(body, secret) {
				t.Fatalf("GET %s leaked deployment detail %q: %s", requestPath, secret, body)
			}
		}
		if !strings.Contains(body, `"status":"discovered"`) || !strings.Contains(body, `"extension_type":"datasource"`) {
			t.Fatalf("GET %s missing safe status projection: %s", requestPath, body)
		}
	}
}

func TestPluginHandlerGetAndAuditRejectUnknownPlugin(t *testing.T) {
	router := newPluginHandlerTestRouter(newPluginHandlerTestManager(t))
	for _, requestPath := range []string{"/plugins/missing", "/plugins/missing/audit"} {
		writer := httptest.NewRecorder()
		router.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if writer.Code != http.StatusNotFound {
			t.Fatalf("GET %s: expected 404, got %d body=%s", requestPath, writer.Code, writer.Body.String())
		}
	}
}

func TestPluginHandlerAuditFiltersAndRedactsSensitiveFields(t *testing.T) {
	manager := newPluginHandlerTestManager(t)
	manager.RecordNetworkDenied("files", "https://private.example/token", "downstream error contains secret")
	manager.RecordNetworkDenied("other", "https://other.example", "other message")

	writer := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plugins/files/audit?action=plugin.network_denied&limit=1", nil)
	newPluginHandlerTestRouter(manager).ServeHTTP(writer, req)
	if writer.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", writer.Code, writer.Body.String())
	}
	var response struct {
		Success bool                       `json:"success"`
		Data    []pluginAuditEventResponse `json:"data"`
	}
	if err := json.Unmarshal(writer.Body.Bytes(), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !response.Success || len(response.Data) != 1 || response.Data[0].PluginID != "files" {
		t.Fatalf("unexpected filtered response: %#v", response)
	}
	body := writer.Body.String()
	for _, secret := range []string{"private.example", "downstream error", "other.example", "other message"} {
		if strings.Contains(body, secret) {
			t.Fatalf("audit response leaked %q: %s", secret, body)
		}
	}
}

func TestPluginHandlerAuditRejectsInvalidLimit(t *testing.T) {
	writer := httptest.NewRecorder()
	newPluginHandlerTestRouter(newPluginHandlerTestManager(t)).ServeHTTP(
		writer,
		httptest.NewRequest(http.MethodGet, "/plugins/files/audit?limit=513", nil),
	)
	if writer.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", writer.Code, writer.Body.String())
	}
}

func TestPluginHandlerRestartMapsUnknownAndNonFailedErrors(t *testing.T) {
	router := newPluginHandlerTestRouter(newPluginHandlerTestManager(t))
	for _, testCase := range []struct {
		path string
		code int
	}{
		{path: "/plugins/missing/restart", code: http.StatusNotFound},
		{path: "/plugins/files/restart", code: http.StatusConflict},
	} {
		writer := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(`{"config":{"rootPath":"/tmp"}}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(writer, req)
		if writer.Code != testCase.code {
			t.Fatalf("POST %s: expected %d, got %d body=%s", testCase.path, testCase.code, writer.Code, writer.Body.String())
		}
	}
}
