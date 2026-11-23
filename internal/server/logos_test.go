package server

import (
	"regexp"
	"strings"
	"testing"
)

// The dashboard CSP allows inline script, and an SVG opened directly runs as a
// document, so bundled logos must stay plain drawings (see static/logos).
func TestIssuerLogosAreInertDrawings(t *testing.T) {
	entries, err := webFiles.ReadDir("static/logos")
	if err != nil {
		t.Fatalf("read logos: %v", err)
	}
	if len(entries) < 4 {
		t.Fatalf("expected bundled issuer logos, found %d", len(entries))
	}
	unsafe := regexp.MustCompile(`(?i)<script|<!entity|<!doctype|\son[a-z]+\s*=|href|foreignobject|javascript:|<style|@import|<image|url\((?:[^#)]|$)`)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".svg") {
			t.Fatalf("unexpected logo file %s", name)
		}
		data, err := webFiles.ReadFile("static/logos/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) > 32*1024 || !strings.HasPrefix(string(data), "<svg ") {
			t.Fatalf("%s is not a small SVG drawing", name)
		}
		if match := unsafe.Find(data); match != nil {
			t.Fatalf("%s contains %q", name, match)
		}
	}
}
