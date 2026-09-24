// Package dockertest is a stand-in for the Docker Engine API, for tests.
//
// Docker's client speaks HTTP, so a package that talks to Docker through
// docker.Connect needs no seam of its own: New starts this server and
// points DOCKER_HOST at it for the test, and every call lands here. It
// keeps containers by name, answers what bedrock asks, and records each
// request so a test can say what happened and in what order.
package dockertest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// APIVersion is the Engine API version the stand-in speaks.
const APIVersion = "1.44"

// Container is one container as the stand-in keeps it.
type Container struct {
	ID    string
	Name  string
	Image string
	// Labels, Env, Entrypoint and Cmd are as created.
	Labels     map[string]string
	Env        []string
	Entrypoint []string
	Cmd        []string
	HostConfig HostConfig
	Running    bool
	ExitCode   int
	// RestartCount is how many times Docker has restarted it.
	RestartCount int
	// ExitAfter makes a started container exit on its own after this many
	// inspections; zero leaves it running until it is stopped.
	ExitAfter int
	// Logs is what the container has written.
	Logs string
}

// HostConfig is the part of a container's host configuration tests look at.
type HostConfig struct {
	Binds          []string
	CapAdd         []string
	CapDrop        []string
	Privileged     bool
	ReadonlyRootfs bool
	SecurityOpt    []string
	Tmpfs          map[string]string
	PidsLimit      *int64
	RestartPolicy  struct{ Name string }
	NetworkMode    string
}

// Server is the stand-in.
type Server struct {
	mu         sync.Mutex
	containers map[string]*Container // by name
	// volumes and networks are by name, with their labels.
	volumes  map[string]map[string]string
	networks map[string]map[string]string
	calls    []string
	next     int
	// OnCreate, when set, sees each container as it is created, before the
	// response, while whatever it was created from still exists.
	OnCreate func(c Container)
}

// New starts the stand-in for the length of the test and points the
// Docker client's environment at it.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{containers: map[string]*Container{}, volumes: map[string]map[string]string{}, networks: map[string]map[string]string{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp"+strings.TrimPrefix(srv.URL, "http"))
	t.Setenv("DOCKER_API_VERSION", APIVersion)
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	return s
}

// Add puts a container on the stand-in as if it had always been there.
func (s *Server) Add(c Container) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == "" {
		s.next++
		c.ID = fmt.Sprintf("c%d", s.next)
	}
	s.containers[c.Name] = &c
}

// Container returns a container by name.
func (s *Server) Container(name string) (Container, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[name]
	if !ok {
		return Container{}, false
	}
	return *c, true
}

// Remove takes a container away, as if someone had removed it by hand.
func (s *Server) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.containers, name)
}

// Update changes a container in place.
func (s *Server) Update(name string, change func(*Container)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.containers[name]; ok {
		change(c)
	}
}

// AddVolume puts a volume on the stand-in, with the labels it was made with.
func (s *Server) AddVolume(name string, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volumes[name] = labels
}

// AddNetwork puts a network on the stand-in, with the labels it was made with.
func (s *Server) AddNetwork(name string, labels map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.networks[name] = labels
}

// VolumeNames lists the volumes the stand-in holds, sorted.
func (s *Server) VolumeNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedNames(s.volumes)
}

// NetworkNames lists the networks the stand-in holds, sorted.
func (s *Server) NetworkNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sortedNames(s.networks)
}

func sortedNames(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Calls lists every request so far, as "METHOD /path" without the API
// version.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// Last is the index in Calls of the last request that fits, or -1.
func (s *Server) Last(fits func(call string) bool) int {
	at := -1
	for i, c := range s.Calls() {
		if fits(c) {
			at = i
		}
	}
	return at
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v"+APIVersion)
	s.mu.Lock()
	s.calls = append(s.calls, r.Method+" "+path)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("API-Version", APIVersion)
	switch {
	case path == "/_ping":
		_, _ = io.WriteString(w, "OK")
	case path == "/images/json":
		writeJSON(w, http.StatusOK, []any{})
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		writeJSON(w, http.StatusOK, map[string]any{"Id": "sha256:" + strings.NewReplacer("/", "-", ":", "-", "@", "-").Replace(ref), "Config": map[string]any{"User": ""}})
	case path == "/images/create":
		writeJSON(w, http.StatusOK, map[string]any{"status": "pulled"})
	case path == "/containers/json":
		s.list(w)
	case path == "/containers/create" && r.Method == http.MethodPost:
		s.create(w, r)
	case strings.HasPrefix(path, "/containers/"):
		s.container(w, r, strings.TrimPrefix(path, "/containers/"))
	case path == "/networks" && r.Method == http.MethodGet:
		s.mu.Lock()
		items := []map[string]any{}
		for _, name := range sortedNames(s.networks) {
			items = append(items, map[string]any{"Name": name, "Labels": s.networks[name]})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, items)
	case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.networks, strings.TrimPrefix(path, "/networks/"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "/networks/") && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"Name": strings.TrimPrefix(path, "/networks/"), "Labels": map[string]string{}})
	case path == "/networks/create":
		writeJSON(w, http.StatusCreated, map[string]any{"Id": "n1"})
	case path == "/volumes/create":
		writeJSON(w, http.StatusCreated, map[string]any{"Name": "v1"})
	case path == "/volumes" && r.Method == http.MethodGet:
		s.mu.Lock()
		items := []map[string]any{}
		for _, name := range sortedNames(s.volumes) {
			items = append(items, map[string]any{"Name": name, "Labels": s.volumes[name], "Driver": "local"})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"Volumes": items, "Warnings": []string{}})
	case strings.HasPrefix(path, "/volumes/") && r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.volumes, strings.TrimPrefix(path, "/volumes/"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) list(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []map[string]any{}
	for _, c := range s.containers {
		state := "exited"
		if c.Running {
			state = "running"
		}
		items = append(items, map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "Image": c.Image, "Labels": c.Labels, "State": state, "Status": state})
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Image      string
		Entrypoint []string
		Cmd        []string
		Env        []string
		Labels     map[string]string
		HostConfig HostConfig
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	name := r.URL.Query().Get("name")
	s.mu.Lock()
	if _, taken := s.containers[name]; taken {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"message": "the name " + name + " is in use"})
		return
	}
	s.next++
	c := &Container{ID: fmt.Sprintf("c%d", s.next), Name: name, Image: body.Image, Labels: body.Labels, Env: body.Env,
		Entrypoint: body.Entrypoint, Cmd: body.Cmd, HostConfig: body.HostConfig}
	s.containers[name] = c
	created := *c
	hook := s.OnCreate
	s.mu.Unlock()
	if hook != nil {
		hook(created)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"Id": c.ID, "Warnings": []string{}})
}

// container answers /containers/<id or name>[/<action>].
func (s *Server) container(w http.ResponseWriter, r *http.Request, rest string) {
	ref, action, _ := strings.Cut(rest, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.find(ref)
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + ref})
		return
	}
	switch {
	case r.Method == http.MethodDelete && action == "":
		delete(s.containers, c.Name)
		w.WriteHeader(http.StatusNoContent)
	case action == "start":
		c.Running = true
		w.WriteHeader(http.StatusNoContent)
	case action == "stop":
		c.Running = false
		w.WriteHeader(http.StatusNoContent)
	case action == "json":
		if c.Running && c.ExitAfter > 0 {
			c.ExitAfter--
			if c.ExitAfter == 0 {
				c.Running = false
			}
		}
		status := "exited"
		if c.Running {
			status = "running"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"Id": c.ID, "Name": "/" + c.Name, "RestartCount": c.RestartCount,
			"State":           map[string]any{"Status": status, "Running": c.Running, "ExitCode": c.ExitCode},
			"Config":          map[string]any{"Image": c.Image, "Labels": c.Labels},
			"NetworkSettings": map[string]any{"Networks": map[string]any{}},
		})
	case action == "logs":
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		frame := make([]byte, 8)
		frame[0] = 1 // stdout
		binary.BigEndian.PutUint32(frame[4:], uint32(len(c.Logs)))
		_, _ = w.Write(append(frame, c.Logs...))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) find(ref string) *Container {
	if c, ok := s.containers[ref]; ok {
		return c
	}
	for _, c := range s.containers {
		if c.ID == ref {
			return c
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
