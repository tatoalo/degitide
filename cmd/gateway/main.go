package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

func main() {
	targetURL := os.Getenv("DEGITIDE_GATEWAY_TARGET")
	if targetURL == "" {
		targetURL = "http://app:8391"
	}
	bind := os.Getenv("DEGITIDE_GATEWAY_BIND")
	if bind == "" {
		bind = "0.0.0.0:8391"
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		log.Fatalf("invalid gateway target: %v", err)
	}
	if target.Scheme != "http" || target.Host == "" {
		log.Fatal("gateway target must be an HTTP URL")
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		log.Printf("dashboard upstream unavailable: %v", err)
		http.Error(w, "dashboard unavailable", http.StatusBadGateway)
	}

	server := &http.Server{
		Addr:              bind,
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("ingress listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
