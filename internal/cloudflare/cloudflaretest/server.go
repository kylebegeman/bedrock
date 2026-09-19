// Package cloudflaretest is a small stand-in for the part of Cloudflare's
// API quark uses: zones, and DNS records in them. The tests use it, and so
// does the lane, so no real token is needed to prove the record logic.
package cloudflaretest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record is a DNS record as the API returns it.
type Record struct {
	ID         string    `json:"id"`
	ZoneID     string    `json:"zone_id"`
	ZoneName   string    `json:"zone_name"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	Proxied    bool      `json:"proxied"`
	TTL        int       `json:"ttl"`
	Comment    string    `json:"comment,omitempty"`
	CreatedOn  time.Time `json:"created_on"`
	ModifiedOn time.Time `json:"modified_on"`
}

type zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Server serves the API under /client/v4.
type Server struct {
	Token string
	mu    sync.Mutex
	zones []zone
	recs  map[string]*Record
}

// New returns a server with the given zones and token.
func New(token string, zones ...string) *Server {
	s := &Server{Token: token, recs: map[string]*Record{}}
	for _, z := range zones {
		s.zones = append(s.zones, zone{ID: "zone-" + strings.ReplaceAll(z, ".", "-"), Name: z, Status: "active"})
	}
	return s
}

// Records returns every record, sorted by name.
func (s *Server) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name || (out[i].Name == out[j].Name && out[i].ID < out[j].ID)
	})
	return out
}

// Seed adds a record directly, as if someone made it in the dashboard.
func (s *Server) Seed(r Record) Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, _ := s.zoneFor(r.Name)
	r.ID, r.ZoneID, r.ZoneName = newID(), z.ID, z.Name
	if r.TTL == 0 {
		r.TTL = 1
	}
	r.CreatedOn, r.ModifiedOn = time.Now().UTC(), time.Now().UTC()
	s.recs[r.ID] = &r
	return r
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) zoneFor(name string) (zone, bool) {
	best := zone{}
	for _, z := range s.zones {
		if (name == z.Name || strings.HasSuffix(name, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	return best, best.ID != ""
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func write(w http.ResponseWriter, status int, result any, info map[string]int, errs ...apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"success": len(errs) == 0, "errors": errs, "messages": []any{}, "result": result}
	if errs == nil {
		body["errors"] = []any{}
	}
	if info != nil {
		body["result_info"] = info
	}
	_ = json.NewEncoder(w).Encode(body)
}

func page[T any](items []T, r *http.Request) ([]T, map[string]int) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per <= 0 {
		per = 20
	}
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p <= 0 {
		p = 1
	}
	total := len(items)
	pages := (total + per - 1) / per
	start, end := (p-1)*per, p*per
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	return items[start:end], map[string]int{"page": p, "per_page": per, "count": end - start, "total_count": total, "total_pages": pages}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		write(w, http.StatusForbidden, nil, nil, apiError{10000, "Authentication error"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case len(parts) == 1 && parts[0] == "zones" && r.Method == http.MethodGet:
		zones := s.zones
		if name := r.URL.Query().Get("name"); name != "" {
			zones = nil
			for _, z := range s.zones {
				if z.Name == name {
					zones = append(zones, z)
				}
			}
		}
		items, info := page(zones, r)
		write(w, http.StatusOK, items, info)
	case len(parts) >= 3 && parts[0] == "zones" && parts[2] == "dns_records":
		var z zone
		for _, candidate := range s.zones {
			if candidate.ID == parts[1] {
				z = candidate
			}
		}
		if z.ID == "" {
			write(w, http.StatusNotFound, nil, nil, apiError{7003, "Could not route to /zones/" + parts[1] + ", perhaps your object identifier is invalid?"})
			return
		}
		if len(parts) == 3 {
			s.records(w, r, z)
			return
		}
		s.record(w, r, z, parts[3])
	default:
		write(w, http.StatusNotFound, nil, nil, apiError{7000, "No route for that URI"})
	}
}

func (s *Server) records(w http.ResponseWriter, r *http.Request, z zone) {
	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")
		if name == "" {
			name = r.URL.Query().Get("name.exact")
		}
		var items []Record
		for _, rec := range s.recs {
			if rec.ZoneID != z.ID || (name != "" && rec.Name != name) || (r.URL.Query().Get("type") != "" && rec.Type != r.URL.Query().Get("type")) {
				continue
			}
			items = append(items, *rec)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].Name < items[j].Name || (items[i].Name == items[j].Name && items[i].ID < items[j].ID)
		})
		if r.URL.Query().Get("per_page") == "" {
			q := r.URL.Query()
			q.Set("per_page", "100")
			r.URL.RawQuery = q.Encode()
		}
		items, info := page(items, r)
		write(w, http.StatusOK, items, info)
	case http.MethodPost:
		var in Record
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, nil, nil, apiError{9207, "Request body is invalid."})
			return
		}
		if e, ok := s.check(in, z, ""); !ok {
			write(w, http.StatusBadRequest, nil, nil, e)
			return
		}
		in.ID, in.ZoneID, in.ZoneName = newID(), z.ID, z.Name
		if in.TTL == 0 {
			in.TTL = 1
		}
		in.CreatedOn, in.ModifiedOn = time.Now().UTC(), time.Now().UTC()
		s.recs[in.ID] = &in
		write(w, http.StatusOK, in, nil)
	default:
		write(w, http.StatusMethodNotAllowed, nil, nil, apiError{10405, "Method not allowed"})
	}
}

func (s *Server) record(w http.ResponseWriter, r *http.Request, z zone, id string) {
	rec, ok := s.recs[id]
	if !ok || rec.ZoneID != z.ID {
		write(w, http.StatusNotFound, nil, nil, apiError{81044, "Record does not exist."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		write(w, http.StatusOK, *rec, nil)
	case http.MethodDelete:
		delete(s.recs, id)
		write(w, http.StatusOK, map[string]string{"id": id}, nil)
	case http.MethodPatch, http.MethodPut:
		updated := *rec
		var patch map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			write(w, http.StatusBadRequest, nil, nil, apiError{9207, "Request body is invalid."})
			return
		}
		b, _ := json.Marshal(updated)
		var merged map[string]json.RawMessage
		_ = json.Unmarshal(b, &merged)
		for k, v := range patch {
			merged[k] = v
		}
		b, _ = json.Marshal(merged)
		_ = json.Unmarshal(b, &updated)
		updated.ID, updated.ZoneID, updated.ZoneName = rec.ID, z.ID, z.Name
		if e, ok := s.check(updated, z, id); !ok {
			write(w, http.StatusBadRequest, nil, nil, e)
			return
		}
		updated.ModifiedOn = time.Now().UTC()
		s.recs[id] = &updated
		write(w, http.StatusOK, updated, nil)
	default:
		write(w, http.StatusMethodNotAllowed, nil, nil, apiError{10405, "Method not allowed"})
	}
}

// check applies the rules Cloudflare applies that quark could trip over.
func (s *Server) check(in Record, z zone, self string) (apiError, bool) {
	if in.Name != z.Name && !strings.HasSuffix(in.Name, "."+z.Name) {
		return apiError{81031, "Invalid DNS record name: " + in.Name + " is not in " + z.Name}, false
	}
	switch in.Type {
	case "A":
		if ip := net.ParseIP(in.Content); ip == nil || ip.To4() == nil {
			return apiError{9005, "Content for A record must be a valid IPv4 address."}, false
		}
	case "AAAA", "CNAME", "TXT":
	default:
		return apiError{9000, "DNS record type is invalid."}, false
	}
	if len(in.Comment) > 100 {
		return apiError{9101, "Comment is too long (maximum 100 characters)."}, false
	}
	for id, rec := range s.recs {
		if id == self || rec.Name != in.Name || rec.ZoneID != z.ID {
			continue
		}
		if rec.Type == "CNAME" || in.Type == "CNAME" {
			return apiError{81053, "An A, AAAA, or CNAME record with that host already exists."}, false
		}
		if rec.Type == in.Type && rec.Content == in.Content {
			return apiError{81058, "An identical record already exists."}, false
		}
	}
	return apiError{}, true
}
