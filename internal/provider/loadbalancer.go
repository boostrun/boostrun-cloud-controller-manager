package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/api"
	servicehelpers "k8s.io/cloud-provider/service/helpers"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/boostrun/boostrun-cloud-controller-manager/internal/brapi"
)

// Service annotations. Every BoostRun load balancer setting lives under one domain, so other
// BoostRun features can use sibling domains without colliding.
const (
	AnnotationPrefix         = "loadbalancer.boostrun.com/"
	AnnotationNetworkCluster = AnnotationPrefix + "network-cluster-id"
	AnnotationIPFamily       = AnnotationPrefix + "ip-family"
	AnnotationHealthcheck    = AnnotationPrefix + "healthcheck-type"
	AnnotationHealthTimeout  = AnnotationPrefix + "healthcheck-timeout-ms"
	AnnotationHealthPath     = AnnotationPrefix + "healthcheck-path"
)

const (
	namePrefix        = "k8s-"
	descriptionLimit  = 255
	provisioningRetry = 10 * time.Second
	defaultTimeoutMS  = 2000
	healthPathLimit   = 255
	// networkClusterTTL is how long the organization's eligible network clusters are cached.
	networkClusterTTL = 5 * time.Minute
)

// errSourceRanges rejects a Service that restricts client addresses: the platform cannot enforce
// it, and serving the Service open to everyone would silently ignore the restriction.
var errSourceRanges = errors.New("loadBalancerSourceRanges is not supported: BoostRun load balancers " +
	"cannot restrict client addresses yet, so the Service is not served (its load balancers are disabled " +
	"until you remove it); restrict access in the cluster instead, for example with a NetworkPolicy")

// Event reasons for problems that do not fail the Service.
const (
	reasonNodesSkipped    = "LoadBalancerNodesSkipped"
	reasonIPFamilyIgnored = "LoadBalancerIPFamilyIgnored"
)

// API is the subset of the platform client the provider needs.
type API interface {
	ListLoadBalancers(ctx context.Context, namePrefix string, includeDeleting bool) ([]brapi.LoadBalancer, error)
	CreateLoadBalancer(ctx context.Context, request brapi.CreateRequest, idempotencyKey string) (*brapi.LoadBalancer, error)
	UpdateLoadBalancer(ctx context.Context, id string, request brapi.UpdateRequest) (*brapi.LoadBalancer, error)
	DeleteLoadBalancer(ctx context.Context, id string) error
	ListEligibleNetworkClusters(ctx context.Context) ([]brapi.NetworkCluster, error)
	ListEligibleRentals(ctx context.Context, id string) ([]brapi.EligibleRental, error)
	SetBackends(ctx context.Context, id string, targets []brapi.BackendTarget) ([]brapi.Backend, error)
	AddBackend(ctx context.Context, id string, target brapi.BackendTarget) error
	RemoveBackend(ctx context.Context, id string, target brapi.BackendTarget) error
}

type loadBalancers struct {
	api                     API
	defaultNetworkClusterID string
	// recorder reports problems on the Service; nil until Initialize runs (and in tests).
	recorder record.EventRecorder

	clustersMu      sync.Mutex
	clusters        []brapi.NetworkCluster
	clustersFetched time.Time
	now             func() time.Time
}

func newLoadBalancers(api API, defaultNetworkClusterID string) *loadBalancers {
	return &loadBalancers{api: api, defaultNetworkClusterID: defaultNetworkClusterID, now: time.Now}
}

// warn records a Warning event on the Service, and logs it.
func (l *loadBalancers) warn(service *v1.Service, reason, message string) {
	klog.Warningf("service %s/%s: %s", service.Namespace, service.Name, message)
	if l.recorder != nil {
		l.recorder.Event(service, v1.EventTypeWarning, reason, message)
	}
}

var _ cloudprovider.LoadBalancer = (*loadBalancers)(nil)

// portSpec is one Service port, which becomes one platform load balancer.
type portSpec struct {
	protocol string // "tcp" or "udp"
	port     int
	nodePort int
}

func (p portSpec) suffix() string { return fmt.Sprintf("%s-%d", p.protocol, p.port) }

// desired is everything derived from a Service that the platform needs.
type desired struct {
	prefix           string // k8s-<uid>-
	ports            []portSpec
	networkClusterID string
	ipFamily         string
	healthcheckType  func(protocol string) string
	timeoutMS        int
	path             string
	description      string
	requestedIP      string
	// warnings are reported on the Service but do not stop it from being served.
	warnings []string
}

// LoadBalancerNamePrefix is the shared prefix of every platform load balancer of a Service.
// The Service UID makes it unique and stable across renames.
func loadBalancerNamePrefix(service *v1.Service) string {
	return namePrefix + string(service.UID) + "-"
}

func (l *loadBalancers) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	return strings.TrimSuffix(loadBalancerNamePrefix(service), "-")
}

func (l *loadBalancers) GetLoadBalancer(ctx context.Context, _ string, service *v1.Service) (*v1.LoadBalancerStatus, bool, error) {
	existing, err := l.api.ListLoadBalancers(ctx, loadBalancerNamePrefix(service), false)
	if err != nil {
		return nil, false, wrapAPIError("listing load balancers", err)
	}
	if len(existing) == 0 {
		return nil, false, nil
	}
	return statusFor(existing), true, nil
}

func (l *loadBalancers) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	want, err := l.parse(service, clusterName)
	if err != nil {
		return nil, l.failClosed(ctx, service, err)
	}
	for _, warning := range want.warnings {
		l.warn(service, reasonIPFamilyIgnored, warning)
	}
	existing, err := l.api.ListLoadBalancers(ctx, want.prefix, true)
	if err != nil {
		return nil, wrapAPIError("listing load balancers", err)
	}
	live, deleting := splitDeleting(existing)
	if err := checkIPFamily(want, live); err != nil {
		return nil, err
	}
	if want.networkClusterID, err = l.networkClusterFor(ctx, service, want.networkClusterID, live); err != nil {
		return nil, err
	}
	byName := map[string]*brapi.LoadBalancer{}
	for i := range live {
		byName[live[i].Name] = &live[i]
	}
	wantedNames := map[string]bool{}
	// frontendIP is the address the Service's load balancers already share. spec.loadBalancerIP is
	// only requested for the first one: the others must wait until it holds the address, because
	// then (and only then) the platform treats the address as theirs to share.
	frontendIP := firstFrontendIP(live)
	targetsFor := func(p portSpec) []brapi.BackendTarget { return backendTargets(nodes, want.ipFamily, p.nodePort) }
	notReady := false

	for _, p := range want.ports {
		name := want.prefix + p.suffix()
		wantedNames[name] = true
		balancer := byName[name]
		if balancer == nil {
			if frontendIP == "" && anyCreatedWithoutIP(byName, want) {
				// The first load balancer allocates the address; the others share it once it is known.
				notReady = true
				continue
			}
			createIP := frontendIP
			if createIP == "" {
				createIP = want.requestedIP
			}
			request := want.createRequest(p, createIP)
			created, err := l.api.CreateLoadBalancer(ctx, request, idempotencyKey(service, p, request))
			if err != nil {
				if len(deleting) > 0 && portInUse(err) {
					return nil, api.NewRetryError(
						"waiting for a previous load balancer of this Service to finish deleting: "+err.Error(),
						provisioningRetry)
				}
				return nil, wrapAPIError("creating load balancer "+name, err)
			}
			klog.V(2).Infof("service %s/%s: created load balancer %s", service.Namespace, service.Name, name)
			balancer = created
			byName[name] = created
			notReady = true
		} else if request, changed := want.settingsChange(balancer, p); changed {
			if _, err := l.api.UpdateLoadBalancer(ctx, balancer.ID, request); err != nil {
				return nil, wrapAPIError("updating load balancer "+name, err)
			}
			klog.V(2).Infof("service %s/%s: updated settings of load balancer %s", service.Namespace, service.Name, name)
			notReady = true
		}
		if frontendIP == "" {
			frontendIP = balancer.IP()
		}
	}

	// Extras go before backends are synced: a renumbered port keeps its NodePort, and the platform
	// refuses the same (protocol, server, port) backend on two load balancers at once.
	extras := 0
	for _, balancer := range live {
		if !wantedNames[balancer.Name] {
			extras++
		}
	}
	if err := l.deleteExtras(ctx, service, live, wantedNames); err != nil {
		return nil, err
	}
	for _, p := range want.ports {
		balancer := byName[want.prefix+p.suffix()]
		if balancer == nil {
			continue
		}
		if err := l.syncBackends(ctx, service, balancer, targetsFor(p), len(nodes)); err != nil {
			var retry *api.RetryError
			if !errors.As(err, &retry) && (extras > 0 || len(deleting) > 0) {
				return nil, api.NewRetryError(
					"waiting for a replaced load balancer to release its backends: "+err.Error(), provisioningRetry)
			}
			return nil, err
		}
	}

	current := make([]brapi.LoadBalancer, 0, len(byName))
	for name, balancer := range byName {
		if wantedNames[name] {
			current = append(current, *balancer)
		}
	}
	for _, balancer := range current {
		if balancer.Status == brapi.StatusError {
			if balancer.IP() == "" && balancer.RequestedFrontendIP != nil && *balancer.RequestedFrontendIP != "" {
				return nil, fmt.Errorf("platform load balancer %s is in error state: the requested address %s "+
					"(spec.loadBalancerIP) is not available; check that it is free, or that your organization "+
					"held it", balancer.Name, *balancer.RequestedFrontendIP)
			}
			return nil, fmt.Errorf("platform load balancer %s is in error state", balancer.Name)
		}
		if balancer.Status != brapi.StatusActive || balancer.IP() == "" {
			notReady = true
		}
	}
	if notReady || len(current) != len(want.ports) {
		return nil, api.NewRetryError("waiting for the platform load balancer(s) to become active", provisioningRetry)
	}
	return statusFor(current), nil
}

func (l *loadBalancers) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	want, err := l.parse(service, clusterName)
	if err != nil {
		return l.failClosed(ctx, service, err)
	}
	existing, err := l.api.ListLoadBalancers(ctx, want.prefix, false)
	if err != nil {
		return wrapAPIError("listing load balancers", err)
	}
	if err := checkIPFamily(want, existing); err != nil {
		return err
	}
	byName := map[string]*brapi.LoadBalancer{}
	for i := range existing {
		byName[existing[i].Name] = &existing[i]
	}
	for _, p := range want.ports {
		balancer := byName[want.prefix+p.suffix()]
		if balancer == nil {
			return fmt.Errorf("load balancer for %s port %d does not exist yet", p.protocol, p.port)
		}
		if err := l.syncBackends(ctx, service, balancer, backendTargets(nodes, want.ipFamily, p.nodePort), len(nodes)); err != nil {
			return err
		}
	}
	return nil
}

func (l *loadBalancers) EnsureLoadBalancerDeleted(ctx context.Context, _ string, service *v1.Service) error {
	existing, err := l.api.ListLoadBalancers(ctx, loadBalancerNamePrefix(service), false)
	if err != nil {
		return wrapAPIError("listing load balancers", err)
	}
	for _, balancer := range existing {
		if err := l.api.DeleteLoadBalancer(ctx, balancer.ID); err != nil && !brapi.IsNotFound(err) {
			return wrapAPIError("deleting load balancer "+balancer.Name, err)
		}
		klog.V(2).Infof("service %s/%s: deleted load balancer %s", service.Namespace, service.Name, balancer.Name)
	}
	return nil
}

func (l *loadBalancers) deleteExtras(ctx context.Context, service *v1.Service, live []brapi.LoadBalancer, wanted map[string]bool) error {
	for _, balancer := range live {
		if wanted[balancer.Name] {
			continue
		}
		if err := l.api.DeleteLoadBalancer(ctx, balancer.ID); err != nil && !brapi.IsNotFound(err) {
			return wrapAPIError("deleting load balancer "+balancer.Name, err)
		}
		klog.V(2).Infof("service %s/%s: deleted load balancer %s (port removed)", service.Namespace, service.Name, balancer.Name)
	}
	return nil
}

// failClosed returns err from parsing the Service. When the Service asks for access restrictions
// the platform cannot enforce, its existing load balancers are disabled as well, so it is not left
// open to everyone; they are enabled again once the restriction is removed.
func (l *loadBalancers) failClosed(ctx context.Context, service *v1.Service, err error) error {
	if !errors.Is(err, errSourceRanges) {
		return err
	}
	existing, listErr := l.api.ListLoadBalancers(ctx, loadBalancerNamePrefix(service), false)
	if listErr != nil {
		return wrapAPIError("listing load balancers", listErr)
	}
	disabled := false
	for _, balancer := range existing {
		if !balancer.Enabled {
			continue
		}
		if _, updateErr := l.api.UpdateLoadBalancer(ctx, balancer.ID, brapi.UpdateRequest{Enabled: &disabled}); updateErr != nil {
			return wrapAPIError(fmt.Sprintf("loadBalancerSourceRanges is not supported, and disabling load balancer "+
				"%s failed, so the Service is still open to everyone; retrying", balancer.Name), updateErr)
		}
		klog.V(2).Infof("service %s/%s: disabled load balancer %s", service.Namespace, service.Name, balancer.Name)
	}
	return err
}

// checkIPFamily fails when the Service's IP family differs from its existing load balancers':
// the family of a platform load balancer cannot change.
func checkIPFamily(want *desired, live []brapi.LoadBalancer) error {
	for _, balancer := range live {
		if balancer.IPFamily != "" && balancer.IPFamily != want.ipFamily {
			return fmt.Errorf("the Service's IP family is %s, but its load balancers are %s; the IP family "+
				"cannot change, so delete and recreate the Service to change it", want.ipFamily, balancer.IPFamily)
		}
	}
	return nil
}

// syncBackends makes the load balancer's backends equal targets, changing nothing (and making
// no write call) when they already match.
func (l *loadBalancers) syncBackends(ctx context.Context, service *v1.Service, balancer *brapi.LoadBalancer, targets []brapi.BackendTarget, nodeCount int) error {
	if len(targets) == 0 && nodeCount > 0 {
		// Never empty a load balancer because of an address-family mismatch or missing node IPs.
		return fmt.Errorf("none of the %d node(s) has an InternalIP of the load balancer's IP family", nodeCount)
	}
	wanted := map[brapi.BackendTarget]bool{}
	for _, target := range targets {
		wanted[target] = true
	}
	have := map[brapi.BackendTarget]bool{}
	for _, backend := range balancer.Backends {
		if backend.IP() != "" {
			have[brapi.BackendTarget{InternalIP: backend.IP(), Port: backend.Port}] = true
		}
	}
	if sameSet(wanted, have) {
		return nil
	}
	wanted = l.eligibleOnly(ctx, service, balancer, wanted)
	if len(wanted) == 0 && len(targets) > 0 {
		// Never report success with no backends, nor empty a serving load balancer because the
		// platform briefly reports no node as eligible: fail and keep the current backends.
		return fmt.Errorf("none of the node(s) %s is one of your active rentals in the load balancer's "+
			"network cluster; node InternalIPs must equal the rentals' internal IPs", describeTargets(targets))
	}
	if sameSet(wanted, have) {
		return nil
	}
	_, err := l.api.SetBackends(ctx, balancer.ID, sortedTargets(wanted))
	if err == nil {
		klog.V(2).Infof("service %s/%s: load balancer %s now has %d backend(s)", service.Namespace, service.Name,
			balancer.Name, len(wanted))
		return nil
	}
	var apiErr *brapi.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		return wrapAPIError("setting backends of "+balancer.Name, err)
	}
	// The platform rejects the whole set when any entry is not one of the organization's active
	// rentals in this network cluster (for example a control-plane or foreign node). Fall back
	// to per-backend changes so the eligible nodes are still served.
	klog.Warningf("setting backends of %s failed (%v); reconciling one by one", balancer.Name, err)
	for target := range have {
		if wanted[target] {
			continue
		}
		if err := l.api.RemoveBackend(ctx, balancer.ID, target); err != nil {
			return wrapAPIError("removing backend from "+balancer.Name, err)
		}
		klog.V(2).Infof("service %s/%s: removed backend %s:%d from load balancer %s", service.Namespace,
			service.Name, target.InternalIP, target.Port, balancer.Name)
	}
	served := 0
	var skipped []string
	for _, target := range sortedTargets(wanted) {
		if have[target] {
			served++
			continue
		}
		if err := l.api.AddBackend(ctx, balancer.ID, target); err != nil {
			if throttle, ok := brapi.IsThrottled(err); ok {
				return api.NewRetryError(err.Error(), throttle)
			}
			skipped = append(skipped, fmt.Sprintf("%s (%v)", target.InternalIP, err))
			continue
		}
		klog.V(2).Infof("service %s/%s: added backend %s:%d to load balancer %s", service.Namespace,
			service.Name, target.InternalIP, target.Port, balancer.Name)
		served++
	}
	if served == 0 && len(wanted) > 0 {
		return fmt.Errorf("none of the %d candidate node(s) could be added to %s", len(wanted), balancer.Name)
	}
	if len(skipped) > 0 {
		l.warn(service, reasonNodesSkipped, fmt.Sprintf("port %s/%d is not served by node(s) %s",
			balancer.Protocol, balancer.FrontendPort, strings.Join(skipped, ", ")))
	}
	return nil
}

// eligibleOnly drops the targets whose node is not one of the organization's eligible rentals in
// the load balancer's network cluster (a control-plane or foreign node), which the platform would
// otherwise answer by rejecting the whole set. It does nothing when the platform does not report
// internal IPs or the lookup fails; the per-backend fallback then still applies.
func (l *loadBalancers) eligibleOnly(ctx context.Context, service *v1.Service, balancer *brapi.LoadBalancer, wanted map[brapi.BackendTarget]bool) map[brapi.BackendTarget]bool {
	rentals, err := l.api.ListEligibleRentals(ctx, balancer.ID)
	if err != nil {
		klog.Warningf("listing eligible rentals of %s failed (%v); not pre-filtering nodes", balancer.Name, err)
		return wanted
	}
	known := map[string]bool{}
	for _, rental := range rentals {
		if rental.IP() != "" {
			known[rental.IP()] = true
		}
	}
	if len(known) == 0 {
		return wanted
	}
	filtered := map[brapi.BackendTarget]bool{}
	var skipped []string
	for target := range wanted {
		if known[target.InternalIP] {
			filtered[target] = true
		} else {
			skipped = append(skipped, target.InternalIP)
		}
	}
	if len(skipped) > 0 && len(filtered) > 0 {
		// With no eligible node at all the Service fails instead, with its own message.
		sort.Strings(skipped)
		l.warn(service, reasonNodesSkipped, fmt.Sprintf("node(s) %s are not served: their InternalIP is not "+
			"the internal IP of one of your active rentals in the load balancer's network cluster",
			strings.Join(skipped, ", ")))
	}
	return filtered
}

// parse derives the platform settings from the Service. It does not call the API; the network
// cluster is resolved later, by networkClusterFor.
func (l *loadBalancers) parse(service *v1.Service, clusterName string) (*desired, error) {
	if service.UID == "" {
		return nil, fmt.Errorf("service %s/%s has no UID", service.Namespace, service.Name)
	}
	// Checked first: this rejection also disables the Service's load balancers (failClosed), so no
	// other problem with the Service may hide it.
	if len(service.Spec.LoadBalancerSourceRanges) > 0 ||
		strings.TrimSpace(service.Annotations[v1.AnnotationLoadBalancerSourceRangesKey]) != "" {
		ranges, err := servicehelpers.GetLoadBalancerSourceRanges(service)
		if err != nil {
			return nil, fmt.Errorf("invalid load balancer source ranges: %w", err)
		}
		if !allowsAll(ranges, requestedIPFamily(service)) {
			return nil, errSourceRanges
		}
	}
	if service.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyLocal {
		return nil, fmt.Errorf("externalTrafficPolicy Local is not supported: the platform health check " +
			"cannot target the Service healthCheckNodePort; use Cluster")
	}
	want := &desired{
		prefix:           loadBalancerNamePrefix(service),
		description:      describeService(service, clusterName),
		timeoutMS:        defaultTimeoutMS,
		path:             "/",
		networkClusterID: service.Annotations[AnnotationNetworkCluster],
	}
	want.ipFamily = requestedIPFamily(service)
	if len(service.Spec.IPFamilies) > 1 {
		want.warnings = append(want.warnings, fmt.Sprintf("the Service is dual-stack, but only its first IP "+
			"family (%s) gets a load balancer address", service.Spec.IPFamilies[0]))
	}
	if value := service.Annotations[AnnotationIPFamily]; value != "" && value != "ipv4" && value != "ipv6" {
		return nil, fmt.Errorf("annotation %s must be ipv4 or ipv6", AnnotationIPFamily)
	}
	if value := service.Annotations[AnnotationHealthTimeout]; value != "" {
		timeout, err := strconv.Atoi(value)
		if err != nil || timeout < 501 || timeout > 60000 {
			return nil, fmt.Errorf("annotation %s must be between 501 and 60000", AnnotationHealthTimeout)
		}
		want.timeoutMS = timeout
	}
	if value := service.Annotations[AnnotationHealthPath]; value != "" {
		if !validHealthPath(value) {
			return nil, fmt.Errorf("annotation %s must start with / and be at most %d characters with no spaces",
				AnnotationHealthPath, healthPathLimit)
		}
		want.path = value
	}
	override := service.Annotations[AnnotationHealthcheck]
	switch override {
	case "", brapi.HealthcheckNone, brapi.HealthcheckTCP, brapi.HealthcheckHTTP:
	default:
		return nil, fmt.Errorf("annotation %s must be none, tcp or http", AnnotationHealthcheck)
	}
	want.healthcheckType = func(protocol string) string {
		// The platform only health-checks TCP load balancers.
		if protocol != "tcp" {
			return brapi.HealthcheckNone
		}
		if override == "" {
			return brapi.HealthcheckTCP
		}
		return override
	}
	if service.Spec.LoadBalancerIP != "" {
		ip := net.ParseIP(service.Spec.LoadBalancerIP)
		if ip == nil {
			return nil, fmt.Errorf("spec.loadBalancerIP %q is not an IP address", service.Spec.LoadBalancerIP)
		}
		want.requestedIP = ip.String()
	}
	seen := map[string]bool{}
	for _, servicePort := range service.Spec.Ports {
		var protocol string
		switch servicePort.Protocol {
		case v1.ProtocolTCP, "":
			protocol = "tcp"
		case v1.ProtocolUDP:
			protocol = "udp"
		default:
			return nil, fmt.Errorf("protocol %s is not supported (tcp and udp only)", servicePort.Protocol)
		}
		if servicePort.NodePort == 0 {
			return nil, fmt.Errorf("port %d has no nodePort; set allocateLoadBalancerNodePorts: true", servicePort.Port)
		}
		spec := portSpec{protocol: protocol, port: int(servicePort.Port), nodePort: int(servicePort.NodePort)}
		if seen[spec.suffix()] {
			return nil, fmt.Errorf("duplicate port %s", spec.suffix())
		}
		seen[spec.suffix()] = true
		want.ports = append(want.ports, spec)
	}
	if len(want.ports) == 0 {
		return nil, fmt.Errorf("service has no ports")
	}
	sort.Slice(want.ports, func(i, j int) bool { return want.ports[i].suffix() < want.ports[j].suffix() })
	return want, nil
}

func (w *desired) createRequest(p portSpec, frontendIP string) brapi.CreateRequest {
	request := brapi.CreateRequest{
		Name:             w.prefix + p.suffix(),
		Description:      w.description,
		NetworkClusterID: w.networkClusterID,
		IPFamily:         w.ipFamily,
		Protocol:         p.protocol,
		FrontendPort:     p.port,
		FrontendIP:       frontendIP,
		HealthcheckType:  w.healthcheckType(p.protocol),
	}
	if request.HealthcheckType != brapi.HealthcheckNone {
		request.HealthcheckTimeoutMS = w.timeoutMS
		request.HealthcheckRequestPath = w.path
	}
	return request
}

// settingsChange returns the PATCH needed to bring an existing load balancer's mutable settings in line.
func (w *desired) settingsChange(balancer *brapi.LoadBalancer, p portSpec) (brapi.UpdateRequest, bool) {
	var request brapi.UpdateRequest
	changed := false
	wantType := w.healthcheckType(p.protocol)
	if balancer.HealthcheckType != wantType {
		request.HealthcheckType = &wantType
		changed = true
	}
	if wantType != brapi.HealthcheckNone {
		if balancer.HealthcheckTimeoutMS != w.timeoutMS {
			timeout := w.timeoutMS
			request.HealthcheckTimeoutMS = &timeout
			changed = true
		}
		if wantType == brapi.HealthcheckHTTP && balancer.HealthcheckRequestPath != w.path {
			path := w.path
			request.HealthcheckRequestPath = &path
			changed = true
		}
	}
	if !balancer.Enabled {
		enabled := true
		request.Enabled = &enabled
		changed = true
	}
	if balancer.Description != w.description {
		description := w.description
		request.Description = &description
		changed = true
	}
	return request, changed
}

// idempotencyKey is stable for a given request body, so a retry after a lost response replays
// the original, while a changed body (for example the port re-added with a shared frontend_ip)
// gets a fresh key instead of a 422 for the rest of the 24 h replay window.
func idempotencyKey(service *v1.Service, p portSpec, request brapi.CreateRequest) string {
	payload, _ := json.Marshal(request)
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("k8s-%s-%s-%x", service.UID, p.suffix(), sum[:6])
}

// networkClusterFor picks the network cluster new load balancers of the Service are created in.
// The Service's existing load balancers fix it (it cannot change); otherwise the annotation, then
// the configured default, then the organization's only eligible network cluster.
func (l *loadBalancers) networkClusterFor(ctx context.Context, service *v1.Service, annotated string, live []brapi.LoadBalancer) (string, error) {
	for _, balancer := range live {
		if balancer.NetworkClusterID == "" {
			continue
		}
		if annotated != "" && annotated != balancer.NetworkClusterID {
			return "", fmt.Errorf("annotation %s is %s, but the Service's load balancers are in network cluster %s; "+
				"the network cluster cannot change, so delete and recreate the Service to move it",
				AnnotationNetworkCluster, annotated, balancer.NetworkClusterID)
		}
		return balancer.NetworkClusterID, nil
	}
	if annotated != "" {
		return annotated, nil
	}
	if l.defaultNetworkClusterID != "" {
		return l.defaultNetworkClusterID, nil
	}
	// Every eligible cluster counts, whether or not it has free addresses right now: the platform's
	// ipv4_available/ipv6_available describe current capacity, so choosing by them would make the
	// automatic choice change as address pools fill and empty.
	clusters, err := l.eligibleNetworkClusters(ctx)
	if err != nil {
		return "", wrapAPIError("listing eligible network clusters", err)
	}
	switch len(clusters) {
	case 1:
		return clusters[0].ID, nil
	case 0:
		return "", errors.New("your organization has no network cluster that can host load balancers")
	}
	options := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		options = append(options, fmt.Sprintf("%s (%s)", cluster.ID, cluster.Name))
	}
	sort.Strings(options)
	return "", fmt.Errorf("your organization has several network clusters (%s); choose one with the %s "+
		"annotation, or set the controller's default (Helm value networkClusterID, or %s)",
		strings.Join(options, ", "), AnnotationNetworkCluster, EnvNetworkClusterID)
}

func (l *loadBalancers) eligibleNetworkClusters(ctx context.Context) ([]brapi.NetworkCluster, error) {
	l.clustersMu.Lock()
	defer l.clustersMu.Unlock()
	if l.clusters != nil && l.now().Sub(l.clustersFetched) < networkClusterTTL {
		return l.clusters, nil
	}
	clusters, err := l.api.ListEligibleNetworkClusters(ctx)
	if err != nil {
		return nil, err
	}
	// An empty list is not cached, so a newly added first cluster is picked up at once.
	if len(clusters) > 0 {
		l.clusters, l.clustersFetched = clusters, l.now()
	}
	return clusters, nil
}

// describeService is the load balancer description shown in the platform: it names the Service
// and the Kubernetes cluster (--cluster-name), so an organization running several clusters can
// tell their load balancers apart.
func describeService(service *v1.Service, clusterName string) string {
	description := fmt.Sprintf("Kubernetes Service %s/%s", service.Namespace, service.Name)
	if clusterName != "" {
		description += " in cluster " + clusterName
	}
	return truncate(description, descriptionLimit)
}

// allowsAll reports whether the ranges admit every client of the load balancer's IP family, that
// is, they include 0.0.0.0/0 (ipv4) or ::/0 (ipv6).
func allowsAll(ranges utilnet.IPNetSet, ipFamily string) bool {
	for _, network := range ranges {
		ones, _ := network.Mask.Size()
		if ones == 0 && (network.IP.To4() != nil) == (ipFamily == "ipv4") {
			return true
		}
	}
	return false
}

// requestedIPFamily is the IP family the Service's load balancers use: the annotation when it is
// valid (parse rejects an invalid one later), else the Service's first family, else ipv4.
func requestedIPFamily(service *v1.Service) string {
	if value := service.Annotations[AnnotationIPFamily]; value == "ipv4" || value == "ipv6" {
		return value
	}
	if len(service.Spec.IPFamilies) > 0 && service.Spec.IPFamilies[0] == v1.IPv6Protocol {
		return "ipv6"
	}
	return "ipv4"
}

func validHealthPath(path string) bool {
	if !strings.HasPrefix(path, "/") || len(path) > healthPathLimit {
		return false
	}
	for _, r := range path {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// backendTargets maps nodes to platform backends: the node's InternalIP of the load
// balancer's family, on the Service nodePort. Nodes without such an address are skipped.
func backendTargets(nodes []*v1.Node, ipFamily string, nodePort int) []brapi.BackendTarget {
	var targets []brapi.BackendTarget
	for _, node := range nodes {
		for _, address := range node.Status.Addresses {
			if address.Type != v1.NodeInternalIP {
				continue
			}
			ip := net.ParseIP(address.Address)
			if ip == nil || (ip.To4() != nil) != (ipFamily == "ipv4") {
				continue
			}
			targets = append(targets, brapi.BackendTarget{InternalIP: ip.String(), Port: nodePort})
			break
		}
	}
	return targets
}

func sortedTargets(set map[brapi.BackendTarget]bool) []brapi.BackendTarget {
	targets := make([]brapi.BackendTarget, 0, len(set))
	for target := range set {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].InternalIP != targets[j].InternalIP {
			return targets[i].InternalIP < targets[j].InternalIP
		}
		return targets[i].Port < targets[j].Port
	})
	return targets
}

// describeTargets lists the targets' IPs for an error message, at most five.
func describeTargets(targets []brapi.BackendTarget) string {
	const limit = 5
	ips := make([]string, 0, limit)
	for _, target := range targets {
		if len(ips) == limit {
			return strings.Join(ips, ", ") + fmt.Sprintf(" and %d more", len(targets)-limit)
		}
		ips = append(ips, target.InternalIP)
	}
	return strings.Join(ips, ", ")
}

func sameSet(a, b map[brapi.BackendTarget]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func splitDeleting(all []brapi.LoadBalancer) (live, deleting []brapi.LoadBalancer) {
	for _, balancer := range all {
		if balancer.Status == brapi.StatusDeleting {
			deleting = append(deleting, balancer)
		} else {
			live = append(live, balancer)
		}
	}
	return live, deleting
}

func firstFrontendIP(balancers []brapi.LoadBalancer) string {
	sorted := append([]brapi.LoadBalancer(nil), balancers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, balancer := range sorted {
		if balancer.IP() != "" {
			return balancer.IP()
		}
	}
	return ""
}

func anyCreatedWithoutIP(byName map[string]*brapi.LoadBalancer, want *desired) bool {
	for _, p := range want.ports {
		if balancer := byName[want.prefix+p.suffix()]; balancer != nil && balancer.IP() == "" {
			return true
		}
	}
	return false
}

func statusFor(balancers []brapi.LoadBalancer) *v1.LoadBalancerStatus {
	return &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: firstFrontendIP(balancers)}}}
}

// portInUse reports the platform's 400 for a frontend port still held by a deleting load balancer.
func portInUse(err error) bool {
	var apiErr *brapi.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		return false
	}
	for _, message := range apiErr.Messages {
		if strings.Contains(message, "already used on this address") {
			return true
		}
	}
	return false
}

// wrapAPIError turns throttling (429) and in-progress (409) responses into a fixed-delay retry
// and leaves every other error to the controller's exponential backoff.
func wrapAPIError(action string, err error) error {
	if wait, ok := brapi.IsThrottled(err); ok {
		return api.NewRetryError(fmt.Sprintf("%s: %v", action, err), wait)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// truncate shortens text to at most limit bytes without splitting a UTF-8 character.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
