// Package notify sends small, account-scoped failure/recovery events. The
// optional relay owns internet access; the app and browser stay isolated.
package notify

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/tatoalo/degitide/internal/db"
)

type Event struct {
	AccountID   string `json:"account_id"`
	AccountName string `json:"account_name"`
	Kind        string `json:"kind"`
}

type Dispatcher struct {
	database        *db.DB
	endpoint, token string
	client          *http.Client
	queue           chan Event
	done            chan struct{}
}

func New(database *db.DB, endpoint, token string) *Dispatcher {
	d := &Dispatcher{database: database, endpoint: endpoint, token: token, client: &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, queue: make(chan Event, 128), done: make(chan struct{})}
	go func() {
		defer close(d.done)
		for event := range d.queue {
			if err := d.deliver(event); err != nil {
				log.Printf("notifications: account %s: %v", event.AccountID, err)
			}
		}
	}()
	return d
}

func (d *Dispatcher) Report(account db.Account, err error) {
	if d.endpoint == "" {
		return
	}
	kind := "recovered"
	if err != nil {
		kind = "failed"
	}
	// Never enqueue worker errors, credentials, balances, or transaction details.
	select {
	case d.queue <- Event{AccountID: account.ID, AccountName: account.Name, Kind: kind}:
	default:
		log.Print("notifications: queue full; will retry on the next sync")
	}
}

func (d *Dispatcher) Close() { close(d.queue); <-d.done }

func (d *Dispatcher) deliver(event Event) error {
	key := "notification_state:" + event.AccountID
	previous, err := d.database.GetMeta(key)
	if err != nil {
		return err
	}
	if previous == event.Kind || (previous == "" && event.Kind == "recovered") {
		return nil
	}
	payload, _ := json.Marshal(event)
	r, requestErr := http.NewRequest("POST", d.endpoint, bytes.NewReader(payload))
	if requestErr != nil {
		return errors.New("invalid notification relay URL")
	}
	r.Header.Set("Authorization", "Bearer "+d.token)
	r.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(r)
	if err != nil {
		return errors.New("relay unavailable; will retry on the next sync")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("relay returned %d; will retry on the next sync", response.StatusCode)
	}
	return d.database.SetMeta(key, event.Kind)
}

type Provider struct {
	Kind, Token, Recipient string
	Client                 *http.Client
	Endpoint               string
}

func (p Provider) Send(ctx context.Context, event Event) error {
	message := "Degitide: " + event.AccountName + " — sync failed. Open the dashboard or run degitide status to check the connection."
	if event.Kind == "recovered" {
		message = "Degitide: " + event.AccountName + " — sync is working again."
	}
	form := url.Values{}
	endpoint := p.Endpoint
	switch p.Kind {
	case "telegram":
		if endpoint == "" {
			endpoint = "https://api.telegram.org/bot" + p.Token + "/sendMessage"
		}
		form.Set("chat_id", p.Recipient)
		form.Set("text", message)
	case "pushover":
		if endpoint == "" {
			endpoint = "https://api.pushover.net/1/messages.json"
		}
		form.Set("token", p.Token)
		form.Set("user", p.Recipient)
		form.Set("message", message)
		form.Set("title", "Degitide")
	default:
		return errors.New("unknown notification provider")
	}
	r, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("invalid notification endpoint")
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(r)
	if err != nil {
		return errors.New("notification provider unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("notification provider returned %d", response.StatusCode)
	}
	var result struct {
		OK     bool `json:"ok"`
		Status int  `json:"status"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result) != nil {
		return errors.New("invalid notification provider response")
	}
	if p.Kind == "telegram" && !result.OK || p.Kind == "pushover" && result.Status != 1 {
		return errors.New("notification provider rejected message")
	}
	return nil
}

func Handler(token string, provider Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /notify", func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		var event Event
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&event) != nil || decoder.Decode(&struct{}{}) != io.EOF || !db.ValidAccountID(event.AccountID) || len(event.AccountName) < 1 || len(event.AccountName) > 80 || strings.ContainsAny(event.AccountName, "\r\n\x00") || (event.Kind != "failed" && event.Kind != "recovered") {
			http.Error(w, "invalid event", 400)
			return
		}
		if err := provider.Send(r.Context(), event); err != nil {
			log.Printf("notifications: %v", err)
			http.Error(w, "delivery failed", 502)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func Secret(name string) (string, error) {
	if path := os.Getenv(name + "_FILE"); path != "" {
		value, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read %s file", name)
		}
		return strings.TrimSpace(string(value)), nil
	}
	return os.Getenv(name), nil
}

func Serve(ctx context.Context) error {
	token, err := Secret("DEGITIDE_NOTIFY_TOKEN")
	if err != nil {
		return err
	}
	if len(token) < 32 {
		return errors.New("notification relay requires a token of at least 32 characters")
	}
	kind := os.Getenv("DEGITIDE_NOTIFY_PROVIDER")
	provider := Provider{Kind: kind}
	switch kind {
	case "telegram":
		provider.Token, err = Secret("DEGITIDE_TELEGRAM_TOKEN")
		provider.Recipient = os.Getenv("DEGITIDE_TELEGRAM_CHAT_ID")
		if err != nil {
			return err
		}
		if !regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`).MatchString(provider.Token) {
			return errors.New("invalid Telegram bot token")
		}
	case "pushover":
		provider.Token, err = Secret("DEGITIDE_PUSHOVER_TOKEN")
		if err != nil {
			return err
		}
		provider.Recipient, err = Secret("DEGITIDE_PUSHOVER_USER")
		if err != nil {
			return err
		}
	default:
		return errors.New("DEGITIDE_NOTIFY_PROVIDER must be telegram or pushover")
	}
	if provider.Token == "" || provider.Recipient == "" {
		return errors.New("notification provider credentials are incomplete")
	}
	bind := os.Getenv("DEGITIDE_NOTIFY_BIND")
	if bind == "" {
		bind = ":9380"
	}
	server := &http.Server{Addr: bind, Handler: Handler(token, provider), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
