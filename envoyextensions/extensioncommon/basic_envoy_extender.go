package extensioncommon

import (
	"fmt"
	"strings"

	envoy_cluster_v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoy_listener_v3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	envoy_route_v3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	envoy_tcp_proxy_v3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	envoy_resource_v3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/hashicorp/go-multierror"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/envoyextensions/xdscommon"
)

// BasicExtension is the interface that each user of BasicEnvoyExtender must implement. It
// is responsible for modifying the xDS structures based on only the state of
// the extension.
type BasicExtension interface {
	// CanApply determines if the extension can mutate resources for the given xdscommon.ExtensionConfiguration.
	CanApply(*RuntimeConfig) bool

	// PatchRoute patches a route to include the custom Envoy configuration
	// required to integrate with the built in extension template.
	PatchRoute(*RuntimeConfig, *envoy_route_v3.RouteConfiguration) (*envoy_route_v3.RouteConfiguration, bool, error)

	// PatchCluster patches a cluster to include the custom Envoy configuration
	// required to integrate with the built in extension template.
	PatchCluster(*RuntimeConfig, *envoy_cluster_v3.Cluster) (*envoy_cluster_v3.Cluster, bool, error)

	// PatchFilter patches an Envoy filter to include the custom Envoy
	// configuration required to integrate with the built in extension template.
	PatchFilter(*RuntimeConfig, *envoy_listener_v3.Filter) (*envoy_listener_v3.Filter, bool, error)
}

var _ EnvoyExtender = (*BasicEnvoyExtender)(nil)

// BasicEnvoyExtender provides convenience functions for iterating and applying modifications
// to Envoy resources.
type BasicEnvoyExtender struct {
	Extension BasicExtension
}

func (envoyExtension *BasicEnvoyExtender) Validate(config *RuntimeConfig) error {
	return nil
}

func (envoyExtender *BasicEnvoyExtender) Extend(resources *xdscommon.IndexedResources, config *RuntimeConfig) (*xdscommon.IndexedResources, error) {
	var resultErr error

	switch config.Kind {
	case api.ServiceKindTerminatingGateway, api.ServiceKindConnectProxy:
	default:
		return resources, nil
	}

	if !envoyExtender.Extension.CanApply(config) {
		return resources, nil
	}

	for _, indexType := range []string{
		xdscommon.ListenerType,
		xdscommon.RouteType,
		xdscommon.ClusterType,
	} {
		for nameOrSNI, msg := range resources.Index[indexType] {
			switch resource := msg.(type) {
			case *envoy_cluster_v3.Cluster:
				// If the Envoy extension configuration is for an upstream service, the Cluster's
				// name must match the upstream service's SNI.
				if config.IsUpstream() && !config.MatchesUpstreamServiceSNI(nameOrSNI) {
					continue
				}

				// If the extension's config is for an an inbound listener, the Cluster's name
				// must be xdscommon.LocalAppClusterName.
				if !config.IsUpstream() && nameOrSNI == xdscommon.LocalAppClusterName {
					continue
				}

				newCluster, patched, err := envoyExtender.Extension.PatchCluster(config, resource)
				if err != nil {
					resultErr = multierror.Append(resultErr, fmt.Errorf("error patching cluster: %w", err))
					continue
				}
				if patched {
					resources.Index[xdscommon.ClusterType][nameOrSNI] = newCluster
				}

			case *envoy_listener_v3.Listener:
				newListener, patched, err := envoyExtender.patchListener(config, resource)
				if err != nil {
					resultErr = multierror.Append(resultErr, fmt.Errorf("error patching listener: %w", err))
					continue
				}
				if patched {
					resources.Index[xdscommon.ListenerType][nameOrSNI] = newListener
				}

			case *envoy_route_v3.RouteConfiguration:
				// If the Envoy extension configuration is for an upstream service, the route's
				// name must match the upstream service's Envoy ID.
				matchesEnvoyID := config.EnvoyID() == nameOrSNI
				if config.IsUpstream() && !config.MatchesUpstreamServiceSNI(nameOrSNI) && !matchesEnvoyID {
					continue
				}

				// There aren't routes for inbound services.
				if !config.IsUpstream() {
					continue
				}

				newRoute, patched, err := envoyExtender.Extension.PatchRoute(config, resource)
				if err != nil {
					resultErr = multierror.Append(resultErr, fmt.Errorf("error patching route: %w", err))
					continue
				}
				if patched {
					resources.Index[xdscommon.RouteType][nameOrSNI] = newRoute
				}
			default:
				resultErr = multierror.Append(resultErr, fmt.Errorf("unsupported type was skipped: %T", resource))
			}
		}
	}

	return resources, resultErr
}

func (envoyExtension BasicEnvoyExtender) patchListener(config *RuntimeConfig, l *envoy_listener_v3.Listener) (proto.Message, bool, error) {
	switch config.Kind {
	case api.ServiceKindTerminatingGateway:
		return envoyExtension.patchTerminatingGatewayListener(config, l)
	case api.ServiceKindConnectProxy:
		return envoyExtension.patchConnectProxyListener(config, l)
	}
	return l, false, nil
}

func (b BasicEnvoyExtender) patchTerminatingGatewayListener(config *RuntimeConfig, l *envoy_listener_v3.Listener) (proto.Message, bool, error) {
	// We don't support directly targeting terminating gateways with extensions.
	if !config.IsUpstream() {
		return l, false, nil
	}

	var resultErr error
	patched := false
	for _, filterChain := range l.FilterChains {
		sni := getSNI(filterChain)

		if sni == "" {
			continue
		}

		// The filter chain's SNI must match the upstream service's SNI.
		if !config.MatchesUpstreamServiceSNI(sni) {
			continue
		}

		var filters []*envoy_listener_v3.Filter

		for _, filter := range filterChain.Filters {
			newFilter, ok, err := b.Extension.PatchFilter(config, filter)

			if err != nil {
				resultErr = multierror.Append(resultErr, fmt.Errorf("error patching listener filter: %w", err))
				filters = append(filters, filter)
				continue
			}
			if ok {
				filters = append(filters, newFilter)
				patched = true
			} else {
				filters = append(filters, filter)
			}
		}
		filterChain.Filters = filters
	}

	return l, patched, resultErr
}

func (b BasicEnvoyExtender) patchConnectProxyListener(config *RuntimeConfig, l *envoy_listener_v3.Listener) (proto.Message, bool, error) {
	var resultErr error

	envoyID := ""
	if i := strings.IndexByte(l.Name, ':'); i != -1 {
		envoyID = l.Name[:i]
	}

	if config.IsUpstream() && envoyID == xdscommon.OutboundListenerName {
		return b.patchTProxyListener(config, l)
	}

	// If the Envoy extension configuration is for an upstream service, the listener's
	// name must match the upstream service's EnvoyID or be the outbound listener.
	if config.IsUpstream() && envoyID != config.EnvoyID() {
		return l, false, nil
	}

	// If the Envoy extension configuration is for inbound resources, the
	// listener must be named xdscommon.PublicListenerName.
	if !config.IsUpstream() && envoyID != xdscommon.PublicListenerName {
		return l, false, nil
	}

	var patched bool

	for _, filterChain := range l.FilterChains {
		var filters []*envoy_listener_v3.Filter

		for _, filter := range filterChain.Filters {
			newFilter, ok, err := b.Extension.PatchFilter(config, filter)
			if err != nil {
				resultErr = multierror.Append(resultErr, fmt.Errorf("error patching listener filter: %w", err))
				filters = append(filters, filter)
				continue
			}

			if ok {
				filters = append(filters, newFilter)
				patched = true
			} else {
				filters = append(filters, filter)
			}
		}
		filterChain.Filters = filters
	}

	return l, patched, resultErr
}

func (b BasicEnvoyExtender) patchTProxyListener(config *RuntimeConfig, l *envoy_listener_v3.Listener) (proto.Message, bool, error) {
	var resultErr error
	patched := false

	vip := config.Upstreams[config.ServiceName].VIP

	for _, filterChain := range l.FilterChains {
		var filters []*envoy_listener_v3.Filter

		match := filterChainTProxyMatch(vip, filterChain)
		if !match {
			continue
		}

		for _, filter := range filterChain.Filters {
			newFilter, ok, err := b.Extension.PatchFilter(config, filter)
			if err != nil {
				resultErr = multierror.Append(resultErr, fmt.Errorf("error patching listener filter: %w", err))
				filters = append(filters, filter)
				continue
			}

			if ok {
				filters = append(filters, newFilter)
				patched = true
			} else {
				filters = append(filters, filter)
			}
		}
		filterChain.Filters = filters
	}

	return l, patched, resultErr
}

func filterChainTProxyMatch(vip string, filterChain *envoy_listener_v3.FilterChain) bool {
	for _, prefixRange := range filterChain.FilterChainMatch.PrefixRanges {
		// Since we always set the address prefix as the full VIP (rather than a prefix), we can just check if they are
		// equal to find the matching filter chain.
		if vip == prefixRange.AddressPrefix {
			return true
		}
	}

	return false
}

func FilterClusterNames(filter *envoy_listener_v3.Filter) map[string]struct{} {
	clusterNames := make(map[string]struct{})
	if filter == nil {
		return clusterNames
	}

	if config := envoy_resource_v3.GetHTTPConnectionManager(filter); config != nil {
		// If it's using RDS, the cluster names will be in the route, rather than in the http filter's route config, so
		// we don't return any cluster names in this case. They can be gathered from the route.
		if config.GetRds() != nil {
			return clusterNames
		}

		cfg := config.GetRouteConfig()

		clusterNames = RouteClusterNames(cfg)
	}

	if config := GetTCPProxy(filter); config != nil {
		clusterNames[config.GetCluster()] = struct{}{}
	}

	return clusterNames
}

func RouteClusterNames(route *envoy_route_v3.RouteConfiguration) map[string]struct{} {
	if route == nil {
		return nil
	}

	clusterNames := make(map[string]struct{})

	for _, virtualHost := range route.VirtualHosts {
		for _, route := range virtualHost.Routes {
			r := route.GetRoute()
			if r == nil {
				continue
			}
			if c := r.GetCluster(); c != "" {
				clusterNames[r.GetCluster()] = struct{}{}
			}

			if wc := r.GetWeightedClusters(); wc != nil {
				for _, c := range wc.GetClusters() {
					if c.Name != "" {
						clusterNames[c.Name] = struct{}{}
					}
				}
			}
		}
	}
	return clusterNames
}

func GetTCPProxy(filter *envoy_listener_v3.Filter) *envoy_tcp_proxy_v3.TcpProxy {
	if typedConfig := filter.GetTypedConfig(); typedConfig != nil {
		config := &envoy_tcp_proxy_v3.TcpProxy{}
		if err := anypb.UnmarshalTo(typedConfig, config, proto.UnmarshalOptions{}); err == nil {
			return config
		}
	}

	return nil
}

func getSNI(chain *envoy_listener_v3.FilterChain) string {
	var sni string

	if chain == nil {
		return sni
	}

	if chain.FilterChainMatch == nil {
		return sni
	}

	if len(chain.FilterChainMatch.ServerNames) == 0 {
		return sni
	}

	return chain.FilterChainMatch.ServerNames[0]
}
