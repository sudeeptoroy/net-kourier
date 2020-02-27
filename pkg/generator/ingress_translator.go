package generator

import (
	"fmt"
	"kourier/pkg/envoy"
	"kourier/pkg/knative"
	"strconv"
	"time"

	endpoint "github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"

	"go.uber.org/zap"
	kubev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubeclient "k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"knative.dev/pkg/tracker"
	"knative.dev/serving/pkg/apis/networking/v1alpha1"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	route "github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
)

type IngressTranslator struct {
	kubeclient      kubeclient.Interface
	endpointsLister corev1listers.EndpointsLister
	localDomainName string
	tracker         tracker.Interface
	logger          *zap.SugaredLogger
}

type translatedIngress struct {
	ingressName          string
	ingressNamespace     string
	sniMatches           []*envoy.SNIMatch
	routes               []*route.Route
	clusters             []*v2.Cluster
	externalVirtualHosts []*route.VirtualHost
	internalVirtualHosts []*route.VirtualHost
}

func NewIngressTranslator(kubeclient kubeclient.Interface, endpointsLister corev1listers.EndpointsLister, localDomainName string, tracker tracker.Interface) IngressTranslator {
	return IngressTranslator{
		kubeclient:      kubeclient,
		endpointsLister: endpointsLister,
		localDomainName: localDomainName,
		tracker:         tracker,
	}
}

func newTranslatedIngress(ingressName string, ingressNamespace string) translatedIngress {
	return translatedIngress{
		ingressName:      ingressName,
		ingressNamespace: ingressNamespace,
	}
}

func (translator *IngressTranslator) translateIngress(ingress *v1alpha1.Ingress, index int, extAuthzEnabled bool) (*translatedIngress, error) {
	res := newTranslatedIngress(ingress.Name, ingress.Namespace)

	for _, ingressTLS := range ingress.GetSpec().TLS {
		sniMatch, err := sniMatchFromIngressTLS(ingressTLS, translator.kubeclient)

		if err != nil {
			translator.logger.Errorf("%s", err)

			// We need to propagate this error to the reconciler so the current
			// event can be retried. This error might be caused because the
			// secrets referenced in the TLS section of the spec do not exist
			// yet. That's expected when auto TLS is configured.
			// See the "TestPerKsvcCert_localCA" test in Knative Serving. It's a
			// test that fails if this error is not propagated:
			// https://github.com/knative/serving/blob/571e4db2392839082c559870ea8d4b72ef61e59d/test/e2e/autotls/auto_tls_test.go#L68
			return nil, err
		}
		res.sniMatches = append(res.sniMatches, sniMatch)
	}

	for _, rule := range ingress.GetSpec().Rules {

		var ruleRoute []*route.Route

		for _, httpPath := range rule.HTTP.Paths {

			path := "/"
			if httpPath.Path != "" {
				path = httpPath.Path
			}

			var wrs []*route.WeightedCluster_ClusterWeight

			for _, split := range httpPath.Splits {
				headersSplit := split.AppendHeaders

				ref := kubev1.ObjectReference{
					Kind:       "Endpoints",
					APIVersion: "v1",
					Namespace:  ingress.Namespace,
					Name:       split.ServiceName,
				}

				if translator.tracker != nil {
					err := translator.tracker.Track(ref, ingress)
					if err != nil {
						translator.logger.Errorf("%s", err)
						break
					}
				}

				endpoints, err := translator.endpointsLister.Endpoints(split.ServiceNamespace).Get(split.ServiceName)
				if apierrors.IsNotFound(err) {
					translator.logger.Infof("Endpoints '%s/%s' not yet created", split.ServiceNamespace, split.ServiceName)
					break
				} else if err != nil {
					translator.logger.Errorf("Failed to fetch endpoints '%s/%s': %v", split.ServiceNamespace, split.ServiceName, err)
					break
				}

				service, err := translator.kubeclient.CoreV1().Services(split.ServiceNamespace).Get(split.ServiceName, metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					translator.logger.Infof("Service '%s/%s' not yet created", split.ServiceNamespace, split.ServiceName)
					break
				} else if err != nil {
					translator.logger.Errorf("Failed to fetch service '%s/%s': %v", split.ServiceNamespace, split.ServiceName, err)
					break
				}

				var targetPort int32
				http2 := false
				for _, port := range service.Spec.Ports {
					if port.Port == split.ServicePort.IntVal || port.Name == split.ServicePort.StrVal {
						targetPort = port.TargetPort.IntVal
						http2 = port.Name == "http2" || port.Name == "h2c"
					}
				}

				publicLbEndpoints := lbEndpointsForKubeEndpoints(endpoints, targetPort)

				connectTimeout := 5 * time.Second
				cluster := envoy.NewCluster(split.ServiceName+path, connectTimeout, publicLbEndpoints, http2, v2.Cluster_STATIC)

				res.clusters = append(res.clusters, cluster)

				weightedCluster := envoy.NewWeightedCluster(split.ServiceName+path, uint32(split.Percent), headersSplit)

				wrs = append(wrs, weightedCluster)
			}

			if len(wrs) != 0 {
				r := createRouteForRevision(ingress.Name, index, httpPath, wrs)
				ruleRoute = append(ruleRoute, r)
				res.routes = append(res.routes, r)
			}

		}

		if len(ruleRoute) == 0 {
			// Propagate the error to the reconciler, we do not want to generate
			// an envoy config where an ingress has no routes, it would return
			// 404.
			return nil, fmt.Errorf("ingress without routes")
		}

		externalDomains := knative.ExternalDomains(rule, translator.localDomainName)

		// External should also be accessible internally
		internalDomains := append(knative.InternalDomains(rule, translator.localDomainName), externalDomains...)

		var virtualHost, internalVirtualHost route.VirtualHost
		if extAuthzEnabled {

			visibility := ingress.GetSpec().Visibility
			if visibility == "" { // Needed because visibility is optional
				visibility = v1alpha1.IngressVisibilityClusterLocal
			}

			ContextExtensions := map[string]string{
				"client":     "kourier",
				"visibility": string(visibility),
			}

			ContextExtensions = mergeMapString(ContextExtensions, ingress.GetLabels())

			virtualHost = envoy.NewVirtualHostWithExtAuthz(ingress.Name, ContextExtensions, externalDomains, ruleRoute)
			internalVirtualHost = envoy.NewVirtualHostWithExtAuthz(ingress.Name, ContextExtensions, internalDomains,
				ruleRoute)
		} else {
			virtualHost = envoy.NewVirtualHost(ingress.GetName(), externalDomains, ruleRoute)
			internalVirtualHost = envoy.NewVirtualHost(ingress.GetName(), internalDomains, ruleRoute)
		}

		if knative.RuleIsExternal(rule, ingress.GetSpec().Visibility) {
			res.externalVirtualHosts = append(res.externalVirtualHosts, &virtualHost)
		}

		res.internalVirtualHosts = append(res.internalVirtualHosts, &internalVirtualHost)
	}

	return &res, nil
}

func lbEndpointsForKubeEndpoints(kubeEndpoints *kubev1.Endpoints, targetPort int32) (publicLbEndpoints []*endpoint.LbEndpoint) {
	for _, subset := range kubeEndpoints.Subsets {
		for _, address := range subset.Addresses {
			lbEndpoint := envoy.NewLBEndpoint(address.IP, uint32(targetPort))
			publicLbEndpoints = append(publicLbEndpoints, lbEndpoint)
		}
	}

	return publicLbEndpoints
}

func createRouteForRevision(routeName string, i int, httpPath v1alpha1.HTTPIngressPath, wrs []*route.WeightedCluster_ClusterWeight) *route.Route {
	name := routeName + "_" + strconv.Itoa(i)

	path := "/"
	if httpPath.Path != "" {
		path = httpPath.Path
	}

	var routeTimeout time.Duration
	if httpPath.Timeout != nil {
		routeTimeout = httpPath.Timeout.Duration
	}

	attempts := 0
	var perTryTimeout time.Duration
	if httpPath.Retries != nil {
		attempts = httpPath.Retries.Attempts

		if httpPath.Retries.PerTryTimeout != nil {
			perTryTimeout = httpPath.Retries.PerTryTimeout.Duration
		}
	}

	return envoy.NewRoute(
		name, path, wrs, routeTimeout, uint32(attempts), perTryTimeout, httpPath.AppendHeaders,
	)
}

func sniMatchFromIngressTLS(ingressTLS v1alpha1.IngressTLS, kubeClient kubeclient.Interface) (*envoy.SNIMatch, error) {
	certChain, privateKey, err := sslCreds(
		kubeClient, ingressTLS.SecretNamespace, ingressTLS.SecretName,
	)

	if err != nil {
		return nil, err
	}

	sniMatch := envoy.NewSNIMatch(ingressTLS.Hosts, certChain, privateKey)
	return &sniMatch, nil
}

func mergeMapString(a, b map[string]string) map[string]string {
	merged := make(map[string]string)
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return merged
}