package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tatoalo/degitide/internal/db"
)

func TestFailureDeduplicatesAcrossRestartAndSendsRecovery(t *testing.T) {
	var delivered []string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered = append(delivered, string(body))
		if r.Header.Get("Authorization") != "Bearer test" {
			t.Error("missing relay auth")
		}
		w.WriteHeader(204)
	}))
	defer relay.Close()
	database, err := db.New(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	account := db.Account{ID: "default", Name: "Personal"}
	d := New(database, relay.URL, "test")
	d.Report(account, nil)
	d.Report(account, errors.New("secret password and balance 1234"))
	d.Report(account, errors.New("same failure"))
	d.Close()
	d = New(database, relay.URL, "test")
	d.Report(account, errors.New("still failing"))
	d.Report(account, nil)
	d.Report(account, nil)
	d.Close()
	if len(delivered) != 2 {
		t.Fatalf("expected failure and recovery, got %d", len(delivered))
	}
	if !strings.Contains(delivered[0], `"kind":"failed"`) || !strings.Contains(delivered[1], `"kind":"recovered"`) {
		t.Fatalf("wrong notifications: %v", delivered)
	}
	if strings.Contains(strings.Join(delivered, ""), "password") || strings.Contains(strings.Join(delivered, ""), "1234") {
		t.Fatal("sensitive details leaked")
	}
}

func TestProvidersUseExpectedFieldsAndValidateAcknowledgement(t *testing.T) {
	for _, kind := range []string{"telegram", "pushover"} {
		t.Run(kind, func(t *testing.T) {
			accepted := true
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm()
				if r.Method != "POST" {
					t.Error("wrong method")
				}
				if kind == "telegram" {
					if r.Form.Get("chat_id") != "recipient" || !strings.Contains(r.Form.Get("text"), "Personal") {
						t.Error("wrong Telegram fields")
					}
				} else {
					if r.Form.Get("user") != "recipient" || r.Form.Get("token") != "token" || !strings.Contains(r.Form.Get("message"), "Personal") {
						t.Error("wrong Pushover fields")
					}
				}
				if !accepted {
					io.WriteString(w, `{"ok":false,"status":0}`)
				} else {
					io.WriteString(w, `{"ok":true,"status":1}`)
				}
			}))
			defer provider.Close()
			p := Provider{Kind: kind, Token: "token", Recipient: "recipient", Endpoint: provider.URL}
			event := Event{AccountID: "default", AccountName: "Personal", Kind: "failed"}
			if err := p.Send(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			accepted = false
			if err := p.Send(context.Background(), event); err == nil {
				t.Fatal("provider rejection ignored")
			}
		})
	}
}

func TestRelayRejectsUnauthorizedOrOversharedEvents(t *testing.T) {
	handler := Handler("test", Provider{})
	for _, test := range []struct {
		token, body string
		want        int
	}{{"", `{}`, 401}, {"test", `{"account_id":"../other","account_name":"Main","kind":"failed"}`, 400}, {"test", `{"account_id":"main","account_name":"Main","kind":"failed","password":"secret"}`, 400}} {
		request := httptest.NewRequest("POST", "/notify", strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer "+test.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("status %d want %d", response.Code, test.want)
		}
	}
}
