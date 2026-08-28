package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/engine"
)

type CloudflareClient struct{ HTTP *http.Client }

type cfResponse[T any] struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
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


// ListTargetRecords reads the records currently published for a target.
// Startup recovery uses this instead of assuming an empty in-memory engine
// means the active DNS IPs are invalid.
func (c CloudflareClient) ListTargetRecords(ctx context.Context, p config.Provider, t config.Target) (map[string][]string, error) {
	if p.Type != "cloudflare" {
		return nil, fmt.Errorf("unsupported provider type: %s", p.Type)
	}
	if strings.TrimSpace(p.ZoneID) == "" {
		return nil, fmt.Errorf("provider %s missing zone_id", p.ID)
	}
	hostname := config.TargetHostname(p, t)
	if hostname == "" {
		return nil, fmt.Errorf("target %s has no resolvable hostname", t.ID)
	}

	out := map[string][]string{"A": {}, "AAAA": {}}
	base := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", p.ZoneID)
	for _, typ := range []string{"A", "AAAA"} {
		q := url.Values{}
		q.Set("type", typ)
		q.Set("name", hostname)
		q.Set("per_page", "100")
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
		applyCloudflareAuth(req, p)
		resp, err := client(c.HTTP).Do(req)
		if err != nil {
			return nil, err
		}
		var got cfResponse[[]cfRecord]
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if !got.Success {
			return nil, cloudflareProviderError(p, cfErr(got.Errors))
		}
		for _, rec := range got.Result {
			ip := strings.TrimSpace(rec.Content)
			if ip == "" {
				continue
			}
			if typ == "A" && net.ParseIP(ip) != nil && net.ParseIP(ip).To4() != nil {
				out[typ] = append(out[typ], ip)
			}
			if typ == "AAAA" && net.ParseIP(ip) != nil && net.ParseIP(ip).To4() == nil {
				out[typ] = append(out[typ], ip)
			}
		}
		out[typ] = unique(out[typ])
		sort.Strings(out[typ])
	}
	return out, nil
}

func (c CloudflareClient) SyncTarget(ctx context.Context, p config.Provider, t config.Target, latest map[string][]engine.Result) error {
	if p.Type != "cloudflare" {
		return fmt.Errorf("unsupported provider type: %s", p.Type)
	}
	if strings.TrimSpace(p.ZoneID) == "" {
		return fmt.Errorf("provider %s missing zone_id", p.ID)
	}
	switch config.CloudflareAuthMode(p) {
	case "global_api_key":
		if strings.TrimSpace(p.Email) == "" || strings.TrimSpace(p.APIKey) == "" {
			return fmt.Errorf("provider %s missing email/api_key for Global API Key authentication", p.ID)
		}
	default:
		if strings.TrimSpace(p.APIToken) == "" {
			return fmt.Errorf("provider %s missing api_token", p.ID)
		}
	}
	desired := map[string][]string{"A": {}, "AAAA": {}}
	for _, ref := range t.Sources {
		rs := latest[ref.SourceID]
		// Strict target semantics: if a source is configured to contribute N records,
		// do not shrink DNS to fewer records after a bad/partial test run.
		// The caller gets an error and the existing DNS set is left untouched.
		if len(rs) < ref.Count {
			return fmt.Errorf("source %s only has %d ready results; target requires %d", ref.SourceID, len(rs), ref.Count)
		}
		for _, r := range rs[:ref.Count] {
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


type reconcilePlan struct {
	Keep    []cfRecord
	Extras  []cfRecord
	Missing []string
}

func planRecordReconcile(records []cfRecord, want []string) reconcilePlan {
	want = unique(want)
	sort.Strings(want)

	wanted := map[string]bool{}
	for _, ip := range want {
		wanted[ip] = true
	}
	keptSet := map[string]bool{}
	plan := reconcilePlan{}
	for _, rec := range records {
		ip := strings.TrimSpace(rec.Content)
		if wanted[ip] && !keptSet[ip] {
			keptSet[ip] = true
			plan.Keep = append(plan.Keep, rec)
			continue
		}
		plan.Extras = append(plan.Extras, rec)
	}
	for _, ip := range want {
		if !keptSet[ip] {
			plan.Missing = append(plan.Missing, ip)
		}
	}
	return plan
}

func (c CloudflareClient) syncType(ctx context.Context, p config.Provider, t config.Target, typ string, want []string) error {
	hostname := config.TargetHostname(p, t)
	if hostname == "" {
		return fmt.Errorf("target %s has no resolvable hostname", t.ID)
	}
	base := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", p.ZoneID)
	q := url.Values{}
	q.Set("type", typ)
	q.Set("name", hostname)
	q.Set("per_page", "100")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	applyCloudflareAuth(req, p)
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
		return cloudflareProviderError(p, cfErr(got.Errors))
	}

	// Reconcile by record content rather than by list position.
	//
	// Slot-by-slot PUT is unsafe with Cloudflare. Example:
	// existing = [1,2,3,4,5], desired = [2,3,4,5,6].
	// Updating the first record 1 -> 2 collides with the already-existing "2"
	// and Cloudflare returns 81058 "An identical record already exists".
	//
	// Instead:
	//   1) keep records whose content is still desired;
	//   2) reuse genuinely-extra records for genuinely-missing contents;
	//   3) delete remaining extras;
	//   4) create remaining missing records.
	plan := planRecordReconcile(got.Result, want)
	extras := plan.Extras
	missing := plan.Missing

	// Reuse extras first. Every item in missing is guaranteed not to already
	// exist among the kept records, so these PUTs cannot trigger 81058 merely
	// because another desired record already has the same content.
	reuse := len(extras)
	if len(missing) < reuse {
		reuse = len(missing)
	}
	for i := 0; i < reuse; i++ {
		if err := c.write(ctx, p, t, typ, extras[i].ID, missing[i], http.MethodPut); err != nil {
			return fmt.Errorf("reconcile %s %s: %w", typ, hostname, err)
		}
	}

	// Delete extras that were not reused.
	for i := reuse; i < len(extras); i++ {
		u := base + "/" + extras[i].ID
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
		applyCloudflareAuth(req, p)
		resp, err := client(c.HTTP).Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("delete %s %s: %s", typ, hostname, resp.Status)
		}
	}

	// Create missing records that had no extra slot available.
	for i := reuse; i < len(missing); i++ {
		if err := c.write(ctx, p, t, typ, "", missing[i], http.MethodPost); err != nil {
			return fmt.Errorf("create %s %s %s: %w", typ, hostname, missing[i], err)
		}
	}
	return nil
}

func (c CloudflareClient) write(ctx context.Context, p config.Provider, t config.Target, typ, id, ip, method string) error {
	hostname := config.TargetHostname(p, t)
	if hostname == "" {
		return fmt.Errorf("target %s has no resolvable hostname", t.ID)
	}
	base := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", p.ZoneID)
	if id != "" {
		base += "/" + id
	}
	ttl := t.TTL
	if ttl == 0 {
		ttl = 60
	}
	body, _ := json.Marshal(map[string]any{"type": typ, "name": hostname, "content": ip, "ttl": ttl, "proxied": t.Proxied})
	req, _ := http.NewRequestWithContext(ctx, method, base, bytes.NewReader(body))
	applyCloudflareAuth(req, p)
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
		return cloudflareProviderError(p, cfErr(out.Errors))
	}
	return nil
}
func applyCloudflareAuth(req *http.Request, p config.Provider) {
	if config.CloudflareAuthMode(p) == "global_api_key" {
		req.Header.Set("X-Auth-Email", strings.TrimSpace(p.Email))
		req.Header.Set("X-Auth-Key", strings.TrimSpace(p.APIKey))
		return
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(p.APIToken))
}

func cloudflareProviderError(p config.Provider, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "authentication") || strings.Contains(msg, "10000") || strings.Contains(msg, "9109") {
		if config.CloudflareAuthMode(p) == "global_api_key" {
			return fmt.Errorf("Cloudflare 认证失败：当前使用 Email + Global API Key，请检查账号邮箱、Global API Key 与 Zone ID 是否匹配。原错误：%w", err)
		}
		return fmt.Errorf("Cloudflare 认证失败：当前使用 API Token（无需邮箱）。请检查 Token 是否有效并至少拥有 Zone:Read 与 DNS:Edit 权限；如果你填写的是 Global API Key，请在 DNS 账号里切换为“Email + Global API Key”。原错误：%w", err)
	}
	return err
}

func (c CloudflareClient) TestProvider(ctx context.Context, p config.Provider) error {
	if p.Type != "cloudflare" {
		return fmt.Errorf("unsupported provider type: %s", p.Type)
	}
	if strings.TrimSpace(p.ZoneID) == "" {
		return fmt.Errorf("Zone ID 不能为空")
	}
	base := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?per_page=1", p.ZoneID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	applyCloudflareAuth(req, p)
	resp, err := client(c.HTTP).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var got cfResponse[[]cfRecord]
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return fmt.Errorf("Cloudflare 返回不可解析响应：%w", err)
	}
	if !got.Success {
		return cloudflareProviderError(p, cfErr(got.Errors))
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
	Code    int    `json:"code"`
	Message string `json:"message"`
}) error {
	msgs := []string{}
	for _, e := range es {
		if e.Code != 0 {
			msgs = append(msgs, fmt.Sprintf("%d: %s", e.Code, e.Message))
		} else {
			msgs = append(msgs, e.Message)
		}
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
