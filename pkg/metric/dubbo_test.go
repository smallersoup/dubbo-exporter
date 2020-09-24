package metric

import (
	"testing"
	"time"
)

func Test_Metric(t *testing.T) {
	e := Exporter{
		telnetTimeout: 100 * time.Microsecond,
	}
	max, active, err := e.metric(testDubboServer)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(max, active)
}
