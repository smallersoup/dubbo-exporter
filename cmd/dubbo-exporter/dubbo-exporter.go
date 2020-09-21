package main

import (
	"github.com/reiver/go-telnet"
	"k8s.io/klog"
	"regexp"
)

const TestDubboServer = "localhost:20880"

func main()  {
	conn, err := telnet.DialTo(TestDubboServer)
	if err != nil {
		klog.Error(err)
	}

	conn.Write([]byte("status -l \n"))
	b := make([]byte, 400)
	_, err = conn.Read(b)
	if err != nil {
		klog.Error(err)
	}
	klog.Infof("conn: %s", string(b))

	tmp1 := b
	tmp2 := b
	r1, _ := regexp.Compile("max:[\\d]+")
	tmpPoolMax := r1.Find(tmp1)
	r2, _ := regexp.Compile("active:[\\d]+")
	tmpPoolActive := r2.Find(tmp2)
	klog.Infof("Dubbo metrics data: %s, %s", tmpPoolMax, tmpPoolActive)

}