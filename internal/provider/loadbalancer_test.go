package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/cloud-provider/api"

	"github.com/boostrun/boostrun-cloud-controller-manager/internal/brapi"
)

type fakeAPI struct {
	balancers    map[string]*brapi.LoadBalancer
	nextID       int
	allocate     string // IP given to a LB created without frontend_ip, once activate() runs
	rejectIPs    map[string]bool
	setCalls     int
	createCalls  []brapi.CreateRequest
	listErr      error
	requested    map[string]string // LB id -> specific address asked for, allocated on activate()
	eligible     []string          // IPs reported by eligible-rentals; nil means the platform reports none
	clusters     []brapi.NetworkCluster
	clusterCalls int
	updateErr    error // returned by every UpdateLoadBalancer when set
}

func newFake() *fakeAPI {
	return &fakeAPI{balancers: map[string]*brapi.LoadBalancer{}, requested: map[string]string{}, allocate: "203.0.113.10", rejectIPs: map[string]bool{}}
}

func (f *fakeAPI) ListLoadBalancers(_ context.Context, prefix string, includeDeleting bool) ([]brapi.LoadBalancer, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []brapi.LoadBalancer
	for _, b := range f.balancers {
		if len(b.Name) >= len(prefix) && b.Name[:len(prefix)] == prefix &&
			(includeDeleting || b.Status != brapi.StatusDeleting) {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (f *fakeAPI) CreateLoadBalancer(_ context.Context, r brapi.CreateRequest, _ string) (*brapi.LoadBalancer, error) {
	f.createCalls = append(f.createCalls, r)
	f.nextID++
	b := &brapi.LoadBalancer{
		ID: fmt.Sprintf("lb-%d", f.nextID), Name: r.Name, Status: "pending", Protocol: r.Protocol,
		FrontendPort: r.FrontendPort, HealthcheckType: r.HealthcheckType,
		HealthcheckTimeoutMS: r.HealthcheckTimeoutMS, HealthcheckRequestPath: r.HealthcheckRequestPath,
		Description: r.Description, Enabled: true, IPFamily: r.IPFamily,
	}
	if r.FrontendIP != "" {
		ip := r.FrontendIP
		held := false
		for _, other := range f.balancers {
			held = held || other.IP() == ip
		}
		if held { // sharing a held address; otherwise it is a request the platform allocates later
			b.FrontendIP = &ip
		} else {
			f.requested[b.ID] = ip
		}
	}
	f.balancers[b.ID] = b
	copied := *b
	return &copied, nil
}

func (f *fakeAPI) UpdateLoadBalancer(_ context.Context, id string, r brapi.UpdateRequest) (*brapi.LoadBalancer, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	b := f.balancers[id]
	if r.HealthcheckType != nil {
		b.HealthcheckType = *r.HealthcheckType
	}
	if r.Description != nil {
		b.Description = *r.Description
	}
	if r.Enabled != nil {
		b.Enabled = *r.Enabled
	}
	if r.HealthcheckTimeoutMS != nil {
		b.HealthcheckTimeoutMS = *r.HealthcheckTimeoutMS
	}
	copied := *b
	return &copied, nil
}

func (f *fakeAPI) DeleteLoadBalancer(_ context.Context, id string) error {
	if b, ok := f.balancers[id]; ok {
		b.Status = brapi.StatusDeleting
	}
	return nil
}

func (f *fakeAPI) ListEligibleNetworkClusters(_ context.Context) ([]brapi.NetworkCluster, error) {
	f.clusterCalls++
	return f.clusters, nil
}

func (f *fakeAPI) ListEligibleRentals(_ context.Context, _ string) ([]brapi.EligibleRental, error) {
	if f.eligible == nil {
		return nil, nil
	}
	var out []brapi.EligibleRental
	for _, ip := range f.eligible {
		address := ip
		out = append(out, brapi.EligibleRental{RentalID: "r-" + ip, InternalIP: &address})
	}
	return out, nil
}

func (f *fakeAPI) SetBackends(_ context.Context, id string, targets []brapi.BackendTarget) ([]brapi.Backend, error) {
	f.setCalls++
	for _, t := range targets {
		if f.rejectIPs[t.InternalIP] {
			return nil, &brapi.Error{StatusCode: 400, Messages: []string{"No active rental with internal IP " + t.InternalIP}}
		}
	}
	b := f.balancers[id]
	b.Backends = nil
	for _, t := range targets {
		ip := t.InternalIP
		b.Backends = append(b.Backends, brapi.Backend{InternalIP: &ip, Port: t.Port})
	}
	return b.Backends, nil
}

func (f *fakeAPI) AddBackend(_ context.Context, id string, t brapi.BackendTarget) error {
	if f.rejectIPs[t.InternalIP] {
		return &brapi.Error{StatusCode: 400, Messages: []string{"No active rental"}}
	}
	b := f.balancers[id]
	ip := t.InternalIP
	b.Backends = append(b.Backends, brapi.Backend{InternalIP: &ip, Port: t.Port})
	return nil
}

func (f *fakeAPI) RemoveBackend(_ context.Context, id string, t brapi.BackendTarget) error {
	b := f.balancers[id]
	var kept []brapi.Backend
	for _, backend := range b.Backends {
		if backend.IP() != t.InternalIP || backend.Port != t.Port {
			kept = append(kept, backend)
		}
	}
	b.Backends = kept
	return nil
}

// activate simulates the platform finishing provisioning.
func (f *fakeAPI) activate() {
	for _, b := range f.balancers {
		if b.Status == "pending" {
			b.Status = brapi.StatusActive
			if b.FrontendIP == nil {
				ip := f.allocate
				if asked, ok := f.requested[b.ID]; ok {
					ip = asked
				}
				b.FrontendIP = &ip
			}
		}
	}
}

func (f *fakeAPI) names() []string {
	var out []string
	for _, b := range f.balancers {
		if b.Status != brapi.StatusDeleting {
			out = append(out, b.Name)
		}
	}
	sort.Strings(out)
	return out
}

func node(ip string) *v1.Node {
	return &v1.Node{Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
		{Type: v1.NodeHostName, Address: "h"}, {Type: v1.NodeInternalIP, Address: ip}}}}
}

func service(ports ...v1.ServicePort) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", UID: "11111111-2222-3333-4444-555555555555"},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: ports},
	}
}

func tcp(port, nodePort int32) v1.ServicePort {
	return v1.ServicePort{Protocol: v1.ProtocolTCP, Port: port, NodePort: nodePort}
}

func newLB(f *fakeAPI) *loadBalancers {
	return newLoadBalancers(f, "nc-1")
}

func isRetry(err error) bool {
	var retry *api.RetryError
	return errors.As(err, &retry)
}

func TestEnsureCreatesThenReturnsStatusWhenActive(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1"), node("10.0.0.2")}

	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); !isRetry(err) {
		t.Fatalf("first ensure should ask to retry while pending, got %v", err)
	}
	f.activate()
	status, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if got := status.Ingress[0].IP; got != "203.0.113.10" {
		t.Fatalf("ingress IP = %q", got)
	}
	lb := f.balancers["lb-1"]
	if len(lb.Backends) != 2 || lb.Backends[0].Port != 30080 {
		t.Fatalf("backends = %+v", lb.Backends)
	}
	if lb.HealthcheckType != "tcp" {
		t.Fatalf("TCP default health check = %q", lb.HealthcheckType)
	}
}

func TestSecondPortSharesFirstFrontendIP(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080), tcp(443, 30443))
	nodes := []*v1.Node{node("10.0.0.1")}

	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); !isRetry(err) {
		t.Fatalf("expected retry, got %v", err)
	}
	if len(f.createCalls) != 1 {
		t.Fatalf("only the first LB may be created before the address is known, got %d", len(f.createCalls))
	}
	f.activate()
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); !isRetry(err) {
		t.Fatalf("expected retry while the second LB is pending, got %v", err)
	}
	if got := f.createCalls[1].FrontendIP; got != "203.0.113.10" {
		t.Fatalf("second LB frontend_ip = %q, want shared address", got)
	}
	f.activate()
	status, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	if err != nil || status.Ingress[0].IP != "203.0.113.10" {
		t.Fatalf("status=%v err=%v", status, err)
	}
}

func TestNamesAreValidForPlatform(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(65535, 32767))
	svc.Spec.Ports = append(svc.Spec.Ports, v1.ServicePort{Protocol: v1.ProtocolUDP, Port: 65535, NodePort: 32766})
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	pattern := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for _, name := range f.names() {
		if !pattern.MatchString(name) || len(name) > 64 {
			t.Fatalf("invalid platform name %q (%d)", name, len(name))
		}
	}
	for _, r := range f.createCalls {
		if r.Protocol == "udp" && r.HealthcheckType != "none" {
			t.Fatalf("UDP must not be health checked, got %q", r.HealthcheckType)
		}
	}
}

func TestBackendSyncIsNoopWhenEqual(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	calls := f.setCalls
	if err := l.UpdateLoadBalancer(context.Background(), "c", svc, nodes); err != nil {
		t.Fatal(err)
	}
	if f.setCalls != calls {
		t.Fatalf("unchanged nodes caused %d extra PUT(s)", f.setCalls-calls)
	}
	if err := l.UpdateLoadBalancer(context.Background(), "c", svc, append(nodes, node("10.0.0.2"))); err != nil {
		t.Fatal(err)
	}
	if len(f.balancers["lb-1"].Backends) != 2 {
		t.Fatalf("added node not synced: %+v", f.balancers["lb-1"].Backends)
	}
}

func TestIneligibleNodeFallsBackToPerBackend(t *testing.T) {
	f := newFake()
	f.rejectIPs["10.9.9.9"] = true
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1"), node("10.9.9.9")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); err != nil {
		t.Fatalf("a non-rental node must not fail the service: %v", err)
	}
	backends := f.balancers["lb-1"].Backends
	if len(backends) != 1 || backends[0].IP() != "10.0.0.1" {
		t.Fatalf("backends = %+v", backends)
	}
}

func TestPortRemovedDeletesOldLoadBalancerAfterNewOne(t *testing.T) {
	f := newFake()
	l := newLB(f)
	nodes := []*v1.Node{node("10.0.0.1")}
	svc := service(tcp(80, 30080))
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	svc = service(tcp(8080, 30081))
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	if got := f.createCalls[1].FrontendIP; got != "203.0.113.10" {
		t.Fatalf("replacement must reuse the address, got %q", got)
	}
	if names := f.names(); len(names) != 1 || names[0] != "k8s-11111111-2222-3333-4444-555555555555-tcp-8080" {
		t.Fatalf("live LBs = %v", names)
	}
}

func TestRejectsUnsupportedServices(t *testing.T) {
	l := newLB(newFake())
	local := service(tcp(80, 30080))
	local.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	sctp := service(v1.ServicePort{Protocol: v1.ProtocolSCTP, Port: 80, NodePort: 30080})
	noNodePort := service(tcp(80, 0))
	for name, svc := range map[string]*v1.Service{"local": local, "sctp": sctp, "nodeport": noNodePort} {
		if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nil); err == nil || isRetry(err) {
			t.Errorf("%s: expected a terminal error, got %v", name, err)
		}
	}
	l.defaultNetworkClusterID = ""
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), nil); err == nil {
		t.Error("expected an error without a network cluster")
	}
}

func TestDeleteAndGet(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080))
	if _, exists, _ := l.GetLoadBalancer(context.Background(), "c", svc); exists {
		t.Fatal("nothing should exist yet")
	}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	f.activate()
	if status, exists, _ := l.GetLoadBalancer(context.Background(), "c", svc); !exists || status.Ingress[0].IP == "" {
		t.Fatalf("exists=%v status=%v", exists, status)
	}
	if err := l.EnsureLoadBalancerDeleted(context.Background(), "c", svc); err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureLoadBalancerDeleted(context.Background(), "c", svc); err != nil {
		t.Fatal("deleting twice must succeed")
	}
	if _, exists, _ := l.GetLoadBalancer(context.Background(), "c", svc); exists {
		t.Fatal("deleting LB must not count as existing")
	}
}

func TestThrottleBecomesRetryError(t *testing.T) {
	f := newFake()
	f.listErr = &brapi.Error{StatusCode: 429, RetryAfter: 42 * time.Second}
	_, err := newLB(f).EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), nil)
	var retry *api.RetryError
	if !errors.As(err, &retry) || retry.RetryAfter() != 42*time.Second {
		t.Fatalf("got %v", err)
	}
}

func TestBackendTargetsFamilyFilter(t *testing.T) {
	nodes := []*v1.Node{node("10.0.0.1"), node("fd00::1")}
	if got := backendTargets(nodes, "ipv4", 1); len(got) != 1 || got[0].InternalIP != "10.0.0.1" {
		t.Fatalf("ipv4: %+v", got)
	}
	if got := backendTargets(nodes, "ipv6", 1); len(got) != 1 || got[0].InternalIP != "fd00::1" {
		t.Fatalf("ipv6: %+v", got)
	}
}

func TestIPFamilyMismatchDoesNotWipeBackends(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	svc.Annotations = map[string]string{AnnotationIPFamily: "ipv6"}
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.balancers["lb-1"].Backends) != 1 {
		t.Fatalf("backends were wiped: %+v", f.balancers["lb-1"].Backends)
	}
}

func TestIdempotencyKeyTracksPayload(t *testing.T) {
	svc := service(tcp(80, 30080))
	first := brapi.CreateRequest{Name: "k8s-" + string(svc.UID) + "-tcp-80", FrontendPort: 80}
	second := first
	second.FrontendIP = "203.0.113.10"
	p := portSpec{protocol: "tcp", port: 80, nodePort: 30080}
	if idempotencyKey(svc, p, first) == idempotencyKey(svc, p, second) {
		t.Fatal("different payloads must not share a key")
	}
	key, again := idempotencyKey(svc, p, first), idempotencyKey(svc, p, first)
	if key != again || len(key) > 255 {
		t.Fatal("key must be stable and short")
	}
}

func TestHealthTimeoutValidated(t *testing.T) {
	svc := service(tcp(80, 30080))
	svc.Annotations = map[string]string{AnnotationHealthTimeout: "100"}
	if _, err := newLB(newFake()).EnsureLoadBalancer(context.Background(), "c", svc, nil); err == nil || isRetry(err) {
		t.Fatalf("got %v", err)
	}
}

func TestRenumberedPortDeletesOldBeforeSyncingBackends(t *testing.T) {
	f := newFake()
	l := newLB(f)
	nodes := []*v1.Node{node("10.0.0.1")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), nodes)
	f.activate()
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), nodes)
	f.rejectIPs["10.0.0.1"] = true // the platform refuses the backend while the old LB still holds it
	_, err := l.EnsureLoadBalancer(context.Background(), "c", service(tcp(8080, 30080)), nodes)
	if !isRetry(err) {
		t.Fatalf("expected retry, got %v", err)
	}
	if names := f.names(); len(names) != 1 || !regexp.MustCompile(`tcp-8080$`).MatchString(names[0]) {
		t.Fatalf("old LB should already be deleting: %v", names)
	}
}

func TestDisabledLoadBalancerIsReEnabled(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080))
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	f.activate()
	f.balancers["lb-1"].Enabled = false
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	if !f.balancers["lb-1"].Enabled {
		t.Fatal("a disabled load balancer must be re-enabled")
	}
}

func TestIneligibleNodesArePreFilteredWithoutFallback(t *testing.T) {
	f := newFake()
	f.eligible = []string{"10.0.0.1"}
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1"), node("10.9.9.9")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); err != nil {
		t.Fatal(err)
	}
	backends := f.balancers["lb-1"].Backends
	if len(backends) != 1 || backends[0].IP() != "10.0.0.1" {
		t.Fatalf("backends = %+v", backends)
	}
}

func TestRequestedIPIsOnlyUsedForTheFirstLoadBalancer(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080), tcp(443, 30443))
	svc.Spec.LoadBalancerIP = "203.0.113.50"
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	if len(f.createCalls) != 1 || f.createCalls[0].FrontendIP != "203.0.113.50" {
		t.Fatalf("creates = %+v", f.createCalls)
	}
	f.activate()
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	if len(f.createCalls) != 2 || f.createCalls[1].FrontendIP != "203.0.113.50" {
		t.Fatalf("second create = %+v", f.createCalls)
	}
}

func TestRequestedAddressErrorIsExplained(t *testing.T) {
	f := newFake()
	l := newLB(f)
	svc := service(tcp(80, 30080))
	svc.Spec.LoadBalancerIP = "203.0.113.50"
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	requested := "203.0.113.50"
	f.balancers["lb-1"].Status = brapi.StatusError
	f.balancers["lb-1"].RequestedFrontendIP = &requested
	_, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	if err == nil || isRetry(err) || !strings.Contains(err.Error(), "203.0.113.50") {
		t.Fatalf("got %v", err)
	}
}

func TestNoEligibleNodeFailsInsteadOfServingNothing(t *testing.T) {
	f := newFake()
	f.eligible = []string{"10.0.0.1", "10.0.0.2"}
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("192.168.1.10"), node("192.168.1.11")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	status, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	if err == nil || isRetry(err) || status != nil || !strings.Contains(err.Error(), "192.168.1.10") {
		t.Fatalf("status = %v, err = %v", status, err)
	}
	if f.setCalls != 0 {
		t.Fatalf("backends were written %d time(s)", f.setCalls)
	}
}

func TestNoEligibleNodeKeepsServingBackends(t *testing.T) {
	f := newFake()
	f.eligible = []string{"10.0.0.1", "10.0.0.2"}
	l := newLB(f)
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1"), node("10.0.0.2")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); err != nil {
		t.Fatal(err)
	}
	f.eligible = []string{"10.0.0.99"}
	nodes = append(nodes, node("10.0.0.3"))
	if err := l.UpdateLoadBalancer(context.Background(), "c", svc, nodes); err == nil {
		t.Fatal("expected an error when no node is eligible")
	}
	if got := len(f.balancers["lb-1"].Backends); got != 2 {
		t.Fatalf("backends = %d, want the 2 serving ones kept", got)
	}
}

func TestNoNodesEmptiesBackends(t *testing.T) {
	f := newFake()
	f.eligible = []string{"10.0.0.1"}
	l := newLB(f)
	svc := service(tcp(80, 30080))
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, []*v1.Node{node("10.0.0.1")})
	f.activate()
	if err := l.UpdateLoadBalancer(context.Background(), "c", svc, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(f.balancers["lb-1"].Backends); got != 0 {
		t.Fatalf("backends = %d, want 0 with no nodes", got)
	}
}

func newEventLB(f *fakeAPI, defaultCluster string) (*loadBalancers, *record.FakeRecorder) {
	l := newLoadBalancers(f, defaultCluster)
	recorder := record.NewFakeRecorder(20)
	l.recorder = recorder
	return l, recorder
}

func events(recorder *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case event := <-recorder.Events:
			out = append(out, event)
		default:
			return out
		}
	}
}

func TestNetworkClusterIsChosenAutomaticallyWhenThereIsOnlyOne(t *testing.T) {
	f := newFake()
	f.clusters = []brapi.NetworkCluster{{ID: "nc-only", Name: "default"}}
	l, _ := newEventLB(f, "")
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), []*v1.Node{node("10.0.0.1")}); !isRetry(err) {
		t.Fatalf("err = %v", err)
	}
	if got := f.createCalls[0].NetworkClusterID; got != "nc-only" {
		t.Fatalf("created in %q", got)
	}
}

func TestSeveralNetworkClustersNeedAChoice(t *testing.T) {
	f := newFake()
	f.clusters = []brapi.NetworkCluster{{ID: "nc-b", Name: "east"}, {ID: "nc-a", Name: "west"}}
	l, _ := newEventLB(f, "")
	_, err := l.EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), []*v1.Node{node("10.0.0.1")})
	if err == nil || !strings.Contains(err.Error(), "nc-a (west), nc-b (east)") || !strings.Contains(err.Error(), AnnotationNetworkCluster) {
		t.Fatalf("err = %v", err)
	}
	if len(f.createCalls) != 0 {
		t.Fatal("nothing should be created")
	}
	f.clusters = nil
	l, _ = newEventLB(f, "")
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", service(tcp(80, 30080)), nil); err == nil ||
		!strings.Contains(err.Error(), "no network cluster") {
		t.Fatalf("err = %v", err)
	}
}

func TestEligibleNetworkClustersAreCached(t *testing.T) {
	f := newFake()
	f.clusters = []brapi.NetworkCluster{{ID: "nc-only"}}
	l, _ := newEventLB(f, "")
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if _, err := l.networkClusterFor(context.Background(), service(), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if f.clusterCalls != 1 {
		t.Fatalf("API called %d times within the TTL", f.clusterCalls)
	}
	clock = clock.Add(networkClusterTTL + time.Second)
	if _, err := l.networkClusterFor(context.Background(), service(), "", nil); err != nil || f.clusterCalls != 2 {
		t.Fatalf("calls = %d, err = %v", f.clusterCalls, err)
	}
}

func TestExistingLoadBalancersFixTheNetworkCluster(t *testing.T) {
	l, _ := newEventLB(newFake(), "nc-default")
	live := []brapi.LoadBalancer{{Name: "x", NetworkClusterID: "nc-old"}}
	if got, err := l.networkClusterFor(context.Background(), service(), "", live); err != nil || got != "nc-old" {
		t.Fatalf("got %q, err %v", got, err)
	}
	if _, err := l.networkClusterFor(context.Background(), service(), "nc-new", live); err == nil ||
		!strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := l.networkClusterFor(context.Background(), service(), "nc-annotated", nil); got != "nc-annotated" {
		t.Fatalf("the annotation should win over the default for a new Service, got %q", got)
	}
}

func TestSourceRangesAreRejectedUnlessOpenToAll(t *testing.T) {
	l, _ := newEventLB(newFake(), "nc-1")
	restricted := service(tcp(80, 30080))
	restricted.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	if _, err := l.parse(restricted, "c"); err == nil || !strings.Contains(err.Error(), "loadBalancerSourceRanges is not supported") {
		t.Fatalf("err = %v", err)
	}
	annotated := service(tcp(80, 30080))
	annotated.Annotations = map[string]string{v1.AnnotationLoadBalancerSourceRangesKey: "192.0.2.0/24"}
	if _, err := l.parse(annotated, "c"); err == nil {
		t.Fatal("the source-ranges annotation must be rejected too")
	}
	open := service(tcp(80, 30080))
	open.Spec.LoadBalancerSourceRanges = []string{"0.0.0.0/0"}
	if _, err := l.parse(open, "c"); err != nil {
		t.Fatalf("0.0.0.0/0 restricts nothing and should be accepted: %v", err)
	}
}

func TestHealthPathValidated(t *testing.T) {
	l, _ := newEventLB(newFake(), "nc-1")
	for path, ok := range map[string]bool{"/healthz": true, "/a?b=c": true, "healthz": false, "/a b": false,
		"/" + strings.Repeat("a", healthPathLimit): false} {
		svc := service(tcp(80, 30080))
		svc.Annotations = map[string]string{AnnotationHealthPath: path}
		if _, err := l.parse(svc, "c"); (err == nil) != ok {
			t.Errorf("path %q: err = %v", path, err)
		}
	}
}

func TestDescriptionNamesTheCluster(t *testing.T) {
	l, _ := newEventLB(newFake(), "nc-1")
	want, err := l.parse(service(tcp(80, 30080)), "prod-east")
	if err != nil || want.description != "Kubernetes Service prod/web in cluster prod-east" {
		t.Fatalf("description = %q, err = %v", want.description, err)
	}
}

func TestDualStackIsReportedOnTheService(t *testing.T) {
	f := newFake()
	l, recorder := newEventLB(f, "nc-1")
	svc := service(tcp(80, 30080))
	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, []*v1.Node{node("10.0.0.1")})
	got := events(recorder)
	if len(got) != 1 || !strings.Contains(got[0], reasonIPFamilyIgnored) || !strings.Contains(got[0], "IPv4") {
		t.Fatalf("events = %v", got)
	}
}

func TestSkippedNodesAreReportedOnTheService(t *testing.T) {
	f := newFake()
	f.eligible = []string{"10.0.0.1"}
	l, recorder := newEventLB(f, "nc-1")
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1"), node("10.0.0.9")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(events(recorder), "\n")
	if !strings.Contains(got, reasonNodesSkipped) || !strings.Contains(got, "10.0.0.9") || strings.Contains(got, "10.0.0.1,") {
		t.Fatalf("events = %s", got)
	}
}

func TestSourceRangesDisableAnExistingServiceUntilRemoved(t *testing.T) {
	f := newFake()
	l, _ := newEventLB(f, "nc-1")
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); err != nil {
		t.Fatal(err)
	}
	svc.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); !errors.Is(err, errSourceRanges) {
		t.Fatalf("err = %v", err)
	}
	if f.balancers["lb-1"].Enabled {
		t.Fatal("the load balancer must be disabled while the Service asks for source ranges")
	}
	if err := l.UpdateLoadBalancer(context.Background(), "c", svc, nodes); !errors.Is(err, errSourceRanges) {
		t.Fatalf("node updates must fail closed too: %v", err)
	}
	svc.Spec.LoadBalancerSourceRanges = nil
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	if !f.balancers["lb-1"].Enabled {
		t.Fatal("removing the source ranges must enable the load balancer again")
	}
}

func TestOpenSourceRangesAreAccepted(t *testing.T) {
	l, _ := newEventLB(newFake(), "nc-1")
	for _, tc := range []struct {
		family string
		ranges []string
		open   bool
	}{
		{"ipv4", []string{"0.0.0.0/0"}, true},
		{"ipv4", []string{"0.0.0.0/0", "10.0.0.0/8"}, true},
		{"ipv4", []string{"0.0.0.0/0", "::/0"}, true},
		{"ipv4", []string{"::/0"}, false},
		{"ipv6", []string{"::/0"}, true},
		{"ipv6", []string{"0.0.0.0/0"}, false},
		{"ipv6", []string{"fd00::/8", "::/0"}, true},
	} {
		svc := service(tcp(80, 30080))
		svc.Annotations = map[string]string{AnnotationIPFamily: tc.family}
		svc.Spec.LoadBalancerSourceRanges = tc.ranges
		if _, err := l.parse(svc, "c"); (err == nil) != tc.open {
			t.Errorf("%s %v: err = %v, want open=%v", tc.family, tc.ranges, err, tc.open)
		}
	}
	for _, annotations := range []map[string]string{
		{AnnotationIPFamily: "ipv6"},
		{AnnotationIPFamily: "ipv6", v1.AnnotationLoadBalancerSourceRangesKey: " "},
	} {
		ipv6 := service(tcp(80, 30080))
		ipv6.Annotations = annotations
		if _, err := l.parse(ipv6, "c"); err != nil {
			t.Fatalf("an IPv6 Service without source ranges is open (%v): %v", annotations, err)
		}
	}
}

func TestIPFamilyCannotChange(t *testing.T) {
	f := newFake()
	l, _ := newEventLB(f, "nc-1")
	svc := service(tcp(80, 30080))
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, []*v1.Node{node("10.0.0.1")})
	svc.Annotations = map[string]string{AnnotationIPFamily: "ipv6"}
	_, err := l.EnsureLoadBalancer(context.Background(), "c", svc, []*v1.Node{node("fd00::1")})
	if err == nil || !strings.Contains(err.Error(), "IP family cannot change") {
		t.Fatalf("err = %v", err)
	}
	if err := l.UpdateLoadBalancer(context.Background(), "c", svc, []*v1.Node{node("fd00::1")}); err == nil ||
		!strings.Contains(err.Error(), "IP family cannot change") {
		t.Fatalf("update err = %v", err)
	}
}

func TestEmptyNetworkClusterListIsNotCached(t *testing.T) {
	f := newFake()
	l, _ := newEventLB(f, "")
	if _, err := l.networkClusterFor(context.Background(), service(), "", nil); err == nil {
		t.Fatal("expected an error with no clusters")
	}
	f.clusters = []brapi.NetworkCluster{{ID: "nc-new"}}
	if got, err := l.networkClusterFor(context.Background(), service(), "", nil); err != nil || got != "nc-new" {
		t.Fatalf("a newly added cluster must be used at once: got %q, err %v", got, err)
	}
}

func TestSourceRangesFailClosedEvenWithOtherProblems(t *testing.T) {
	f := newFake()
	l, _ := newEventLB(f, "nc-1")
	svc := service(tcp(80, 30080))
	nodes := []*v1.Node{node("10.0.0.1")}
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, nodes)
	f.activate()
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	svc.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	if _, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nodes); !errors.Is(err, errSourceRanges) {
		t.Fatalf("err = %v", err)
	}
	if f.balancers["lb-1"].Enabled {
		t.Fatal("source ranges must disable the load balancer even when the Service has other problems")
	}
}

func TestFailedDisableStillNamesSourceRanges(t *testing.T) {
	f := newFake()
	l, _ := newEventLB(f, "nc-1")
	svc := service(tcp(80, 30080))
	_, _ = l.EnsureLoadBalancer(context.Background(), "c", svc, []*v1.Node{node("10.0.0.1")})
	f.updateErr = &brapi.Error{StatusCode: 409, Messages: []string{"busy"}}
	svc.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	_, err := l.EnsureLoadBalancer(context.Background(), "c", svc, nil)
	if err == nil || !strings.Contains(err.Error(), "loadBalancerSourceRanges") || !isRetry(err) {
		t.Fatalf("err = %v, want a retry that names loadBalancerSourceRanges", err)
	}
}
