package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
	"github.com/tatoalo/degitide/internal/notify"
	"github.com/tatoalo/degitide/internal/scheduler"
	"github.com/tatoalo/degitide/internal/server"
)

type Config struct {
	Bind         string
	Port         string
	Worker       WorkerConfig
	Database     string
	Schedule     ScheduleConfig
	APIToken     string
	AllowedHosts string
}

type WorkerConfig struct {
	URL   string // browser worker endpoint, e.g. http://127.0.0.1:9377
	Token string
}

type ScheduleConfig struct {
	Daily           string // "market" (default), or an explicit cron expression
	IntervalMinutes int
	Maintenance     string
}

func main() {
	if handled, err := runCLI(os.Args[1:]); handled {
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	cfg := loadConfig()
	if !isLoopbackBind(cfg.Bind) && cfg.APIToken == "" {
		log.Fatal("DEGITIDE_API_TOKEN or DEGITIDE_API_TOKEN_FILE is required when binding outside loopback")
	}

	// Init database
	database, err := db.New(cfg.Database)
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer database.Close()

	// Browser worker client
	worker := browser.NewClient(cfg.Worker.URL, cfg.Worker.Token)

	// Scheduler
	sched := scheduler.New(database, worker, cfg.Schedule.Daily)
	if err := sched.ConfigureMarket(time.Duration(cfg.Schedule.IntervalMinutes)*time.Minute, cfg.Schedule.Maintenance); err != nil {
		log.Fatal(err)
	}
	if endpoint := os.Getenv("DEGITIDE_NOTIFY_URL"); endpoint != "" {
		token, err := notify.Secret("DEGITIDE_NOTIFY_TOKEN")
		if err != nil || token == "" {
			log.Fatal("notifications require DEGITIDE_NOTIFY_TOKEN or its _FILE")
		}
		notifications := notify.New(database, endpoint, token)
		defer notifications.Close()
		sched.OnResult = notifications.Report
	}
	sched.Start()

	// HTTP server
	app := server.New(database, worker, sched, server.Options{
		APIToken:     cfg.APIToken,
		AllowedHosts: cfg.AllowedHosts,
		Version:      version,
	})
	requestContext, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	httpServer := &http.Server{
		Addr:              net.JoinHostPort(cfg.Bind, cfg.Port),
		Handler:           app.Router(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestContext },
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	errCh := make(chan error, 1)
	go func() {
		log.Printf("degitide listening on http://%s", httpServer.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Release live event streams before waiting for HTTP handlers to finish.
	cancelRequests()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown: %v", err)
	}
	sched.Stop()
	log.Println("degitide shut down cleanly")
}

func isLoopbackBind(bind string) bool {
	if strings.EqualFold(bind, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(bind, "[]"))
	return ip != nil && ip.IsLoopback()
}

func loadConfig() Config {
	cfg := Config{
		Bind:         "127.0.0.1",
		Port:         "8391",
		Worker:       WorkerConfig{URL: "http://127.0.0.1:9377"},
		Database:     "./data/degitide.db",
		Schedule:     ScheduleConfig{Daily: "market", IntervalMinutes: 60, Maintenance: "CRON_TZ=Europe/Dublin 0 2 * * *"},
		AllowedHosts: "127.0.0.1,localhost,[::1]",
	}
	if bind := os.Getenv("DEGITIDE_BIND"); bind != "" {
		cfg.Bind = bind
	}
	if port := os.Getenv("DEGITIDE_PORT"); port != "" {
		cfg.Port = port
	}
	if workerURL := os.Getenv("DEGITIDE_WORKER_URL"); workerURL != "" {
		cfg.Worker.URL = workerURL
	}
	if tokenFile := os.Getenv("DEGITIDE_WORKER_TOKEN_FILE"); tokenFile != "" {
		cfg.Worker.Token = mustReadSecret(tokenFile, "worker token")
	} else if token := os.Getenv("DEGITIDE_WORKER_TOKEN"); token != "" {
		cfg.Worker.Token = token
	}
	if tokenFile := os.Getenv("DEGITIDE_API_TOKEN_FILE"); tokenFile != "" {
		cfg.APIToken = mustReadSecret(tokenFile, "dashboard token")
	} else if token := os.Getenv("DEGITIDE_API_TOKEN"); token != "" {
		cfg.APIToken = token
	}
	if allowedHosts := os.Getenv("DEGITIDE_ALLOWED_HOSTS"); allowedHosts != "" {
		cfg.AllowedHosts = allowedHosts
	}
	if dbPath := os.Getenv("DEGITIDE_DB"); dbPath != "" {
		cfg.Database = dbPath
	}
	if cron := os.Getenv("DEGITIDE_SCHEDULE"); cron != "" {
		cfg.Schedule.Daily = cron
	}
	if interval := os.Getenv("DEGITIDE_SYNC_INTERVAL_MINUTES"); interval != "" {
		minutes, err := strconv.Atoi(interval)
		if err != nil || minutes < 5 || minutes > 240 {
			log.Fatal("DEGITIDE_SYNC_INTERVAL_MINUTES must be between 5 and 240")
		}
		cfg.Schedule.IntervalMinutes = minutes
	}
	if maintenance := os.Getenv("DEGITIDE_MAINTENANCE_SCHEDULE"); maintenance != "" {
		cfg.Schedule.Maintenance = maintenance
	}
	return cfg
}

func mustReadSecret(path, label string) string {
	value, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", label, err)
	}
	secret := string(value)
	for len(secret) > 0 && (secret[len(secret)-1] == '\n' || secret[len(secret)-1] == '\r') {
		secret = secret[:len(secret)-1]
	}
	if secret == "" {
		log.Fatalf("%s is empty", label)
	}
	return secret
}
