package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/simplefin"
)

func secretHash(s string) string {
	digest := sha256.Sum256([]byte(s))
	return hex.EncodeToString(digest[:])
}

// This mux is separate from dashboard authentication. Its only capabilities
// are a one-time claim and reads of a scoped, normalized feed.
func (s *Server) simpleFINRouter(dashboard http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /simplefin/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "1.0.7\n2.0.0\n")
	})
	mux.HandleFunc("POST /simplefin/claim/{token}", s.claimSimpleFIN)
	mux.HandleFunc("GET /simplefin/access/accounts", s.readSimpleFIN)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/simplefin/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			mux.ServeHTTP(w, r)
			return
		}
		dashboard.ServeHTTP(w, r)
	})
}

func (s *Server) createSimpleFIN(w http.ResponseWriter, r *http.Request) {
	if s.options.APIToken == "" {
		writeError(w, 409, errors.New("configure dashboard authentication before connecting Sure"))
		return
	}
	var input struct {
		Name                string   `json:"name"`
		BaseURL             string   `json:"base_url"`
		AccountIDs          []string `json:"account_ids"`
		IncludeTransactions bool     `json:"include_transactions"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, 400, errors.New("invalid connection request"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, 400, errors.New("invalid connection request"))
		return
	}
	base, err := s.simpleFINBaseURL(input.BaseURL)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		input.Name = "Sure"
	}
	if len(input.Name) > 100 {
		writeError(w, 400, errors.New("connection name is too long"))
		return
	}
	accounts, err := s.db.Accounts()
	if err != nil {
		writeError(w, 500, errors.New("accounts unavailable"))
		return
	}
	selected := []string{}
	for _, account := range accounts {
		if len(input.AccountIDs) == 0 {
			selected = append(selected, account.ID)
			continue
		}
		for _, id := range input.AccountIDs {
			if id == account.ID {
				selected = append(selected, id)
				break
			}
		}
	}
	if len(selected) == 0 || (len(input.AccountIDs) > 0 && len(selected) != len(input.AccountIDs)) {
		writeError(w, 400, errors.New("select valid, distinct account IDs"))
		return
	}
	ids, _ := json.Marshal(selected)
	token, err := newAuthCSRFToken()
	if err != nil {
		writeError(w, 500, errors.New("cannot create token"))
		return
	}
	id, err := newAuthCSRFToken()
	if err != nil {
		writeError(w, 500, errors.New("cannot create connection"))
		return
	}
	now := time.Now().UTC()
	grant := db.SimpleFINConnection{ID: id, Name: input.Name, BaseURL: base, AccountIDs: string(ids), ClaimHash: secretHash(token), CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute), IncludeTransactions: input.IncludeTransactions}
	if err := s.db.Create(&grant).Error; err != nil {
		writeError(w, 500, errors.New("cannot save connection"))
		return
	}
	setup := base64.StdEncoding.EncodeToString([]byte(base + "/simplefin/claim/" + token))
	writeJSONStatus(w, 201, map[string]any{"id": id, "setup_token": setup, "expires_at": grant.ExpiresAt, "account_ids": selected, "include_transactions": input.IncludeTransactions})
}

func (s *Server) simpleFINBaseURL(value string) (string, error) {
	invalid := errors.New("enter only the Degitide address, such as https://portfolio.example.com, without a path, query, or login details")
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", invalid
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "degitide-sure" || host == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return "", errors.New("use an HTTPS address, or choose Same Docker host and follow the private-network setup instructions")
	}
	allowed := false
	for _, candidate := range strings.Split(s.options.AllowedHosts, ",") {
		if normalizeHostname(candidate) == host {
			allowed = true
		}
	}
	if !allowed {
		if host == "degitide-sure" {
			return "", errors.New("the private Docker connection is not enabled yet; open the server setup instructions and add docker-compose.sure.yml to your Degitide deployment")
		}
		return "", fmt.Errorf("add %s to DEGITIDE_ALLOWED_HOSTS in your Degitide configuration, keep any existing hosts, then recreate the app and try again", host)
	}
	u.Path = ""
	u.RawPath = ""
	return u.String(), nil
}

func (s *Server) listSimpleFIN(w http.ResponseWriter, r *http.Request) {
	grants := []db.SimpleFINConnection{}
	if err := s.db.Order("created_at DESC").Find(&grants).Error; err != nil {
		writeError(w, 500, errors.New("connections unavailable"))
		return
	}
	// Account scope is decoded for the administrator; secrets are never returned.
	result := []map[string]any{}
	for _, grant := range grants {
		var ids []string
		_ = json.Unmarshal([]byte(grant.AccountIDs), &ids)
		result = append(result, map[string]any{"id": grant.ID, "name": grant.Name, "base_url": grant.BaseURL, "account_ids": ids, "include_transactions": grant.IncludeTransactions, "expires_at": grant.ExpiresAt, "claimed_at": grant.ClaimedAt, "last_used_at": grant.LastUsedAt, "revoked_at": grant.RevokedAt})
	}
	writeJSON(w, result)
}
func (s *Server) revokeSimpleFIN(w http.ResponseWriter, r *http.Request) {
	result := s.db.Model(&db.SimpleFINConnection{}).Where("id = ?", r.PathValue("id")).Updates(map[string]any{"revoked_at": time.Now().UTC(), "access_hash": ""})
	if result.Error != nil {
		writeError(w, 500, errors.New("cannot revoke connection"))
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, 404, errors.New("connection not found"))
		return
	}
	writeJSON(w, map[string]string{"status": "revoked"})
}
func (s *Server) claimSimpleFIN(w http.ResponseWriter, r *http.Request) {
	// Claims are bearer capabilities. Never log the URL or accept a dashboard
	// session as a replacement for the one-time token.
	denied := func() {
		http.Error(w, "Setup token is invalid, expired, already claimed or revoked. Generate a new token in Degitide.", 403)
	}
	token := r.PathValue("token")
	if !validAuthCSRFToken(token) {
		denied()
		return
	}
	var grant db.SimpleFINConnection
	now := time.Now().UTC()
	if err := s.db.Where("claim_hash = ? AND claimed_at IS NULL AND revoked_at IS NULL AND expires_at > ?", secretHash(token), now).First(&grant).Error; err != nil {
		denied()
		return
	}
	password, err := newAuthCSRFToken()
	if err != nil {
		http.Error(w, "Token unavailable", 503)
		return
	}
	result := s.db.Model(&db.SimpleFINConnection{}).Where("id = ? AND claimed_at IS NULL AND revoked_at IS NULL AND expires_at > ?", grant.ID, now).Updates(map[string]any{"claimed_at": now, "access_hash": secretHash(password)})
	if result.Error != nil || result.RowsAffected != 1 {
		denied()
		return
	}
	access, _ := url.Parse(grant.BaseURL + "/simplefin/access")
	access.User = url.UserPassword(grant.ID, password)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, access.String())
}
func (s *Server) readSimpleFIN(w http.ResponseWriter, r *http.Request) {
	id, password, ok := r.BasicAuth()
	var grant db.SimpleFINConnection
	if !ok || !validAuthCSRFToken(id) || !validAuthCSRFToken(password) || s.db.Where("id = ? AND claimed_at IS NOT NULL AND revoked_at IS NULL", id).First(&grant).Error != nil || subtle.ConstantTimeCompare([]byte(grant.AccessHash), []byte(secretHash(password))) != 1 {
		http.Error(w, "Connection unavailable or revoked", 403)
		return
	}
	if s.scheduler.IsRunning() {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "Degitide sync in progress; retry shortly", 503)
		return
	}
	now := time.Now().UTC()
	window, err := simplefin.ParseWindow(r.URL.Query(), now)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	feed, err := simplefin.Build(s.db, grant, window, now)
	if err != nil {
		writeError(w, 503, errors.New("saved portfolio unavailable"))
		return
	}
	if err := s.db.Model(&db.SimpleFINConnection{}).Where("id = ? AND revoked_at IS NULL", id).Update("last_used_at", now).Error; err != nil {
		writeError(w, 503, errors.New("connection state unavailable"))
		return
	}
	writeJSON(w, feed)
}
