// Package provider implements the Kubernetes cloud-provider interface on top of the BoostRun
// platform load balancer API. Only the LoadBalancer interface is implemented; node lifecycle,
// zones and routes are left to the cluster's existing tooling.
package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	"github.com/boostrun/boostrun-cloud-controller-manager/internal/brapi"
	"github.com/boostrun/boostrun-cloud-controller-manager/internal/version"
)

// ProviderName is the value of --cloud-provider.
const ProviderName = "boostrun"

// DefaultAPIURL is the platform API used when none is configured.
const DefaultAPIURL = "https://platform.boostrun.com/api/v1"

// Environment variables. Each overrides the matching field of the --cloud-config file.
const (
	EnvAPIURL           = "BOOSTRUN_API_URL"
	EnvAPIKey           = "BOOSTRUN_API_KEY"
	EnvAPIKeyFile       = "BOOSTRUN_API_KEY_FILE"
	EnvNetworkClusterID = "BOOSTRUN_NETWORK_CLUSTER_ID"
)

// Config is the provider configuration. Every field can be set in the optional JSON config
// file (--cloud-config) and is overridden by the matching environment variable.
type Config struct {
	// APIURL is the platform API base including the version (BOOSTRUN_API_URL). Defaults to
	// DefaultAPIURL.
	APIURL string `json:"apiURL"`
	// APIKey is the organization API key, sent as X-API-Key (BOOSTRUN_API_KEY).
	APIKey string `json:"apiKey"`
	// APIKeyFile is a file holding the API key (BOOSTRUN_API_KEY_FILE), typically a mounted
	// Secret. It is read on every request, so a rotated key takes effect without a restart.
	APIKeyFile string `json:"apiKeyFile"`
	// NetworkClusterID is the network cluster new load balancers are created in
	// (BOOSTRUN_NETWORK_CLUSTER_ID). Optional when the organization has a single eligible one.
	NetworkClusterID string `json:"networkClusterID"`
}

func loadConfig(reader io.Reader) (Config, error) {
	var config Config
	if reader != nil {
		if err := json.NewDecoder(reader).Decode(&config); err != nil && !errors.Is(err, io.EOF) {
			return config, fmt.Errorf("reading cloud config: %w", err)
		}
	}
	// A key from the environment replaces the file's key setting entirely, so the environment can
	// switch between a key and a key file.
	if strings.TrimSpace(os.Getenv(EnvAPIKey)) != "" || strings.TrimSpace(os.Getenv(EnvAPIKeyFile)) != "" {
		config.APIKey, config.APIKeyFile = "", ""
	}
	for env, target := range map[string]*string{
		EnvAPIURL:           &config.APIURL,
		EnvAPIKey:           &config.APIKey,
		EnvAPIKeyFile:       &config.APIKeyFile,
		EnvNetworkClusterID: &config.NetworkClusterID,
	} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			*target = value
		}
	}
	config.APIURL = strings.TrimSpace(config.APIURL)
	if config.APIURL == "" {
		config.APIURL = DefaultAPIURL
	}
	parsed, err := url.Parse(config.APIURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return config, fmt.Errorf("platform API URL %q is not an http(s) URL (%s)", config.APIURL, EnvAPIURL)
	}
	switch {
	case config.APIKey != "" && config.APIKeyFile != "":
		return config, fmt.Errorf("set only one of %s and %s", EnvAPIKey, EnvAPIKeyFile)
	case config.APIKey == "" && config.APIKeyFile == "":
		return config, fmt.Errorf("platform API key is not set (%s or %s)", EnvAPIKeyFile, EnvAPIKey)
	}
	return config, nil
}

func (c Config) keySource() brapi.KeySource {
	if c.APIKeyFile != "" {
		return brapi.FileKey(c.APIKeyFile)
	}
	return brapi.StaticKey(c.APIKey)
}

// Cloud is the cloudprovider.Interface implementation.
type Cloud struct {
	lb *loadBalancers
}

var _ cloudprovider.Interface = (*Cloud)(nil)

func init() {
	cloudprovider.RegisterCloudProvider(ProviderName, func(config io.Reader) (cloudprovider.Interface, error) {
		parsed, err := loadConfig(config)
		if err != nil {
			return nil, err
		}
		if parsed.APIKeyFile != "" {
			if _, err := parsed.keySource()(); err != nil {
				return nil, err
			}
		}
		klog.Infof("%s: platform API %s", version.UserAgent(), parsed.APIURL)
		client := brapi.NewClient(parsed.APIURL, parsed.keySource(), version.UserAgent(), nil)
		return New(client, parsed.NetworkClusterID), nil
	})
}

// New builds the provider around an API implementation.
func New(api API, defaultNetworkClusterID string) *Cloud {
	return &Cloud{lb: newLoadBalancers(api, defaultNetworkClusterID)}
}

// Initialize starts the event recorder that reports problems on the affected Service, where
// `kubectl describe service` shows them.
func (c *Cloud) Initialize(builder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	client, err := builder.Client("boostrun-cloud-controller-manager")
	if err != nil {
		klog.Errorf("creating the event client: %v; problems are logged only", err)
		return
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedv1.EventSinkImpl{Interface: client.CoreV1().Events("")})
	c.lb.recorder = broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "boostrun-cloud-controller-manager"})
	go func() {
		<-stop
		broadcaster.Shutdown()
	}()
}

func (c *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) { return c.lb, true }

func (c *Cloud) Instances() (cloudprovider.Instances, bool) { return nil, false }

func (c *Cloud) InstancesV2() (cloudprovider.InstancesV2, bool) { return nil, false }

func (c *Cloud) Zones() (cloudprovider.Zones, bool) { return nil, false }

func (c *Cloud) Clusters() (cloudprovider.Clusters, bool) { return nil, false }

func (c *Cloud) Routes() (cloudprovider.Routes, bool) { return nil, false }

func (c *Cloud) ProviderName() string { return ProviderName }

func (c *Cloud) HasClusterID() bool { return true }
