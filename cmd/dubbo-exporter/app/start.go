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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/xuchaoi/dubbo-exporter/pkg/metric"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	"net/http"
	"time"
)

func NewCommandStartExporterServer(stopCh <-chan struct{}) *cobra.Command {
	o := NewDubboExporterServerOptions()
	cmd := &cobra.Command{
		Short: "Launch dubbo-exporter",
		Long:  "Launch dubbo-exporter",
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Run(stopCh); err != nil {
				return err
			}
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "The path to the kubeconfig used to connect to the Kubernetes API server and the Kubelets (defaults to in-cluster config)")
	flags.StringVar(&o.MetricPath, "path", o.MetricPath, "Path under which to expose metrics.")
	flags.StringVar(&o.ListenAddress, "listen-addr", o.ListenAddress, "Address to listen on for web interface and telemetry.")
	flags.StringVar(&o.dubboPodLabel, "dubbo-pod-label", o.dubboPodLabel, "dubbo pod label, default "+metric.DefaultDubboPodLabelSelector)
	flags.StringVar(&o.provinceNodeLabelValue, "province", o.provinceNodeLabelValue, "province node label value, must not be empty by specific province collect, default "+metric.ProvinceAll+" pod in k8s cluster")
	flags.IntVar(&o.dubboPort, "dubbo-port", o.dubboPort, "dubbo port, default 8082")
	flags.DurationVar(&o.telnetTimeout, "telnet-timeout", o.telnetTimeout, "telnet timeout, default 100ms")
	return cmd
}

type DubboExporterServerOptions struct {
	Kubeconfig             string
	MetricPath             string
	ListenAddress          string
	dubboPodLabel          string
	provinceNodeLabelValue string
	dubboPort              int
	telnetTimeout          time.Duration
}

// NewDubboExporterServerOptions constructs a new set of default options for metric.DubboExporter.
func NewDubboExporterServerOptions() *DubboExporterServerOptions {
	o := &DubboExporterServerOptions{
		MetricPath:             "/metrics",
		ListenAddress:          ":8080",
		dubboPort:              8082,
		dubboPodLabel:          metric.DefaultDubboPodLabelSelector,
		telnetTimeout:          100 * time.Millisecond,
		provinceNodeLabelValue: metric.ProvinceAll,
	}
	return o
}

func (o *DubboExporterServerOptions) Run(stopCh <-chan struct{}) error {

	_, err := labels.Parse(o.dubboPodLabel)
	if err != nil {
		return err
	}

	if o.provinceNodeLabelValue == "" {
		return fmt.Errorf("--province args must be not empty")
	}

	// set up the client config
	var (
		clientConfig *rest.Config
	)
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

	// set up the informers
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		klog.Fatal(err)
	}

	informers := informers.NewSharedInformerFactory(kubeClient, 0)

	exp := metric.NewDubboExporter(informers, kubeClient, o.dubboPort, o.dubboPodLabel, o.provinceNodeLabelValue, o.telnetTimeout)
	podInformer := informers.Core().V1().Pods().Informer()
	nodeInformer := informers.Core().V1().Nodes().Informer()
	go informers.Start(stopCh)
	//等待缓存同步
	if !cache.WaitForNamedCacheSync(metric.DubboExporter, stopCh, podInformer.HasSynced, nodeInformer.HasSynced) {
		return fmt.Errorf("pod cache not sync")
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(exp)
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	//metrics接口
	http.Handle(o.MetricPath, handler)
	//健康检查接口
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`ok`))
	})
	klog.Fatal(http.ListenAndServe(o.ListenAddress, nil))
	return fmt.Errorf("never unreachable")
}
