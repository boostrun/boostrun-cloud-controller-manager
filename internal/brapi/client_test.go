package brapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListFollowsPaginationAndSendsKey(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "secret" {
			t.Errorf("missing API key")
		}
		cursor := r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		switch cursor {
		case "":
			fmt.Fprintf(w, `{"status":"success","errors":[],"data":[{"name":"a"}],"pagination":{"next":%q}}`,
				server.URL+"/load-balancers?cursor=abc&page_size=200")
		case "abc":
			fmt.Fprint(w, `{"status":"success","errors":[],"data":[{"name":"b"}],"pagination":{"next":null}}`)
		default:
			t.Errorf("unexpected cursor %q", cursor)
		}
	}))
	defer server.Close()
	got, err := NewClient(server.URL, StaticKey("secret"), "", nil).ListLoadBalancers(context.Background(), "k8s-", false)
	if err != nil || len(got) != 2 || got[1].Name != "b" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestErrorParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"detail":"Request was throttled."}`)
	}))
	defer server.Close()
	_, err := NewClient(server.URL, StaticKey("k"), "", nil).GetLoadBalancer(context.Background(), "x")
	wait, ok := IsThrottled(err)
	if !ok || wait != 7*time.Second {
		t.Fatalf("err=%v wait=%v ok=%v", err, wait, ok)
	}
}

func TestCreateSendsIdempotencyKeyAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "key-1" {
			t.Errorf("idempotency key = %q", r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["frontend_port"].(float64) != 80 {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status":"success","errors":[],"data":{"load_balancer_id":"id-1","name":"n","frontend_ip":null}}`)
	}))
	defer server.Close()
	lb, err := NewClient(server.URL, StaticKey("k"), "", nil).CreateLoadBalancer(context.Background(),
		CreateRequest{Name: "n", FrontendPort: 80}, "key-1")
	if err != nil || lb.ID != "id-1" || lb.IP() != "" {
		t.Fatalf("lb=%+v err=%v", lb, err)
	}
}

func TestNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"status":"error","errors":["Backend not found"],"data":null}`)
	}))
	defer server.Close()
	if err := NewClient(server.URL, StaticKey("k"), "", nil).RemoveBackend(context.Background(), "x", BackendTarget{InternalIP: "10.0.0.1", Port: 1}); err != nil {
		t.Fatalf("removing a missing backend should be a no-op: %v", err)
	}
	if _, err := NewClient(server.URL, StaticKey("k"), "", nil).GetLoadBalancer(context.Background(), "x"); !IsNotFound(err) {
		t.Fatalf("got %v", err)
	}
}

func TestUserAgentAndKeyFileRotation(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "boostrun-cloud-controller-manager/v9" {
			t.Errorf("User-Agent = %q", got)
		}
		keys = append(keys, r.Header.Get("X-API-Key"))
		fmt.Fprint(w, `{"status":"success","errors":[],"data":[]}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewClient(server.URL, FileKey(path), "boostrun-cloud-controller-manager/v9", nil)
	if _, err := client.ListEligibleNetworkClusters(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListEligibleNetworkClusters(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "first" || keys[1] != "second" {
		t.Fatalf("keys sent = %v, want the rotated key picked up", keys)
	}
}

func TestEmptyKeyFileFailsWithoutCallingTheAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewClient("http://127.0.0.1:1", FileKey(path), "", nil)
	client.retryDelay = 0
	if _, err := client.ListEligibleNetworkClusters(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestTransientErrorsAreRetriedOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		call      func(*Client) error
		failures  int
		wantCalls int
		wantErr   bool
	}{
		{"GET recovers", func(c *Client) error {
			_, err := c.ListEligibleNetworkClusters(context.Background())
			return err
		}, 1, 2, false},
		{"GET fails twice", func(c *Client) error {
			_, err := c.ListEligibleNetworkClusters(context.Background())
			return err
		}, 2, 2, true},
		{"POST with an idempotency key recovers", func(c *Client) error {
			_, err := c.CreateLoadBalancer(context.Background(), CreateRequest{Name: "n"}, "key")
			return err
		}, 1, 2, false},
		{"POST without an idempotency key is not repeated", func(c *Client) error {
			return c.AddBackend(context.Background(), "id", BackendTarget{InternalIP: "10.0.0.1", Port: 1})
		}, 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls <= tc.failures {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				fmt.Fprint(w, `{"status":"success","errors":[],"data":null}`)
			}))
			defer server.Close()
			client := NewClient(server.URL, StaticKey("k"), "", nil)
			client.retryDelay = 0
			err := tc.call(client)
			if calls != tc.wantCalls || (err != nil) != tc.wantErr {
				t.Fatalf("calls = %d, err = %v", calls, err)
			}
		})
	}
}

func TestListEligibleNetworkClusters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/load-balancers/eligible-network-clusters" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"status":"success","errors":[],"data":[{"network_cluster_id":"nc-1","name":"default","ipv4_available":true,"ipv6_available":false}]}`)
	}))
	defer server.Close()
	got, err := NewClient(server.URL, StaticKey("k"), "", nil).ListEligibleNetworkClusters(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "nc-1" || got[0].Name != "default" || !got[0].IPv4Available {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	if got := truncate("aé", 2); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Fatalf("got %q", got)
	}
}
