// Package cloudflare is the little of Cloudflare's API bedrock needs: find
// a zone, list, make, change and remove DNS records in it, and read the
// address ranges Cloudflare's proxy connects from.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBase is Cloudflare's API.
const DefaultBase = "https://api.cloudflare.com/client/v4"

// Client talks to the API with one token.
type Client struct {
	Token string
	Base  string
	http  *http.Client
}

// New returns a client for a token.
func New(token string) *Client {
	return &Client{Token: token, Base: DefaultBase, http: &http.Client{Timeout: 30 * time.Second}}
}

// Zone is a DNS zone the token can see.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Record is one DNS record.
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ErrNoZone means no zone the token can see holds a name.
var ErrNoZone = errors.New("no zone")

type envelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (*envelope, error) {
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	base := c.Base
	if base == "" {
		base = DefaultBase
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("cloudflare answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		if len(msgs) == 0 {
			msgs = append(msgs, resp.Status)
		}
		return nil, fmt.Errorf("cloudflare: %s", strings.Join(msgs, "; "))
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, fmt.Errorf("cloudflare: %w", err)
		}
	}
	return &env, nil
}

// Zones lists the zones the token can see.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	var all []Zone
	for page := 1; ; page++ {
		var zones []Zone
		env, err := c.do(ctx, http.MethodGet, "/zones?per_page=50&page="+strconv.Itoa(page), nil, &zones)
		if err != nil {
			return nil, err
		}
		all = append(all, zones...)
		if env.ResultInfo.TotalPages == 0 || page >= env.ResultInfo.TotalPages {
			return all, nil
		}
	}
}

// ZoneFor finds the zone that holds a host: the longest zone name the
// host ends with.
func (c *Client) ZoneFor(ctx context.Context, host string) (Zone, error) {
	zones, err := c.Zones(ctx)
	if err != nil {
		return Zone{}, err
	}
	var best Zone
	for _, z := range zones {
		if (host == z.Name || strings.HasSuffix(host, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	if best.ID == "" {
		return Zone{}, fmt.Errorf("%w for %s among %d zone(s) the token can see", ErrNoZone, host, len(zones))
	}
	return best, nil
}

// Records lists a zone's records, all of them when name is empty.
func (c *Client) Records(ctx context.Context, zoneID, name string) ([]Record, error) {
	var all []Record
	for page := 1; ; page++ {
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if name != "" {
			q.Set("name", name)
		}
		var records []Record
		env, err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil, &records)
		if err != nil {
			return nil, err
		}
		all = append(all, records...)
		if env.ResultInfo.TotalPages == 0 || page >= env.ResultInfo.TotalPages {
			return all, nil
		}
	}
}

// Create makes a record and returns it with its id.
func (c *Client) Create(ctx context.Context, zoneID string, r Record) (Record, error) {
	if r.TTL == 0 {
		r.TTL = 1 // automatic
	}
	var made Record
	_, err := c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", r, &made)
	return made, err
}

// Update changes a record's content, proxied flag and comment.
func (c *Client) Update(ctx context.Context, zoneID string, r Record) error {
	body := map[string]any{"type": r.Type, "name": r.Name, "content": r.Content, "proxied": r.Proxied, "comment": r.Comment}
	if r.TTL != 0 {
		body["ttl"] = r.TTL
	}
	_, err := c.do(ctx, http.MethodPatch, "/zones/"+zoneID+"/dns_records/"+r.ID, body, nil)
	return err
}

// Delete removes a record.
func (c *Client) Delete(ctx context.Context, zoneID, recordID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+recordID, nil, nil)
	return err
}

// Verify checks the token works by listing zones.
func (c *Client) Verify(ctx context.Context) ([]string, error) {
	zones, err := c.Zones(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		names = append(names, z.Name)
	}
	return names, nil
}

// Ranges are the addresses Cloudflare's proxy connects to an origin from.
type Ranges struct {
	IPv4 []string `json:"ipv4_cidrs"`
	IPv6 []string `json:"ipv6_cidrs"`
}

// All returns every range, IPv4 first.
func (r Ranges) All() []string {
	return append(append([]string(nil), r.IPv4...), r.IPv6...)
}

// RangesFile is where a machine keeps the ranges bedrock last read: host
// setup writes it, and the edge trusts what it lists to say who a visitor
// behind the proxy is.
const RangesFile = "/etc/bedrock/cloudflare-ranges.json"

// ParseRanges reads a kept list, refusing one that fails Check.
func ParseRanges(b []byte) (Ranges, error) {
	var r Ranges
	if err := json.Unmarshal(b, &r); err != nil {
		return Ranges{}, err
	}
	if err := r.Check(); err != nil {
		return Ranges{}, err
	}
	return r, nil
}

// Check refuses a list that would give far more than Cloudflare what is
// given to Cloudflare, whoever served it: something other than address
// ranges, or a range as wide as a continent. Cloudflare's are /12 to /22
// and /29 to /32.
func (r Ranges) Check() error {
	if len(r.IPv4) == 0 {
		return errors.New("the list has no IPv4 ranges")
	}
	for _, list := range []struct {
		ranges []string
		v4     bool
		widest int
	}{{r.IPv4, true, 8}, {r.IPv6, false, 16}} {
		for _, s := range list.ranges {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return fmt.Errorf("%q is not an address range", s)
			}
			if p.Addr().Is4() != list.v4 || p != p.Masked() || p.Bits() < list.widest {
				return fmt.Errorf("%q is not a range bedrock will trust as Cloudflare's", s)
			}
		}
	}
	return nil
}

// IPs reads Cloudflare's published ranges. The list is public: it needs
// no token, so a client made with New("") can ask.
func (c *Client) IPs(ctx context.Context) (Ranges, error) {
	var r Ranges
	if _, err := c.do(ctx, http.MethodGet, "/ips", nil, &r); err != nil {
		return Ranges{}, err
	}
	if err := r.Check(); err != nil {
		return Ranges{}, fmt.Errorf("cloudflare: %w", err)
	}
	return r, nil
}
