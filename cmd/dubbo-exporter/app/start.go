// Copyright 2018 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package app

import (
	"fmt"
	apiv3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s/resources"
	"github.com/spf13/cobra"
	genericmetrics "harmonycloud.cn/ippool-server/pkg/apiserver/generic"
	poolalpha1 "harmonycloud.cn/ippool-server/pkg/client/clientset_generated/clientset"
	poolInfomers "harmonycloud.cn/ippool-server/pkg/client/informers_generated/externalversions"
	"harmonycloud.cn/ippool-server/pkg/common"
	"harmonycloud.cn/ippool-server/pkg/metrics"
	"harmonycloud.cn/ippool-server/pkg/metrics/manager"
	"harmonycloud.cn/ippool-server/pkg/metrics/sources"
	svcalpha1 "harmonycloud.cn/ippool-server/pkg/pool/serviceippool/generated/clientset/versioned"
	svcpoolInformer "harmonycloud.cn/ippool-server/pkg/pool/serviceippool/generated/informers/externalversions"
	"harmonycloud.cn/ippool-server/pkg/storage/calico"
	"io"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	apiserver "k8s.io/apiserver/pkg/server"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/logs"
	"k8s.io/klog"
	"net"
	"net/http"
	goruntime "runtime"
	"sigs.k8s.io/apiserver-builder-alpha/pkg/builders"
	"time"
)

// NewCommandStartPoolServer provides a CLI handler for the service pool server entrypoint
func NewCommandStartPoolServer(out, errOut io.Writer, stopCh <-chan struct{}) *cobra.Command {
	o := NewServicePoolServerOptions()
	cmd := &cobra.Command{
		Short: "Launch svc-pool",
		Long:  "Launch svc-pool",
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Run(stopCh); err != nil {
				return err
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.DurationVar(&o.RecyleLoopPeriod, "recyle-loop-period", o.RecyleLoopPeriod, "recyle loop period")
	flags.DurationVar(&o.RecyleLoopWait, "recyle-loop-wait", o.RecyleLoopWait, "recyle loop wait")
	flags.DurationVar(&o.MetricResolution, "metric-resolution", o.MetricResolution, "The resolution at which ippool-server will retain metrics.")
	flags.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "The path to the kubeconfig used to connect to the Kubernetes API server and the Kubelets (defaults to in-cluster config)")
	flags.StringVar(&o.RandomSelectorKey, "random-selector-key", o.RandomSelectorKey, "random selector key")
	flags.StringVar(&o.RandomSelectorValue, "random-selector-value", o.RandomSelectorValue, "random selector value")
	flags.StringVar(&o.StatefulSelectorKey, "stateful-selector-key", o.StatefulSelectorKey, "stateful selector key")
	flags.StringVar(&o.StatefulSelectorValue, "stateful-selector-value", o.StatefulSelectorValue, "stateful selector value")
	flags.StringVar(&o.StatelessLabelKey, "stateless-label-key", o.StatelessLabelKey, "stateless label key")
	flags.StringVar(&o.StatelessLabelValue, "stateless-label-value", o.StatelessLabelValue, "stateless label value")

	o.SecureServing.AddFlags(flags)
	o.InSecureServing.AddFlags(flags)
	o.Authentication.AddFlags(flags)
	o.Authorization.AddFlags(flags)
	o.Features.AddFlags(flags)

	return cmd
}

type ServicePoolServerOptions struct {
	// genericoptions.ReccomendedOptions - EtcdOptions
	SecureServing         *genericoptions.SecureServingOptionsWithLoopback
	InSecureServing       *genericoptions.DeprecatedInsecureServingOptionsWithLoopback
	InSecureMetricServing *apiserver.DeprecatedInsecureServingInfo
	Authentication        *genericoptions.DelegatingAuthenticationOptions
	Authorization         *genericoptions.DelegatingAuthorizationOptions
	Features              *genericoptions.FeatureOptions

	Kubeconfig string

	MetricResolution time.Duration
	RecyleLoopPeriod time.Duration
	RecyleLoopWait   time.Duration
	//选择器
	common.MetricSelector
	// Only to be used to for testing
	DisableAuthForTesting bool
}

// NewServicePoolServerOptions constructs a new set of default options for svc-pool.
func NewServicePoolServerOptions() *ServicePoolServerOptions {
	o := &ServicePoolServerOptions{
		SecureServing: genericoptions.NewSecureServingOptions().WithLoopback(),
		InSecureServing: (&genericoptions.DeprecatedInsecureServingOptions{
			BindAddress: net.ParseIP("0.0.0.0"),
			BindPort:    8080,
			BindNetwork: "tcp",
		}).WithLoopback(),
		Authentication:   genericoptions.NewDelegatingAuthenticationOptions(),
		Authorization:    genericoptions.NewDelegatingAuthorizationOptions(),
		Features:         genericoptions.NewFeatureOptions(),
		MetricResolution: 60 * time.Second,
		RecyleLoopPeriod: 30 * time.Second,
		RecyleLoopWait:   20 * time.Second,
		MetricSelector: common.MetricSelector{
			RandomSelectorKey:     "nodetype",
			RandomSelectorValue:   "STATELESS",
			StatefulSelectorKey:   "nodetype",
			StatefulSelectorValue: "STATEFUL",
			StatelessLabelKey:     common.LabelKey,
			StatelessLabelValue:   common.LabelValue,
		},
	}
	return o
}

func (o *ServicePoolServerOptions) Config() (*genericmetrics.Config, error) {
	if err := o.SecureServing.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		return nil, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	//enable监控metrics等接口开关
	serverConfig := genericapiserver.NewConfig(builders.Codecs)
	if err := o.SecureServing.ApplyTo(&serverConfig.SecureServing, &serverConfig.LoopbackClientConfig); err != nil {
		return nil, err
	}
	if err := o.InSecureServing.ApplyTo(&o.InSecureMetricServing, &serverConfig.LoopbackClientConfig); err != nil {
		return nil, err
	}

	if !o.DisableAuthForTesting {
		if err := o.Authentication.ApplyTo(&serverConfig.Authentication, serverConfig.SecureServing, nil); err != nil {
			return nil, err
		}
		if err := o.Authorization.ApplyTo(&serverConfig.Authorization); err != nil {
			return nil, err
		}
	}

	return &genericmetrics.Config{
		GenericConfig: serverConfig,
	}, nil
}

// buildHandlerChain wraps the given handler with the standard filters.
func buildHandlerChain(handler http.Handler, authn authenticator.Request, authz authorizer.Authorizer) http.Handler {
	requestInfoResolver := &apirequest.RequestInfoFactory{}
	failedHandler := genericapifilters.Unauthorized(builders.Codecs, false)

	handler = genericapifilters.WithAuthorization(handler, authz, builders.Codecs)
	handler = genericapifilters.WithAuthentication(handler, authn, failedHandler, nil)
	handler = genericapifilters.WithRequestInfo(handler, requestInfoResolver)
	handler = genericapifilters.WithCacheControl(handler)
	handler = genericfilters.WithPanicRecovery(handler)

	return handler
}

func (o *ServicePoolServerOptions) Run(stopCh <-chan struct{}) error {
	// grab the config for the API server
	//打开监控metrics接口的开关,o.Config()里默认打开
	config, err := o.Config()
	if err != nil {
		return err
	}

	// set up the client config
	var clientConfig *rest.Config
	if len(o.Kubeconfig) > 0 {
		loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: o.Kubeconfig}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{})

		clientConfig, err = loader.ClientConfig()
	} else {
		clientConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return fmt.Errorf("unable to construct lister client config: %v", err)
	}
	// Use protobufs for communication with apiserver
	//clientConfig.ContentType = "application/vnd.kubernetes.protobuf"

	// set up the informers
	config.KubeClient, err = kubernetes.NewForConfig(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}
	//svc pool client
	config.SvcIPPoolClient, err = svcalpha1.NewForConfig(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}

	//pool client
	config.PoolClientSet, err = poolalpha1.NewForConfig(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}

	//calico crd client
	copyConfig := rest.CopyConfig(clientConfig)
	restClient, err := buildCalicoCRDClientV1(*copyConfig)
	if err != nil {
		klog.Fatal(err)
	}

	//calico ippool client
	config.K8sResourceClient = calico.NewCalicoIPPoolClient(config.KubeClient, restClient)

	// create the calico ippool watcher
	calicoIPPoolListWatcher := cache.NewListWatchFromClient(restClient, resources.IPPoolResourceName, "", fields.Everything())
	config.CalicoIPPoolIndexer, config.CalicoIPPoolInformer = cache.NewIndexerInformer(calicoIPPoolListWatcher, &apiv3.IPPool{}, 0, cache.ResourceEventHandlerFuncs{}, cache.Indexers{})

	// create the calico ipamblock watcher
	calicoIPAMBlockListWatcher := cache.NewListWatchFromClient(restClient, resources.IPAMBlockResourceName, "", fields.Everything())
	config.CalicoIPAMBlockIndexer, config.CalicoIPAMBlockInformer = cache.NewIndexerInformer(calicoIPAMBlockListWatcher, &apiv3.IPAMBlock{}, 0, cache.ResourceEventHandlerFuncs{}, cache.Indexers{})

	//crd client
	//extensionClientSet, err := extensionClientSet.NewForConfig(clientConfig)
	/*_, err = extensionClientSet.NewForConfig(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}*/

	//TODO 代码创建CRD

	config.SvcPoolInformerFactory = svcpoolInformer.NewSharedInformerFactory(config.SvcIPPoolClient, 0)
	// we should never need to resync, since we're not worried about missing events,
	// and resync is actually for regular interval-based reconciliation these days,
	// so set the default resync interval to 0
	informers := informers.NewSharedInformerFactory(config.KubeClient, 0)

	config.PoolInfomersFactory = poolInfomers.NewSharedInformerFactory(config.PoolClientSet, 0)
	// inject the providers into the config
	//config.ProviderConfig.Node = metricsProvider

	// complete the config to get an API server
	//添加接口
	server, err := config.Complete(informers)
	if err != nil {
		return err
	}
	//metrics接口http
	if o.InSecureMetricServing != nil {
		handler := buildHandlerChain(newMetricsHandler(config), nil, nil)
		if err := o.InSecureMetricServing.Serve(handler, 0, stopCh); err != nil {
			return fmt.Errorf("failed to start metrics server: %v", err)
		}
	}
	sourceProvider := sources.NewMetricsProvider(config.KubeClient,
		config.PoolInfomersFactory,
		config.PoolClientSet,
		config.CalicoIPPoolIndexer,
		config.CalicoIPAMBlockIndexer,
		o.MetricSelector)
	scrapeTimeout := time.Duration(float64(o.MetricResolution) * 0.90) // scrape timeout is 90% of the scrape interval
	// set up the general manager
	metrics.RegisterMetrics(o.MetricResolution, scrapeTimeout)
	mgr := manager.NewManager(manager.NewSourceManager(sourceProvider, scrapeTimeout), o.MetricResolution)
	// add health checks
	server.AddHealthChecks(healthz.NamedCheck("healthz", mgr.CheckHealth))

	go config.CalicoIPPoolInformer.Run(stopCh)
	go config.CalicoIPAMBlockInformer.Run(stopCh)
	go config.CompletedConfig.SharedInformerFactory.Start(stopCh)
	go config.SvcPoolInformerFactory.Start(stopCh)
	go config.PoolInfomersFactory.Start(stopCh)

	//等待缓存同步
	if !cache.WaitForNamedCacheSync("ippool-leak-controller", stopCh,
		config.CalicoIPPoolInformer.HasSynced,
		config.PoolInfomersFactory.Crd().V1alpha1().IPPoolMaintainers().Informer().HasSynced,
		config.CalicoIPAMBlockInformer.HasSynced,
		config.SvcPoolInformerFactory.Serviceippool().V1alpha1().ServiceIPPools().Informer().HasSynced) {
		return fmt.Errorf("cache sync failed")
	}

	// run everything (the apiserver runs the shared informer factory for us)
	mgr.RunUntil(stopCh)

	calico.NewIPPoolCleanController(config.PoolInfomersFactory, config.SvcPoolInformerFactory,
		config.PoolClientSet.CrdV1alpha1().IPPoolMaintainers(),
		config.CalicoIPAMBlockIndexer,
		config.CalicoIPPoolIndexer,
		stopCh,
		o.RecyleLoopPeriod,
		o.RecyleLoopWait,
		config.K8sResourceClient)
	return server.GenericAPIServer.PrepareRun().Run(stopCh)
}

// newMetricsHandler builds a metrics server from the config.
func newMetricsHandler(config *genericmetrics.Config) http.Handler {
	pathRecorderMux := mux.NewPathRecorderMux("ippool-server")
	routes.MetricsWithReset{}.Install(pathRecorderMux)
	if config.EnableProfiling {
		routes.Profiling{}.Install(pathRecorderMux)
		if config.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
		routes.DebugFlags{}.Install(pathRecorderMux, "v", routes.StringFlagPutHandler(logs.GlogSetter))
	}
	return pathRecorderMux
}

// buildCalicoCRDClientV1 builds a RESTClient configured to interact with Calico CustomResourceDefinitions
func buildCalicoCRDClientV1(cfg rest.Config) (*rest.RESTClient, error) {
	// Generate config using the base config.
	cfg.GroupVersion = &schema.GroupVersion{
		Group:   "crd.projectcalico.org",
		Version: "v1",
	}
	cfg.APIPath = "/apis"
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: builders.Codecs}

	cli, err := rest.RESTClientFor(&cfg)
	if err != nil {
		return nil, err
	}
	// We also need to register resources.
	builders.Scheme.AddKnownTypes(*cfg.GroupVersion,
		&apiv3.IPPool{},
		&apiv3.IPPoolList{},
		&apiv3.IPAMBlock{},
		&apiv3.IPAMBlockList{})
	return cli, nil
}
