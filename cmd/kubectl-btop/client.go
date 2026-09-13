package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	metricsv1beta2 "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	"k8s.io/metrics/pkg/client/custom_metrics"
)

// cancelTransport binds each HTTP round trip to the context of the
// invocation that triggered it. The custom-metrics client's own methods
// accept no context and use context.TODO() internally (design.md §11.1), so
// cancellation has to happen at the transport layer instead. bind/unbind
// wrap one round trip at a time; Client never issues concurrent calls
// through the same transport, so a single held context is sufficient.
type cancelTransport struct {
	base http.RoundTripper

	mu  sync.Mutex
	ctx context.Context
}

func (t *cancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	ctx := t.ctx
	t.mu.Unlock()

	if ctx == nil {
		return t.base.RoundTrip(req)
	}

	return t.base.RoundTrip(req.WithContext(ctx))
}

func (t *cancelTransport) bind(ctx context.Context) (unbind func()) {
	t.mu.Lock()
	t.ctx = ctx
	t.mu.Unlock()

	return func() {
		t.mu.Lock()
		t.ctx = nil
		t.mu.Unlock()
	}
}

// Client is btop's thin lifecycle adapter around the standard custom-metrics
// client (design.md §11.1 and §11.2's "this local lifecycle adapter is not a
// competing public metrics client"). It owns kubeconfig loading, the
// effective default namespace, and per-call cancellation.
type Client struct {
	metrics   custom_metrics.CustomMetricsClient
	transport *cancelTransport
	namespace string
}

// NewClient resolves kubeconfig/context configuration and builds the
// custom-metrics client. KUBECONFIG path-list merging and per-context
// namespace defaulting stay owned by client-go (design.md §5.2); o.Kubeconfig
// only becomes an ExplicitPath override when the flag was actually set.
func NewClient(o *Options) (*Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		loadingRules.ExplicitPath = o.Kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if o.Context != "" {
		overrides.CurrentContext = o.Context
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	restConfig.Timeout = o.RequestTimeout

	transport := &cancelTransport{}
	restConfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		transport.base = rt

		return transport
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cacheddiscovery.NewMemCacheClient(discoveryClient))
	availableAPIs := custom_metrics.NewAvailableAPIsGetter(discoveryClient)

	namespace := o.Namespace
	if namespace == "" {
		if ns, _, err := clientConfig.Namespace(); err == nil {
			namespace = ns
		}
	}

	return &Client{
		metrics:   custom_metrics.NewForConfig(restConfig, mapper, availableAPIs),
		transport: transport,
		namespace: namespace,
	}, nil
}

// Namespace returns the effective default namespace: an explicit
// --namespace, or else the current kubeconfig context's namespace.
func (c *Client) Namespace() string {
	return c.namespace
}

// GetForObject fetches metric for a single named object. namespace == ""
// selects the root-scoped getter (Nodes); metric selectors are always empty
// since v1 rejects nonempty metric selectors (metric-gateway.md §3.6).
func (c *Client) GetForObject(ctx context.Context, gk schema.GroupKind, namespace, name, metric string) (*metricsv1beta2.MetricValue, error) {
	unbind := c.transport.bind(ctx)
	defer unbind()

	if namespace == "" {
		return c.metrics.RootScopedMetrics().GetForObject(gk, name, metric, labels.Everything())
	}

	return c.metrics.NamespacedMetrics(namespace).GetForObject(gk, name, metric, labels.Everything())
}

// GetForObjects fetches metric for every object matching selector.
func (c *Client) GetForObjects(ctx context.Context, gk schema.GroupKind, namespace string, selector labels.Selector, metric string) (*metricsv1beta2.MetricValueList, error) {
	unbind := c.transport.bind(ctx)
	defer unbind()

	if namespace == "" {
		return c.metrics.RootScopedMetrics().GetForObjects(gk, selector, metric, labels.Everything())
	}

	return c.metrics.NamespacedMetrics(namespace).GetForObjects(gk, selector, metric, labels.Everything())
}
