// Command fakeplatform is an in-memory stand-in for the BoostRun platform load balancer API, used
// by the kind install test in CI. It implements the endpoints the controller calls, provisions
// asynchronously like the platform (a new load balancer is pending for a moment before it gets an
// address and goes active), serves its state at GET /debug/state for assertions, and takes the
// eligible rental IPs at PUT /debug/rentals.
//
// It is a test double: no persistence, no validation beyond what the controller relies on.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type backend struct {
	BackendID  string `json:"backend_id"`
	RentalID   string `json:"rental_id"`
	InternalIP string `json:"internal_ip"`
	Port       int    `json:"port"`
	Status     string `json:"status"`
}

type loadBalancer struct {
	ID                     string    `json:"load_balancer_id"`
	Name                   string    `json:"name"`
	Description            string    `json:"description"`
	Status                 string    `json:"status"`
	FrontendIP             *string   `json:"frontend_ip"`
	FrontendPort           int       `json:"frontend_port"`
	Protocol               string    `json:"protocol"`
	IPFamily               string    `json:"ip_family"`
	Enabled                bool      `json:"enabled"`
	HealthcheckType        string    `json:"healthcheck_type"`
	HealthcheckTimeoutMS   int       `json:"healthcheck_timeout_ms"`
	HealthcheckRequestPath string    `json:"healthcheck_request_path"`
	NetworkClusterID       string    `json:"network_cluster_id"`
	Backends               []backend `json:"backends"`
	readyAt                time.Time
	goneAt                 time.Time
}

type server struct {
	mu        sync.Mutex
	key       string
	delay     time.Duration
	balancers map[string]*loadBalancer
	nextID    int
	nextIP    int
	idem      map[string]string
	// rentals are the internal IPs reported as eligible backends. Empty means the fake reports none,
	// and the controller then skips its pre-filter.
	rentals []string
}

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	key := flag.String("api-key", "test-key", "accepted X-API-Key")
	delay := flag.Duration("provision-delay", 2*time.Second, "time a load balancer stays pending or deleting")
	flag.Parse()
	s := &server{key: *key, delay: *delay, balancers: map[string]*loadBalancer{}, idem: map[string]string{}}
	log.Printf("fake platform listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, s))
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/debug/rentals" && r.Method == http.MethodPut {
		var rentals []string
		if err := json.NewDecoder(r.Body).Decode(&rentals); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid JSON.")
			return
		}
		s.mu.Lock()
		s.rentals = rentals
		s.mu.Unlock()
		writeData(w, http.StatusOK, rentals)
		return
	}
	if r.URL.Path == "/debug/state" {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.tick()
		writeJSON(w, http.StatusOK, s.sorted("", true))
		return
	}
	if r.Header.Get("X-API-Key") != s.key {
		writeError(w, http.StatusUnauthorized, "Invalid API key.")
		return
	}
	// %q escapes newlines and control characters, so a request cannot forge log lines.
	log.Printf("%s %q (%q)", r.Method, r.URL.RequestURI(), r.UserAgent())
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/load-balancers")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tick()
	switch {
	case path == "" && r.Method == http.MethodGet:
		query := r.URL.Query()
		writeData(w, http.StatusOK, s.sorted(query.Get("name_prefix"), query.Get("include_deleting") == "true"))
	case path == "" && r.Method == http.MethodPost:
		s.create(w, r)
	case parts[0] == "eligible-network-clusters":
		writeData(w, http.StatusOK, []map[string]any{{"network_cluster_id": "nc-kind", "name": "kind",
			"ipv4_available": true, "ipv6_available": false}})
	default:
		balancer := s.balancers[parts[0]]
		if balancer == nil {
			writeError(w, http.StatusNotFound, "Load balancer not found.")
			return
		}
		s.item(w, r, balancer, parts[1:])
	}
}

// tick advances provisioning: pending load balancers become active, deleting ones disappear.
func (s *server) tick() {
	now := time.Now()
	for id, balancer := range s.balancers {
		if balancer.Status == "pending" && now.After(balancer.readyAt) {
			if balancer.FrontendIP == nil {
				s.nextIP++
				ip := fmt.Sprintf("198.51.100.%d", s.nextIP)
				balancer.FrontendIP = &ip
			}
			balancer.Status = "active"
		}
		if balancer.Status == "deleting" && now.After(balancer.goneAt) {
			delete(s.balancers, id)
		}
	}
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name                   string `json:"name"`
		Description            string `json:"description"`
		NetworkClusterID       string `json:"network_cluster_id"`
		IPFamily               string `json:"ip_family"`
		Protocol               string `json:"protocol"`
		FrontendPort           int    `json:"frontend_port"`
		FrontendIP             string `json:"frontend_ip"`
		HealthcheckType        string `json:"healthcheck_type"`
		HealthcheckTimeoutMS   int    `json:"healthcheck_timeout_ms"`
		HealthcheckRequestPath string `json:"healthcheck_request_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON.")
		return
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		if id, ok := s.idem[key]; ok && s.balancers[id] != nil {
			writeData(w, http.StatusCreated, s.balancers[id])
			return
		}
	}
	for _, other := range s.balancers {
		if other.Status != "deleting" && other.FrontendPort == request.FrontendPort &&
			other.Protocol == request.Protocol && request.FrontendIP != "" && other.FrontendIP != nil &&
			*other.FrontendIP == request.FrontendIP {
			writeError(w, http.StatusBadRequest, "That port is already used on this address.")
			return
		}
	}
	s.nextID++
	balancer := &loadBalancer{
		ID: fmt.Sprintf("lb-%d", s.nextID), Name: request.Name, Description: request.Description,
		Status: "pending", FrontendPort: request.FrontendPort, Protocol: request.Protocol,
		IPFamily: request.IPFamily, Enabled: true, HealthcheckType: request.HealthcheckType,
		HealthcheckTimeoutMS: request.HealthcheckTimeoutMS, HealthcheckRequestPath: request.HealthcheckRequestPath,
		NetworkClusterID: request.NetworkClusterID, Backends: []backend{}, readyAt: time.Now().Add(s.delay),
	}
	if request.HealthcheckType == "" {
		balancer.HealthcheckType = "none"
	}
	if request.FrontendIP != "" {
		ip := request.FrontendIP
		balancer.FrontendIP = &ip
	}
	s.balancers[balancer.ID] = balancer
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		s.idem[key] = balancer.ID
	}
	writeData(w, http.StatusCreated, balancer)
}

func (s *server) item(w http.ResponseWriter, r *http.Request, balancer *loadBalancer, rest []string) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeData(w, http.StatusOK, balancer)
	case len(rest) == 0 && r.Method == http.MethodPatch:
		var patch map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid JSON.")
			return
		}
		for field, target := range map[string]any{"description": &balancer.Description, "enabled": &balancer.Enabled,
			"healthcheck_type": &balancer.HealthcheckType, "healthcheck_timeout_ms": &balancer.HealthcheckTimeoutMS,
			"healthcheck_request_path": &balancer.HealthcheckRequestPath} {
			if raw, ok := patch[field]; ok {
				_ = json.Unmarshal(raw, target)
			}
		}
		writeData(w, http.StatusOK, balancer)
	case len(rest) == 0 && r.Method == http.MethodDelete:
		if balancer.Status != "deleting" {
			balancer.Status = "deleting"
			balancer.goneAt = time.Now().Add(s.delay)
		}
		writeData(w, http.StatusAccepted, nil)
	case len(rest) == 0:
		writeError(w, http.StatusMethodNotAllowed, "Not implemented by the fake platform.")
	case rest[0] == "eligible-rentals":
		rentals := []map[string]string{}
		for _, ip := range s.rentals {
			rentals = append(rentals, map[string]string{"rental_id": "rental-" + ip, "internal_ip": ip})
		}
		writeData(w, http.StatusOK, rentals)
	case rest[0] == "backends" && r.Method == http.MethodPut:
		var body struct {
			Backends []backend `json:"backends"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid JSON.")
			return
		}
		balancer.Backends = []backend{}
		for i, target := range body.Backends {
			balancer.Backends = append(balancer.Backends, backend{BackendID: fmt.Sprintf("%s-b%d", balancer.ID, i),
				RentalID: "rental-" + target.InternalIP, InternalIP: target.InternalIP, Port: target.Port, Status: "included"})
		}
		writeData(w, http.StatusOK, balancer.Backends)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Not implemented by the fake platform.")
	}
}

func (s *server) sorted(prefix string, includeDeleting bool) []*loadBalancer {
	out := []*loadBalancer{}
	for _, balancer := range s.balancers {
		if strings.HasPrefix(balancer.Name, prefix) && (includeDeleting || balancer.Status != "deleting") {
			out = append(out, balancer)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func writeData(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, map[string]any{"status": "success", "message": nil, "errors": []string{}, "data": data})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"status": "error", "message": nil, "errors": []string{message}, "data": nil})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
