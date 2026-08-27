package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/yourname/bestip-manager/internal/config"
	"github.com/yourname/bestip-manager/internal/engine"
)

type CloudflareClient struct{ HTTP *http.Client }

type cfResponse[T any] struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result T `json:"result"`
}
type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (c CloudflareClient) SyncTarget(ctx context.Context, p config.Provider, t config.Target, latest map[string][]engine.Result) error {
	if p.Type != "cloudflare" {
		return fmt.Errorf("unsupported provider type: %s", p.Type)
	}
	if p.ZoneID == "" || p.APIToken == "" {
		return fmt.Errorf("provider %s missing zone_id/api_token", p.ID)
	}
	desired := map[string][]string{"A": {}, "AAAA": {}}
	for _, ref := range t.Sources {
		rs := latest[ref.SourceID]
		n := ref.Count
		if n > len(rs) {
			n = len(rs)
		}
		for _, r := range rs[:n] {
			if r.Family == "ipv4" {
				desired["A"] = append(desired["A"], r.IP)
			} else {
				desired["AAAA"] = append(desired["AAAA"], r.IP)
			}
		}
	}
	desired["A"] = unique(desired["A"])
	desired["AAAA"] = unique(desired["AAAA"])
	for _, typ := range []string{"A", "AAAA"} {
		if err := c.syncType(ctx, p, t, typ, desired[typ]); err != nil {
			return err
		}
	}
	return nil
}

func (c CloudflareClient) syncType(ctx context.Context, p config.Provider, t config.Target, typ string, want []string) error {
	base := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", p.ZoneID)
	q := url.Values{}
	q.Set("type", typ)
	q.Set("name", t.Hostname)
	q.Set("per_page", "100")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+p.APIToken)
	resp, err := client(c.HTTP).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var got cfResponse[[]cfRecord]
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return err
	}
	if !got.Success {
		return cfErr(got.Errors)
	}

	sort.Strings(want)
	records := got.Result
	// Reuse existing records by slot, then delete extras and create missing.
	common := len(records)
	if len(want) < common {
		common = len(want)
	}
	for i := 0; i < common; i++ {
		if records[i].Content == want[i] {
			continue
		}
		if err := c.write(ctx, p, t, typ, records[i].ID, want[i], http.MethodPut); err != nil {
			return err
		}
	}
	for i := common; i < len(want); i++ {
		if err := c.write(ctx, p, t, typ, "", want[i], http.MethodPost); err != nil {
			return err
		}
	}
	for i := common; i < len(records); i++ {
		u := base + "/" + records[i].ID
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
		req.Header.Set("Authorization", "Bearer "+p.APIToken)
		resp, err := client(c.HTTP).Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("delete %s %s: %s", typ, t.Hostname, resp.Status)
		}
	}
	return nil
}

func (c CloudflareClient) write(ctx context.Context, p config.Provider, t config.Target, typ, id, ip, method string) error {
	base := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", p.ZoneID)
	if id != "" {
		base += "/" + id
	}
	ttl := t.TTL
	if ttl == 0 {
		ttl = 60
	}
	body, _ := json.Marshal(map[string]any{"type": typ, "name": t.Hostname, "content": ip, "ttl": ttl, "proxied": t.Proxied})
	req, _ := http.NewRequestWithContext(ctx, method, base, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+p.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client(c.HTTP).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out cfResponse[cfRecord]
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.Success {
		return cfErr(out.Errors)
	}
	return nil
}
func client(h *http.Client) *http.Client {
	if h != nil {
		return h
	}
	return http.DefaultClient
}
func cfErr(es []struct {
	Message string `json:"message"`
}) error {
	msgs := []string{}
	for _, e := range es {
		msgs = append(msgs, e.Message)
	}
	if len(msgs) == 0 {
		return fmt.Errorf("cloudflare api error")
	}
	return fmt.Errorf("cloudflare api: %s", strings.Join(msgs, "; "))
}
func unique(in []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, x := range in {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	return out
}
