package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSHUsesArgumentArrayAndLoopbackForward(t *testing.T) {
	args, err := sshArgs("owner@my-vps", 18391, 8391)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "127.0.0.1:18391:127.0.0.1:8391") || !strings.Contains(joined, "ExitOnForwardFailure=yes") || args[len(args)-2] != "--" {
		t.Fatalf("unsafe SSH arguments: %v", args)
	}
	for _, destination := range []string{"-oProxyCommand=bad", "host;bad", "user@host $(bad)", "host\nother"} {
		if _, err := sshArgs(destination, 18391, 8391); err == nil {
			t.Errorf("accepted %q", destination)
		}
	}
}
func TestDashboardURLRefusesRemotePlaintextAndURLCredentials(t *testing.T) {
	for _, address := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "javascript:alert(1)"} {
		if validateDashboardURL(address) == nil {
			t.Errorf("accepted %s", address)
		}
	}
	for _, address := range []string{"http://127.0.0.1:18391", "http://[::1]:8391", "https://portfolio.example.com"} {
		if err := validateDashboardURL(address); err != nil {
			t.Errorf("rejected %s: %v", address, err)
		}
	}
}
func TestCLIAuthenticatesAndKeepsAccountScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "degitide" || password != "test" {
			t.Error("missing CLI authentication")
		}
		if r.URL.Query().Get("account") != "savings" || r.Header.Get("Origin") == "" {
			t.Error("missing scope/origin")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := cliClient{base: server.URL, token: "test", http: server.Client()}
	var result map[string]bool
	if err := client.api(context.Background(), "POST", "/api/pull/trigger?account=savings", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if !result["ok"] {
		t.Fatal("missing response")
	}
}
