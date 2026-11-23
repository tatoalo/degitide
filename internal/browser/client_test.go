package browser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSendsWorkerBearerToken(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer worker-secret" {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "missing worker token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
	}))
	defer worker.Close()

	status, err := NewClient(worker.URL, "worker-secret").CheckSession(context.Background())
	if err != nil {
		t.Fatalf("check session: %v", err)
	}
	if !status.Valid {
		t.Fatal("expected a valid session response")
	}
}

func TestClientSubmitsOneEphemeralReauthentication(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tasks/reauthenticate" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer worker-secret" {
			t.Errorf("Authorization = %q", got)
			http.Error(w, "missing worker token", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]string
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request["username"] != "account-user" || request["password"] != "account-password" || request["totp"] != "123456" {
			t.Fatalf("unexpected request fields: %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"authenticated": true, "session_saved": true})
	}))
	defer worker.Close()

	result, err := NewClient(worker.URL, "worker-secret").Reauthenticate(
		context.Background(), "account-user", "account-password", "123456",
	)
	if err != nil {
		t.Fatalf("reauthenticate: %v", err)
	}
	if !result.Authenticated || !result.SessionSaved {
		t.Fatalf("unexpected result: %#v", result)
	}
}
