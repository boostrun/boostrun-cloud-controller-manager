// Package brapi is a minimal client for the BoostRun platform load balancer API.
package brapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	StatusActive   = "active"
	StatusError    = "error"
	StatusDeleting = "deleting"

	HealthcheckNone = "none"
	HealthcheckTCP  = "tcp"
	HealthcheckHTTP = "http"
)

// Backend is one server port behind a load balancer.
type Backend struct {
	BackendID  string  `json:"backend_id"`
	RentalID   string  `json:"rental_id"`
	InternalIP *string `json:"internal_ip"`
	Port       int     `json:"port"`
	Status     string  `json:"status"`
}

// IP returns the backend's internal IP, or "".
func (b Backend) IP() string {
	if b.InternalIP == nil {
		return ""
	}
	return *b.InternalIP
}

// LoadBalancer mirrors the platform's load balancer resource (fields the adapter uses).
type LoadBalancer struct {
	ID                     string  `json:"load_balancer_id"`
	Name                   string  `json:"name"`
	Description            string  `json:"description"`
	Status                 string  `json:"status"`
	FrontendIP             *string `json:"frontend_ip"`
	FrontendPort           int     `json:"frontend_port"`
	Protocol               string  `json:"protocol"`
	IPFamily               string  `json:"ip_family"`
	Enabled                bool    `json:"enabled"`
	HealthcheckType        string  `json:"healthcheck_type"`
	HealthcheckTimeoutMS   int     `json:"healthcheck_timeout_ms"`
	HealthcheckRequestPath string  `json:"healthcheck_request_path"`
	NetworkClusterID       string  `json:"network_cluster_id"`
	// RequestedFrontendIP is the specific address asked for at creation; empty on older platforms.
	RequestedFrontendIP *string   `json:"requested_frontend_ip"`
	Backends            []Backend `json:"backends"`
}

// IP returns the frontend IP, or "" while it is not yet allocated.
func (l LoadBalancer) IP() string {
	if l.FrontendIP == nil {
		return ""
	}
	return *l.FrontendIP
}

// NetworkCluster is a network cluster the organization can create load balancers in.
type NetworkCluster struct {
	ID            string `json:"network_cluster_id"`
	Name          string `json:"name"`
	IPv4Available bool   `json:"ipv4_available"`
	IPv6Available bool   `json:"ipv6_available"`
}

// EligibleRental is a rental that can be added as a backend. InternalIP is empty on platforms
// that do not report it yet.
type EligibleRental struct {
	RentalID   string  `json:"rental_id"`
	InternalIP *string `json:"internal_ip"`
}

// IP returns the rental's internal IP, or "".
func (r EligibleRental) IP() string {
	if r.InternalIP == nil {
		return ""
	}
	return *r.InternalIP
}

// BackendTarget identifies a server port by internal IP.
type BackendTarget struct {
	InternalIP string `json:"internal_ip"`
	Port       int    `json:"port"`
}

// CreateRequest is the body of POST /load-balancers.
type CreateRequest struct {
	Name                   string          `json:"name"`
	Description            string          `json:"description,omitempty"`
	NetworkClusterID       string          `json:"network_cluster_id"`
	IPFamily               string          `json:"ip_family"`
	Protocol               string          `json:"protocol"`
	FrontendPort           int             `json:"frontend_port"`
	FrontendIP             string          `json:"frontend_ip,omitempty"`
	HealthcheckType        string          `json:"healthcheck_type,omitempty"`
	HealthcheckTimeoutMS   int             `json:"healthcheck_timeout_ms,omitempty"`
	HealthcheckRequestPath string          `json:"healthcheck_request_path,omitempty"`
	Backends               []BackendTarget `json:"backends,omitempty"`
}

// UpdateRequest is the body of PATCH /load-balancers/{id}; nil fields are left unchanged.
type UpdateRequest struct {
	Description            *string `json:"description,omitempty"`
	Enabled                *bool   `json:"enabled,omitempty"`
	HealthcheckType        *string `json:"healthcheck_type,omitempty"`
	HealthcheckTimeoutMS   *int    `json:"healthcheck_timeout_ms,omitempty"`
	HealthcheckRequestPath *string `json:"healthcheck_request_path,omitempty"`
}

// Error is a non-2xx API response.
type Error struct {
	StatusCode int
	Messages   []string
	// RetryAfter is set for 429 and 409 responses that carry a Retry-After header.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("platform API returned %d: %s", e.StatusCode, strings.Join(e.Messages, "; "))
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsThrottled reports whether err is a 429 or an in-progress 409 and, if so, how long to wait.
func IsThrottled(err error) (time.Duration, bool) {
	var apiErr *Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusTooManyRequests ||
		apiErr.StatusCode == http.StatusConflict) {
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = 15 * time.Second
		}
		return wait, true
	}
	return 0, false
}

// KeySource returns the API key for a request. It is called per request, so a key read from a
// mounted Secret file is picked up after rotation without a restart.
type KeySource func() (string, error)

// StaticKey returns a KeySource for a fixed key.
func StaticKey(key string) KeySource { return func() (string, error) { return key, nil } }

// FileKey returns a KeySource that reads the key from path (surrounding whitespace removed).
func FileKey(path string) KeySource {
	return func() (string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading the API key file: %w", err)
		}
		key := strings.TrimSpace(string(raw))
		if key == "" {
			return "", fmt.Errorf("the API key file %s is empty", path)
		}
		return key, nil
	}
}

// Client talks to the platform API with an X-API-Key.
type Client struct {
	baseURL   string
	key       KeySource
	http      *http.Client
	userAgent string
	// retryDelay is the pause before the one retry of a transient failure.
	retryDelay time.Duration
}

// NewClient returns a client for baseURL (for example https://host/api/v1). userAgent may be empty.
func NewClient(baseURL string, key KeySource, userAgent string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), key: key, http: httpClient,
		userAgent: userAgent, retryDelay: time.Second}
}

type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Errors  []string        `json:"errors"`
	Data    json.RawMessage `json:"data"`
	// Pagination appears only on paginated list responses.
	Pagination *struct {
		Next *string `json:"next"`
	} `json:"pagination"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values,
	headers map[string]string, body, out any) error {
	_, err := c.doEnvelope(ctx, method, path, query, headers, body, out)
	return err
}

// doEnvelope sends one request. A connection error or a 502/503/504 is retried once after a short
// pause, except for a POST without an Idempotency-Key, which could otherwise run twice. Timeouts
// and cancellations are not retried; the controller's own backoff handles them.
func (c *Client) doEnvelope(ctx context.Context, method, path string, query url.Values,
	headers map[string]string, body, out any) (*envelope, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	retryable := method != http.MethodPost || headers["Idempotency-Key"] != ""
	resp, err := c.send(ctx, method, path, query, headers, payload)
	if retryable && transient(resp, err) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.retryDelay):
		}
		resp, err = c.send(ctx, method, path, query, headers, payload)
	}
	if err != nil {
		return nil, err
	}
	return decode(resp, out)
}

// reply is a response whose body has been read and closed.
type reply struct {
	status int
	header http.Header
	body   []byte
}

func transient(resp *reply, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, errKey)
	}
	switch resp.status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// errKey marks a failure to obtain the API key, which retrying cannot fix.
var errKey = errors.New("no API key")

func (c *Client) send(ctx context.Context, method, path string, query url.Values,
	headers map[string]string, payload []byte) (*reply, error) {
	key, err := c.key()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKey, err)
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: raw}, nil
}

func decode(resp *reply, out any) (*envelope, error) {
	var env envelope
	jsonErr := json.Unmarshal(resp.body, &env)
	if resp.status < 200 || resp.status > 299 {
		apiErr := &Error{StatusCode: resp.status, Messages: env.Errors}
		if jsonErr != nil || len(apiErr.Messages) == 0 {
			apiErr.Messages = []string{truncate(string(resp.body), 200)}
		}
		if seconds, convErr := strconv.Atoi(resp.header.Get("Retry-After")); convErr == nil {
			apiErr.RetryAfter = time.Duration(seconds) * time.Second
		}
		return nil, apiErr
	}
	if jsonErr != nil {
		return nil, fmt.Errorf("decoding platform API response: %w", jsonErr)
	}
	if out == nil {
		return &env, nil
	}
	return &env, json.Unmarshal(env.Data, out)
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

// ListLoadBalancers returns the organization's load balancers whose name starts with prefix.
// Load balancers being deleted are included only when includeDeleting is set. Pages are
// followed, so the result is complete whether or not the platform paginates by default.
func (c *Client) ListLoadBalancers(ctx context.Context, namePrefix string, includeDeleting bool) ([]LoadBalancer, error) {
	query := url.Values{}
	query.Set("name_prefix", namePrefix)
	query.Set("page_size", "200")
	if includeDeleting {
		query.Set("include_deleting", "true")
	}
	var all []LoadBalancer
	for page := 0; page < 50; page++ {
		var chunk []LoadBalancer
		env, err := c.doEnvelope(ctx, http.MethodGet, "/load-balancers", query, nil, nil, &chunk)
		if err != nil {
			return nil, err
		}
		all = append(all, chunk...)
		if env.Pagination == nil || env.Pagination.Next == nil || *env.Pagination.Next == "" {
			return all, nil
		}
		next, err := url.Parse(*env.Pagination.Next)
		if err != nil || next.Query().Get("cursor") == "" {
			return nil, fmt.Errorf("unusable pagination link %q", *env.Pagination.Next)
		}
		query.Set("cursor", next.Query().Get("cursor"))
	}
	return nil, errors.New("load balancer list did not terminate")
}

// GetLoadBalancer returns one load balancer; deleting ones are still returned.
func (c *Client) GetLoadBalancer(ctx context.Context, id string) (*LoadBalancer, error) {
	var result LoadBalancer
	if err := c.do(ctx, http.MethodGet, "/load-balancers/"+id, nil, nil, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CreateLoadBalancer creates a load balancer. idempotencyKey makes a retry safe.
func (c *Client) CreateLoadBalancer(ctx context.Context, request CreateRequest, idempotencyKey string) (*LoadBalancer, error) {
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	var result LoadBalancer
	if err := c.do(ctx, http.MethodPost, "/load-balancers", nil, headers, request, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UpdateLoadBalancer changes mutable settings.
func (c *Client) UpdateLoadBalancer(ctx context.Context, id string, request UpdateRequest) (*LoadBalancer, error) {
	var result LoadBalancer
	if err := c.do(ctx, http.MethodPatch, "/load-balancers/"+id, nil, nil, request, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DeleteLoadBalancer requests deletion; deleting twice succeeds.
func (c *Client) DeleteLoadBalancer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/load-balancers/"+id, nil, nil, nil, nil)
}

// ListEligibleNetworkClusters returns the network clusters the organization can create load
// balancers in.
func (c *Client) ListEligibleNetworkClusters(ctx context.Context) ([]NetworkCluster, error) {
	var result []NetworkCluster
	err := c.do(ctx, http.MethodGet, "/load-balancers/eligible-network-clusters", nil, nil, nil, &result)
	return result, err
}

// ListEligibleRentals returns the rentals that can be backends of the load balancer.
func (c *Client) ListEligibleRentals(ctx context.Context, id string) ([]EligibleRental, error) {
	var result []EligibleRental
	err := c.do(ctx, http.MethodGet, "/load-balancers/"+id+"/eligible-rentals", nil, nil, nil, &result)
	return result, err
}

// SetBackends replaces the whole backend set atomically.
func (c *Client) SetBackends(ctx context.Context, id string, targets []BackendTarget) ([]Backend, error) {
	if targets == nil {
		targets = []BackendTarget{}
	}
	body := map[string]any{"backends": targets}
	var result []Backend
	err := c.do(ctx, http.MethodPut, "/load-balancers/"+id+"/backends", nil, nil, body, &result)
	return result, err
}

// AddBackend adds one backend.
func (c *Client) AddBackend(ctx context.Context, id string, target BackendTarget) error {
	return c.do(ctx, http.MethodPost, "/load-balancers/"+id+"/backends", nil, nil, target, nil)
}

// RemoveBackend removes one backend by internal IP and port.
func (c *Client) RemoveBackend(ctx context.Context, id string, target BackendTarget) error {
	query := url.Values{}
	query.Set("internal_ip", target.InternalIP)
	query.Set("port", strconv.Itoa(target.Port))
	err := c.do(ctx, http.MethodDelete, "/load-balancers/"+id+"/backends", query, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
