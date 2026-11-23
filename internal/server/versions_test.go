package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
)

func TestVersionsUsesWorkerMetadataWithoutBrowserTasks(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer worker-secret" {
			t.Errorf("unexpected worker request: %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeJSON(w, browser.WorkerInfo{Version: "v2.3.4", Python: "3.12.12", Patchright: "1.62.1"})
	}))
	defer worker.Close()
	service := New(nil, browser.NewClient(worker.URL, "worker-secret"), nil, Options{Version: "v2.3.5"})
	response := httptest.NewRecorder()
	service.handleVersions(response, httptest.NewRequest(http.MethodGet, "/api/versions", nil))
	var data struct {
		App    string
		Worker browser.WorkerInfo
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.App != "v2.3.5" || data.Worker.Version != "v2.3.4" || data.Worker.Patchright != "1.62.1" || data.Worker.Python != "3.12.12" {
		t.Fatalf("versions = %+v", data)
	}
}

func TestVersionsSurvivesUnavailableWorker(t *testing.T) {
	worker := httptest.NewServer(http.NotFoundHandler())
	defer worker.Close()
	service := New(nil, browser.NewClient(worker.URL), nil)
	response := httptest.NewRecorder()
	service.handleVersions(response, httptest.NewRequest(http.MethodGet, "/api/versions", nil))
	var data map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || data["app"] != "dev" || data["worker"] != nil {
		t.Fatalf("versions = %+v", data)
	}
}

func TestVersionsRequiresDashboardAuthentication(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.CreateAdmin("admin", "unused"); err != nil {
		t.Fatal(err)
	}
	service := New(database, nil, nil, Options{APIToken: "test-token"})
	response := httptest.NewRecorder()
	service.Router().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost/api/versions", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
}
