package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/scheduler"
)

func TestScheduleSettingsAPI(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admin, err := database.CreateAdmin("admin", "unused")
	if err != nil {
		t.Fatal(err)
	}
	planner := scheduler.New(database, nil, "market")
	defer planner.Stop()
	handler := New(database, nil, planner, Options{APIToken: "test-api-key", AllowedHosts: "127.0.0.1"}).Router()
	request := func(method, path, body, origin, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if token != "" {
			req.AddCookie(&http.Cookie{Name: dashboardSessionCookie, Value: dashboardSessionValue(token, admin)})
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		result := request(method, "/api/schedule", "{}", "http://127.0.0.1", "")
		if result.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", method, result.Code)
		}
	}
	settings := planner.Settings("").Settings
	settings.IntervalMinutes = 90
	settings.CloseDelayMinutes = 30
	encoded, _ := json.Marshal(settings)
	for _, method := range []string{"PUT", "DELETE"} {
		result := request(method, "/api/schedule", string(encoded), "https://attacker.example", "test-api-key")
		if result.Code != http.StatusForbidden {
			t.Fatalf("cross-origin %s = %d", method, result.Code)
		}
	}
	for _, body := range []string{"{}", string(encoded) + " {}", strings.TrimSuffix(string(encoded), "}") + `,"extra":true}`, strings.Repeat(" ", 2049) + string(encoded)} {
		if result := request("PUT", "/api/schedule", body, "http://127.0.0.1", "test-api-key"); result.Code != 400 {
			t.Fatalf("malformed write = %d: %s", result.Code, result.Body.String())
		}
	}
	result := request("PUT", "/api/schedule", string(encoded), "http://127.0.0.1", "test-api-key")
	if result.Code != 200 {
		t.Fatalf("save = %d: %s", result.Code, result.Body.String())
	}
	if planner.IsRunning() {
		t.Fatal("settings write started broker work")
	}
	var view scheduler.SettingsView
	result = request("GET", "/api/schedule", "", "", "test-api-key")
	if err := json.Unmarshal(result.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Source != "dashboard" || view.Settings != settings {
		t.Fatalf("settings not saved: %+v", view)
	}
	if result := request("DELETE", "/api/schedule", "{}", "http://127.0.0.1", "test-api-key"); result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	if got := planner.Settings(""); got.Source != "environment" || got.Settings.IntervalMinutes != 60 {
		t.Fatal(got)
	}

	if err := database.Create(&db.Account{ID: "second", Name: "Tilde"}).Error; err != nil {
		t.Fatal(err)
	}
	scoped := func(method, path string) *httptest.ResponseRecorder {
		return request(method, path, string(encoded), "http://127.0.0.1", "test-api-key")
	}
	for query, want := range map[string]int{"account=%ZZ": 400, "account=missing": 404, "account=": 400, "account=bad.id": 400, "account=default&account=second": 400} {
		if result := scoped("PUT", "/api/schedule?"+query); result.Code != want {
			t.Fatalf("invalid scope %q: %d", query, result.Code)
		}
	}
	for _, method := range []string{"PUT", "GET"} {
		result := scoped(method, "/api/schedule?account=second")
		if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &view) != nil || view.Settings != settings || view.Source != "account" {
			t.Fatalf("%s did not return the second account's saved settings: %s", method, result.Body)
		}
	}
	for id, minutes := range map[string]int{"default": 60, "second": 90} {
		result := scoped("GET", "/api/status?account="+id)
		var status struct{ Schedule scheduler.ScheduleStatus }
		if err := json.Unmarshal(result.Body.Bytes(), &status); err != nil || status.Schedule.IntervalMinutes != minutes {
			t.Fatalf("wrong schedule in %s overview: %s", id, result.Body)
		}
	}
	result = scoped("DELETE", "/api/schedule?account=second")
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &view) != nil || view.Source != "workspace" || view.Settings.IntervalMinutes != 60 {
		t.Fatalf("account did not return to workspace defaults: %s", result.Body)
	}
}
