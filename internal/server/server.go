package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/scheduler"
)

//go:embed static
var webFiles embed.FS

type Server struct {
	db        *db.DB
	worker    *browser.Client
	scheduler PullScheduler
	mux       *http.ServeMux
	options   Options
	loginWork chan struct{}
}

type Options struct {
	APIToken     string
	AllowedHosts string
	Version      string
}

type PullScheduler interface {
	TriggerNow() error
	IsRunning() bool
	LastError() string
}

func New(database *db.DB, worker *browser.Client, scheduler PullScheduler, configured ...Options) *Server {
	options := Options{AllowedHosts: "127.0.0.1,localhost,[::1]"}
	if len(configured) > 0 {
		options = configured[0]
		if options.AllowedHosts == "" {
			options.AllowedHosts = "127.0.0.1,localhost,[::1]"
		}
	}
	s := &Server{
		db:        database,
		worker:    worker,
		scheduler: scheduler,
		mux:       http.NewServeMux(),
		options:   options,
		loginWork: make(chan struct{}, 1),
	}
	s.routes()
	return s
}

func (s *Server) Router() http.Handler {
	return securityHeaders(
		guardRequests(
			s.simpleFINRouter(authenticateDashboard(s.mux, s.db, s.options.APIToken)),
			s.options.AllowedHosts,
		),
	)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /setup", s.handleSetupPage)
	s.mux.HandleFunc("POST /setup", s.handleSetup)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	// API
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/sync/events", s.handleSyncEvents)
	s.mux.HandleFunc("GET /api/schedule", s.handleSchedule)
	s.mux.HandleFunc("PUT /api/schedule", s.handleSchedule)
	s.mux.HandleFunc("DELETE /api/schedule", s.handleSchedule)
	s.mux.HandleFunc("GET /api/versions", s.handleVersions)
	s.mux.HandleFunc("GET /api/accounts", s.handleAccounts)
	s.mux.HandleFunc("POST /api/accounts", s.handleCreateAccount)
	s.mux.HandleFunc("POST /api/pull/trigger", s.handlePullTrigger)
	s.mux.HandleFunc("POST /api/degiro/reauthenticate", s.handleDegiroReauthenticate)
	s.mux.HandleFunc("GET /api/transactions", s.handleTransactions)
	s.mux.HandleFunc("GET /api/transactions/export", s.handleTransactionExport)
	s.mux.HandleFunc("GET /api/holdings", s.handleHoldings)
	s.mux.HandleFunc("GET /api/portfolio/history", s.handlePortfolioHistory)
	s.mux.HandleFunc("GET /api/portfolio/valuation", s.handlePortfolioValuation)
	s.mux.HandleFunc("GET /api/portfolio/snapshot", s.handlePortfolioSnapshot)
	s.mux.HandleFunc("GET /api/portfolio/returns", s.handlePriceReturns)
	s.mux.HandleFunc("GET /api/integrations/sure", s.listSimpleFIN)
	s.mux.HandleFunc("POST /api/integrations/sure", s.createSimpleFIN)
	s.mux.HandleFunc("DELETE /api/integrations/sure/{id}", s.revokeSimpleFIN)

	// Embedded web UI
	staticFS, _ := fs.Sub(webFiles, "static")
	s.mux.Handle("GET /", http.FileServer(http.FS(staticFS)))
}

// --- Handlers ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	pullRunning := s.scheduler.IsRunning()
	var count int64
	s.db.Model(&db.Transaction{}).Where("account_id = ?", account.ID).Count(&count)

	resp := map[string]any{
		"session_valid":         account.Connected,
		"last_pull":             account.LastPull,
		"tx_count":              count,
		"pull_running":          pullRunning,
		"pull_error":            s.scheduler.LastError(),
		"status_error":          account.LastError,
		"session_check_skipped": true,
		"account":               account,
	}
	if reporter, ok := s.scheduler.(interface{ Progress() scheduler.Progress }); ok {
		resp["progress"] = reporter.Progress()
	}
	if planner, ok := s.scheduler.(interface {
		ScheduleStatus(db.Account, time.Time) scheduler.ScheduleStatus
	}); ok {
		resp["schedule"] = planner.ScheduleStatus(account, time.Now())
	}
	writeJSON(w, resp)
}

func (s *Server) handlePullTrigger(w http.ResponseWriter, r *http.Request) {
	if len(s.loginWork) != 0 {
		writeError(w, http.StatusConflict, errors.New("wait for DEGIRO sign-in to finish before pulling"))
		return
	}
	var err error
	if r.URL.Query().Get("account") != "" {
		account, ok := s.requestAccount(w, r)
		if !ok {
			return
		}
		if runner, ok := s.scheduler.(interface{ TriggerAccounts([]string) error }); ok {
			err = runner.TriggerAccounts([]string{account.ID})
		} else {
			err = s.scheduler.TriggerNow()
		}
	} else {
		err = s.scheduler.TriggerNow()
	}
	if err != nil {
		if errors.Is(err, scheduler.ErrPullInProgress) {
			writeJSONStatus(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "sync_in_progress"})
			return
		}
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"status": "pull started"})
}

func (s *Server) handleDegiroReauthenticate(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	if !credentialTransportAllowed(r) {
		writeError(w, http.StatusUpgradeRequired, errors.New("HTTPS is required for DEGIRO sign-in"))
		return
	}
	if s.scheduler.IsRunning() {
		writeError(w, http.StatusConflict, errors.New("wait for the current pull to finish before signing in"))
		return
	}
	select {
	case s.loginWork <- struct{}{}:
		defer func() { <-s.loginWork }()
	default:
		writeError(w, http.StatusConflict, errors.New("a DEGIRO sign-in is already in progress"))
		return
	}

	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
		Seed     string `json:"seed"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("invalid sign-in request"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, errors.New("invalid sign-in request"))
		return
	}
	if len(request.Username) < 1 || len(request.Username) > 256 ||
		len(request.Password) < 1 || len(request.Password) > 512 ||
		len(request.Seed) > 2048 || (request.Seed == "" && (len(request.TOTP) != 6 || strings.Trim(request.TOTP, "0123456789") != "")) {
		writeError(w, http.StatusBadRequest, errors.New("enter a username, password, and six-digit verification code"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 210*time.Second)
	defer cancel()
	result, err := s.worker.ForAccount(account.ID).Reauthenticate(
		ctx, request.Username, request.Password, request.TOTP, request.Seed,
	)
	request.Username, request.Password, request.TOTP, request.Seed = "", "", "", ""
	if err != nil {
		log.Printf("DEGIRO web reauthentication failed: %v", err)
		if strings.Contains(err.Error(), "use a new account") {
			writeError(w, http.StatusConflict, errors.New("add a separate account for a different DEGIRO username"))
			return
		}
		if strings.Contains(err.Error(), `"error": "invalid authenticator secret"`) {
			writeError(w, http.StatusBadRequest, errors.New("invalid authenticator setup secret: enter the base32 setup key or an otpauth:// export using SHA-1, six digits, and a 30-second period; a six-digit code is not a setup secret"))
			return
		}
		writeError(w, http.StatusUnprocessableEntity, errors.New("DEGIRO sign-in did not complete; check your username, password, and authenticator setup secret"))
		return
	}
	if result == nil || !result.Authenticated || !result.SessionSaved {
		log.Printf("DEGIRO web reauthentication returned an incomplete result")
		writeError(w, http.StatusBadGateway, errors.New("DEGIRO sign-in did not produce a restart-safe session"))
		return
	}
	if err := s.db.Model(&db.Account{}).Where("id = ?", account.ID).Updates(map[string]any{"connected": true, "last_error": ""}).Error; err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]string{"status": "connected", "account_id": account.ID})
}

func (s *Server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	var txs []db.Transaction
	if err := s.db.Where("account_id = ?", account.ID).Order("date DESC").Limit(100).Find(&txs).Error; err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, txs)
}

func (s *Server) handleHoldings(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	var latest db.Holding
	result := s.db.Where("account_id = ?", account.ID).Order("snapshot_date DESC").Limit(1).Find(&latest)
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		writeJSON(w, []db.Holding{})
		return
	}
	var holdings []db.Holding
	snapshot := latest.SnapshotDate
	if account.SnapshotDate != nil {
		snapshot = *account.SnapshotDate
	}
	if err := s.db.Where("snapshot_date = ? AND account_id = ?", snapshot, account.ID).Order("symbol ASC").Find(&holdings).Error; err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, holdings)
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSONStatus(w, status, map[string]string{"error": err.Error()})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; object-src 'none'; form-action 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if (len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/") || isPublicAuthAsset(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func requestIsSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func guardRequests(next http.Handler, allowedHostList string) http.Handler {
	allowedHosts := make(map[string]struct{})
	for _, host := range strings.Split(allowedHostList, ",") {
		if normalized := normalizeHostname(host); normalized != "" {
			allowedHosts[normalized] = struct{}{}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, allowed := allowedHosts[normalizeHostname(r.Host)]; !allowed {
			writeError(w, http.StatusMisdirectedRequest, http.ErrNotSupported)
			return
		}
		if site := strings.ToLower(r.Header.Get("Sec-Fetch-Site")); site == "cross-site" {
			if isAuthFormRequest(r) {
				rejectRequestOrigin(w, r, "cross_site_metadata")
				return
			}
			writeError(w, http.StatusForbidden, errors.New("cross-site request denied"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Sure claims a one-time capability server-to-server, without browser
			// Origin headers. This exception grants no dashboard access.
			machineClaim := r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/simplefin/claim/") && r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") == ""
			if !requestHasSameOrigin(r) && !authFormMayUseCSRF(r) && !machineClaim {
				rejectRequestOrigin(w, r, "origin_mismatch")
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") &&
				!strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, http.ErrNotSupported)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func authFormMayUseCSRF(r *http.Request) bool {
	if !isAuthFormRequest(r) || !strings.HasPrefix(
		strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))),
		"application/x-www-form-urlencoded",
	) {
		return false
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if origin == "" {
		// Preserve compatibility with clients and trusted proxies which omit
		// both headers. If Fetch Metadata is present, require same-origin.
		return fetchSite == "" || fetchSite == "same-origin"
	}

	// Navigation-mode form POSTs rendered under Referrer-Policy: no-referrer
	// serialize their Origin as "null". Accept that opaque value only when
	// browser-controlled Fetch Metadata says the request is same-origin; the
	// handler still requires the matching double-submit CSRF token.
	return strings.EqualFold(origin, "null") && fetchSite == "same-origin"
}

func isAuthFormRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && (r.URL.Path == "/setup" || r.URL.Path == "/login")
}

func requestHasSameOrigin(r *http.Request) bool {
	originValue := strings.TrimSpace(r.Header.Get("Origin"))
	if originValue == "" {
		// A cross-site browser form cannot opt into this Fetch Metadata value.
		// This fallback keeps form POSTs working through reverse proxies which
		// omit Origin while retaining browser CSRF protection.
		return strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "same-origin")
	}

	origin, err := url.Parse(originValue)
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil ||
		origin.Opaque != "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	originHost, originPort, ok := canonicalOriginHost(origin.Scheme, origin.Host)
	if !ok {
		return false
	}
	requestHost, requestPort, ok := canonicalOriginHost(requestScheme(r), r.Host)
	return ok && strings.EqualFold(origin.Scheme, requestScheme(r)) &&
		originHost == requestHost && originPort == requestPort
}

func canonicalOriginHost(scheme, hostport string) (string, string, bool) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	u, err := url.Parse(scheme + "://" + strings.TrimSpace(hostport))
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", "", false
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", "", false
		}
		port = strconv.Itoa(value)
	}
	return host, port, true
}

func rejectRequestOrigin(w http.ResponseWriter, r *http.Request, reason string) {
	switch r.URL.Path {
	case "/setup":
		logAuthFormRejection(r, reason)
		http.Redirect(w, r, "/setup?error=origin", http.StatusSeeOther)
	case "/login":
		logAuthFormRejection(r, reason)
		http.Redirect(w, r, "/login?error=origin", http.StatusSeeOther)
	default:
		writeError(w, http.StatusForbidden, errors.New("same-origin request required"))
	}
}

func requestScheme(r *http.Request) string {
	if requestIsSecure(r) {
		return "https"
	}
	return "http"
}

func normalizeHostname(hostport string) string {
	hostport = strings.TrimSpace(strings.ToLower(hostport))
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(strings.TrimSuffix(hostport, "."), "[]")
}

func (s *Server) handlePortfolioHistory(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	// Synced snapshots plus value reconstructed from DEGIRO position reports.
	points, err := s.db.ValueHistory(account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, points)
}

func (s *Server) handlePriceReturns(w http.ResponseWriter, r *http.Request) {
	account, ok := s.requestAccount(w, r)
	if !ok {
		return
	}
	// Price returns per security from the weekly DEGIRO closes.
	returns, err := s.db.PriceReturns(account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, returns)
}
