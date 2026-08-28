package dns

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okClient(check func(*http.Request)) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		check(r)
		return &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"success":true,"errors":[],"result":[]}`)),
		}, nil
	})}
}

func TestAPITokenAuthHeader(t *testing.T) {
	p := config.Provider{
		Type: "cloudflare", ZoneID: "zone", AuthMode: "api_token", APIToken: "abc",
	}
	c := CloudflareClient{HTTP: okClient(func(r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer abc" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Auth-Email"); got != "" {
			t.Fatalf("unexpected email header = %q", got)
		}
	})}
	if err := c.TestProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalAPIKeyHeaders(t *testing.T) {
	p := config.Provider{
		Type: "cloudflare", ZoneID: "zone", AuthMode: "global_api_key",
		Email: "me@example.com", APIKey: "secret",
	}
	c := CloudflareClient{HTTP: okClient(func(r *http.Request) {
		if got := r.Header.Get("X-Auth-Email"); got != "me@example.com" {
			t.Fatalf("X-Auth-Email = %q", got)
		}
		if got := r.Header.Get("X-Auth-Key"); got != "secret" {
			t.Fatalf("X-Auth-Key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("unexpected Authorization = %q", got)
		}
	})}
	if err := c.TestProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}


func TestPlanRecordReconcileAvoidsIdenticalRecordCollision(t *testing.T) {
	records := []cfRecord{
		{ID: "r1", Type: "A", Name: "v4.example.com", Content: "1.1.1.1"},
		{ID: "r2", Type: "A", Name: "v4.example.com", Content: "2.2.2.2"},
		{ID: "r3", Type: "A", Name: "v4.example.com", Content: "3.3.3.3"},
		{ID: "r4", Type: "A", Name: "v4.example.com", Content: "4.4.4.4"},
		{ID: "r5", Type: "A", Name: "v4.example.com", Content: "5.5.5.5"},
	}
	want := []string{"2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5", "6.6.6.6"}

	p := planRecordReconcile(records, want)

	if len(p.Keep) != 4 {
		t.Fatalf("keep=%d want 4: %#v", len(p.Keep), p.Keep)
	}
	if len(p.Extras) != 1 || p.Extras[0].Content != "1.1.1.1" {
		t.Fatalf("extras=%#v want only 1.1.1.1", p.Extras)
	}
	if len(p.Missing) != 1 || p.Missing[0] != "6.6.6.6" {
		t.Fatalf("missing=%#v want only 6.6.6.6", p.Missing)
	}

	// Crucially, 2.2.2.2 is kept rather than targeted by a PUT from r1,
	// which is exactly the old 81058 collision.
	for _, m := range p.Missing {
		if m == "2.2.2.2" {
			t.Fatalf("existing desired record incorrectly marked missing")
		}
	}
}

func TestPlanRecordReconcileHandlesDuplicateExistingRecord(t *testing.T) {
	records := []cfRecord{
		{ID: "a", Content: "1.1.1.1"},
		{ID: "b", Content: "1.1.1.1"},
		{ID: "c", Content: "2.2.2.2"},
	}
	want := []string{"1.1.1.1", "3.3.3.3"}
	p := planRecordReconcile(records, want)
	if len(p.Keep) != 1 || p.Keep[0].Content != "1.1.1.1" {
		t.Fatalf("unexpected keep: %#v", p.Keep)
	}
	if len(p.Missing) != 1 || p.Missing[0] != "3.3.3.3" {
		t.Fatalf("unexpected missing: %#v", p.Missing)
	}
	if len(p.Extras) != 2 {
		t.Fatalf("unexpected extras: %#v", p.Extras)
	}
}
