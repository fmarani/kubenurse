// Package servicecheck implements the checks the kubenurse performs.
package servicecheck

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	okStr            = "ok"
	errStr           = "error"
	skippedStr       = "skipped"
	MetricsNamespace = "kubenurse"
)

// New configures the checker with a httpClient and a cache timeout for check
// results. Other parameters of the Checker struct need to be configured separately.
func New(_ context.Context, cl client.Client, promRegistry *prometheus.Registry,
	allowUnschedulable bool, cacheTTL time.Duration, durationHistogramBuckets []float64) (*Checker, error) {
	// setup http transport
	tlsConfig, err := generateTLSConfig(os.Getenv("KUBENURSE_EXTRA_CA"))
	if err != nil {
		slog.Error("cannot generate tlsConfig with provided KUBENURSE_EXTRA_CA. Continuing with default tlsConfig",
			"KUBENURSE_EXTRA_CA", os.Getenv("KUBENURSE_EXTRA_CA"), "err", err)

		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	tlsConfig.InsecureSkipVerify = os.Getenv("KUBENURSE_INSECURE") == "true"
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     os.Getenv("KUBENURSE_REUSE_CONNECTIONS") != "true",
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: withHttptrace(promRegistry, transport, durationHistogramBuckets),
	}

	return &Checker{
		allowUnschedulable: allowUnschedulable,
		client:             cl,
		httpClient:         httpClient,
		cacheTTL:           cacheTTL,
		stop:               make(chan struct{}),
	}, nil
}

// Run runs all servicechecks and returns the result togeter with a boolean which indicates success. The cache
// is respected.
func (c *Checker) Run() {
	// Run Checks
	result := sync.Map{}

	wg := sync.WaitGroup{}

	// Cache result (used for /alive handler)
	defer func() {
		res := make(map[string]any)

		result.Range(func(key, value any) bool {
			k, _ := key.(string)
			res[k] = value

			return true
		})

		c.LastCheckResult = res
	}()

	wg.Add(4)

	go c.measure(&wg, &result, c.APIServerDirect, APIServerDirect)
	go c.measure(&wg, &result, c.APIServerDNS, APIServerDNS)
	go c.measure(&wg, &result, c.MeIngress, meIngress)
	go c.measure(&wg, &result, c.MeService, meService)

	if c.SkipCheckNeighbourhood {
		result.Store(NeighbourhoodState, skippedStr)
		return
	}

	neighbours, err := c.getNeighbours(context.Background(), c.KubenurseNamespace, c.NeighbourFilter)
	if err != nil {
		result.Store(NeighbourhoodState, err.Error())
		return
	}

	result.Store(NeighbourhoodState, okStr)
	result.Store(Neighbourhood, neighbours)

	if c.NeighbourLimit > 0 && len(neighbours) > c.NeighbourLimit {
		neighbours = c.filterNeighbours(neighbours)
	}

	wg.Add((len(neighbours)))

	for _, neighbour := range neighbours {
		check := func(ctx context.Context) string {
			return c.doRequest(ctx, podIPtoURL(neighbour.PodIP, c.UseTLS), true)
		}

		go c.measure(&wg, &result, check, "path_"+neighbour.NodeName)
	}

	wg.Wait()
}

// RunScheduled runs the checks in the specified interval which can be used to keep the metrics up-to-date. This
// function does not return until StopScheduled is called.
func (c *Checker) RunScheduled(d time.Duration) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.Run()
		case <-c.stop:
			return
		}
	}
}

// StopScheduled is used to stop the scheduled run of checks.
func (c *Checker) StopScheduled() {
	close(c.stop)
}

// APIServerDirect checks the /version endpoint of the Kubernetes API Server through the direct link
func (c *Checker) APIServerDirect(ctx context.Context) string {
	if c.SkipCheckAPIServerDirect {
		return skippedStr
	}

	apiurl := fmt.Sprintf("https://%s:%s/version", c.KubernetesServiceHost, c.KubernetesServicePort)

	return c.doRequest(ctx, apiurl, false)
}

// APIServerDNS checks the /version endpoint of the Kubernetes API Server through the Cluster DNS URL
func (c *Checker) APIServerDNS(ctx context.Context) string {
	if c.SkipCheckAPIServerDNS {
		return skippedStr
	}

	apiurl := fmt.Sprintf("https://kubernetes.default.svc.cluster.local:%s/version", c.KubernetesServicePort)

	return c.doRequest(ctx, apiurl, false)
}

// MeIngress checks if the kubenurse is reachable at the /alwayshappy endpoint behind the ingress
func (c *Checker) MeIngress(ctx context.Context) string {
	if c.SkipCheckMeIngress {
		return skippedStr
	}

	return c.doRequest(ctx, c.KubenurseIngressURL+"/alwayshappy", false) //nolint:goconst // readability
}

// MeService checks if the kubenurse is reachable at the /alwayshappy endpoint through the kubernetes service
func (c *Checker) MeService(ctx context.Context) string {
	if c.SkipCheckMeService {
		return skippedStr
	}

	return c.doRequest(ctx, c.KubenurseServiceURL+"/alwayshappy", false)
}

// measure implements metric collections for the check
func (c *Checker) measure(wg *sync.WaitGroup, res *sync.Map, check Check, requestType string) {
	// Add our label (check type) to the context so our http tracer can annotate
	// metrics and errors based with the label
	defer wg.Done()

	ctx := context.WithValue(context.Background(), kubenurseTypeKey{}, requestType)
	res.Store(requestType, check(ctx))
}

func podIPtoURL(podIP string, useTLS bool) string {
	if useTLS {
		return "https://" + podIP + ":8443/alwayshappy"
	}

	return "http://" + podIP + ":8080/alwayshappy"
}
