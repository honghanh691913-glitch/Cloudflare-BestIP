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
