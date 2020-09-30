package metric

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/xuchaoi/dubbo-exporter/pkg/util"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog"
	"net"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	testDubboServer              = "localhost:20880"
	namespaceLabel               = "namespace"
	podNameLabel                 = "name"
	podProvinceLabel             = "province"
	podIPLabel                   = "ip"
	podPortLabel                 = "port"
	maxMetric                    = "max"
	activeMetric                 = "active"
	DefaultDubboPodLabelSelector = "monitor-type-thread-dubbo-pool=enable"
	DubboExporter                = "dubbo_exporter"
	provinceNodeLabelKey         = "province-%v"
	ProvinceAll                  = "all"
)

type Exporter struct {
	metrics                map[string]*prometheus.GaugeVec
	podLister              v1.PodLister
	nodeLister             v1.NodeLister
	defaultClient          *kubernetes.Clientset
	dubboPort              int
	dubboPodLabel          string
	provinceNodeLabelValue string
	telnetTimeout          time.Duration
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
	//清理metrics信息
	e.reset()
	se, _ := labels.Parse(e.dubboPodLabel)
	pods, err := e.podLister.List(se)
	if err != nil {
		return
	}

	wg := sync.WaitGroup{}
	for _, p := range pods {
		wg.Add(1)
		go func(pod *apiv1.Pod) {
			defer wg.Done()
			nodeName := pod.Spec.NodeName
			if nodeName == "" || pod.Status.PodIP == "" {
				klog.V(2).Infof("Dubbo pod: %v/%v nodeName: %v or podIP: %v is empty, skip collect!", pod.Namespace, pod.Name, pod.Spec.NodeName, pod.Status.PodIP)
				return
			}
			node, err := e.nodeLister.Get(nodeName)
			if err != nil {
				klog.Errorf("Get dubbo pod: %v/%v node: %v err: %v, skip!", pod.Namespace, pod.Name, nodeName, err)
				return
			}
			if e.provinceNodeLabelValue != ProvinceAll && node.Labels[fmt.Sprintf(provinceNodeLabelKey, e.provinceNodeLabelValue)] != e.provinceNodeLabelValue {
				klog.V(2).Infof("Dubbo pod: %v/%v nodeName: %v is not province: %v, skip collect!", pod.Namespace, pod.Name, pod.Spec.NodeName, e.provinceNodeLabelValue)
				return
			}
			if !util.IsPodReady(pod) {
				klog.V(2).Infof("Dubbo pod: %v/%v is not ready, skip collect!", pod.Namespace, pod.Name)
				return
			}
			max, active, err := e.metric(fmt.Sprintf("%v:%v", pod.Status.PodIP, e.dubboPort))
			if err != nil {
				klog.V(2).Infof("Read dubbo pod: %v/%v metric err: %v", pod.Namespace, pod.Name, err)
				return
			}
			m, errm := strconv.Atoi(max)
			a, erra := strconv.Atoi(active)

			if errm != nil || erra != nil {
				klog.V(2).Infof("Dubbo pod: %v/%v max err: %v, active err: %v, skip collect!", pod.Namespace, pod.Name, errm, erra)
				return
			}

			label := map[string]string{namespaceLabel: pod.Namespace, podNameLabel: pod.Name, podIPLabel: pod.Status.PodIP, podPortLabel: strconv.Itoa(e.dubboPort), podProvinceLabel: e.provinceNodeLabelValue}
			e.metrics[maxMetric].With(label).Set(float64(m))
			e.metrics[activeMetric].With(label).Set(float64(a))
		}(p)
	}
	wg.Wait()
	e.collectMetrics(metrics)
}

// NewDubboExporter returns a new exporter of Redis metrics.
// note to self: next time we add an argument, instead add a RedisExporter struct
func NewDubboExporter(informer informers.SharedInformerFactory,
	c *kubernetes.Clientset,
	port int,
	dubboPodLabel string,
	provinceNodeLabelValue string,
	tm time.Duration) *Exporter {
	e := Exporter{
		metrics:                make(map[string]*prometheus.GaugeVec),
		podLister:              informer.Core().V1().Pods().Lister(),
		nodeLister:             informer.Core().V1().Nodes().Lister(),
		defaultClient:          c,
		dubboPort:              port,
		dubboPodLabel:          dubboPodLabel,
		provinceNodeLabelValue: provinceNodeLabelValue,
		telnetTimeout:          tm,
	}
	e.metrics[maxMetric] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: DubboExporter,
		Name:      maxMetric,
		Help:      "The value of max",
	}, []string{namespaceLabel, podNameLabel, podIPLabel, podPortLabel, podProvinceLabel})

	e.metrics[activeMetric] = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: DubboExporter,
		Name:      activeMetric,
		Help:      "The value of active",
	}, []string{namespaceLabel, podNameLabel, podIPLabel, podPortLabel, podProvinceLabel})
	return &e
}

func (e *Exporter) metric(addr string) (string, string, error) {
	// 3 秒超时
	conn, err := net.DialTimeout("tcp", addr, e.telnetTimeout)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	conn.Write([]byte("status -l \n"))
	output := make([]byte, 400)
	_, err = conn.Read(output)

	if err != nil {
		return "", "", err
	}
	klog.V(2).Infof("conn: %s", string(output))

	r1 := regexp.MustCompile("max:([\\d]+)")
	tmpPoolMax := r1.FindStringSubmatch(string(output))
	r2 := regexp.MustCompile("active:([\\d]+)")
	tmpPoolActive := r2.FindStringSubmatch(string(output))
	klog.V(2).Infof("Dubbo metrics data: %s, %s", tmpPoolMax, tmpPoolActive)
	if len(tmpPoolActive) != 2 || len(tmpPoolMax) != 2 {
		return "", "", fmt.Errorf("get max--- %v ; active--- %v", tmpPoolMax, tmpPoolActive)
	}
	return tmpPoolMax[1], tmpPoolActive[1], nil
}

//清理之前的metric
func (e *Exporter) reset() {
	for _, metric := range e.metrics {
		metric.Reset()
	}
}
