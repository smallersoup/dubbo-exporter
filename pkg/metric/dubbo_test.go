package metric

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/reiver/go-telnet"
	"github.com/xuchaoi/dubbo-exporter/pkg/util"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
	"regexp"
	"strconv"
	"sync"
)

const (
	TestDubboServer              = "localhost:20880"
	namespaceLabel               = "namespace"
	podNameLabel                 = "podName"
	podIPLabel                   = "podIP"
	podPortLabel                 = "podPort"
	maxMetric                    = "max"
	activeMetric                 = "active"
	defaultDubboPodLabelSelector = "monitor-type-thread-dubbo-pool=enable"
	DubboExporter                = "dubbo_exporter"
)

type Exporter struct {
	metrics       map[string]*prometheus.GaugeVec
	podLister     v1.PodLister
	defaultClient *kubernetes.Clientset
	dubboPort     int
}

func (e *Exporter) Describe(descs chan<- *prometheus.Desc) {
	for _, m := range e.metrics {
		m.Describe(descs)
	}
}

func (e *Exporter) collectMetrics(metrics chan<- prometheus.Metric) {
	for _, m := range e.metrics {
		m.Collect(metrics)
	}
}

func (e *Exporter) Collect(metrics chan<- prometheus.Metric) {
	se, _ := labels.Parse(defaultDubboPodLabelSelector)
	pods, err := e.podLister.List(se)
	if err != nil {
		return
	}

	wg := sync.WaitGroup{}
	for _, pod := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if pod.Status.PodIP == "" {
				klog.Warningf("Dubbo pod: %v/%v podIP is empty, skip collect!", pod.Namespace, pod.Name)
				return
			}
			if !util.IsPodReady(pod) {
				klog.Warningf("Dubbo pod: %v/%v is not ready, skip collect!", pod.Namespace, pod.Name)
				return
			}
			max, active, err := metric(fmt.Sprintf("%v:%v", pod.Status.PodIP, e.dubboPort))
			if err != nil {
				klog.Warningf("Read dubbo pod: %v/%v metric err: %v", pod.Namespace, pod.Name, err)
				return
			}
			m, errm := strconv.Atoi(max)
			a, erra := strconv.Atoi(active)

			if errm != nil || erra != nil {
				klog.Warningf("Dubbo pod: %v/%v max err: %v, active err: %v, skip collect!", pod.Namespace, pod.Name, errm, erra)
				return
			}

			label := map[string]string{namespaceLabel: pod.Namespace, podNameLabel: pod.Name, podIPLabel: pod.Status.PodIP, podPortLabel: string(e.dubboPort)}
			e.metrics[maxMetric].With(label).Set(float64(m))
			e.metrics[activeMetric].With(label).Set(float64(a))
		}()
	}
	wg.Wait()
	e.collectMetrics(metrics)
}

// NewDubboExporter returns a new exporter of Redis metrics.
// note to self: next time we add an argument, instead add a RedisExporter struct
func NewDubboExporter(informer informers.SharedInformerFactory, c *kubernetes.Clientset, port int) *Exporter {
	e := Exporter{
		metrics:       make(map[string]*prometheus.GaugeVec),
		podLister:     informer.Core().V1().Pods().Lister(),
		defaultClient: c,
		dubboPort:     port,
	}
	e.metrics[maxMetric] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: DubboExporter,
		Name:      maxMetric,
		Help:      "The value of max",
	}, []string{namespaceLabel, podNameLabel, podIPLabel, podPortLabel})

	e.metrics[activeMetric] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: DubboExporter,
		Name:      activeMetric,
		Help:      "The value of active",
	}, []string{namespaceLabel, podNameLabel, podIPLabel, podPortLabel})
	return &e
}

func metric(addr string) (string, string, error) {
	conn, err := telnet.DialTo(addr)
	if err != nil {
		return "", "", err
	}

	conn.Write([]byte("status -l \n"))
	output := make([]byte, 400)
	_, err = conn.Read(output)
	defer conn.Close()
	if err != nil {
		return "", "", err
	}
	klog.Infof("conn: %s", string(output))

	r1 := regexp.MustCompile("max:[\\d]+")
	tmpPoolMax := r1.FindStringSubmatch(string(output))
	r2 := regexp.MustCompile("active:[\\d]+")
	tmpPoolActive := r2.FindStringSubmatch(string(output))
	klog.Infof("Dubbo metrics data: %s, %s", tmpPoolMax, tmpPoolActive)
	return tmpPoolMax[1], tmpPoolActive[1], nil
}
