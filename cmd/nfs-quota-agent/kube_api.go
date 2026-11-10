package main

import (
	"flag"
	"fmt"
	"math"

	"k8s.io/client-go/rest"
)

type kubeAPIOptions struct {
	qps   float64
	burst int
}

func (o *kubeAPIOptions) addFlags(fs *flag.FlagSet) {
	// D1: client-go's 5/10 defaults serialize PV bursts at ~2.5 PVs/s.
	// kube-controller-manager defaults to 20/30; this agent runs one pod per
	// NFS node with two API operations (GET + UPDATE) per new PV. Use 50/100
	// for bursts. Cost: more API load; escape hatch: set the flags to 5/10.
	fs.Float64Var(&o.qps, "kube-api-qps", 50, "Kubernetes API client QPS (must be > 0)")
	fs.IntVar(&o.burst, "kube-api-burst", 100, "Kubernetes API client burst (must be >= 1)")
}

func (o kubeAPIOptions) validate() error {
	if o.qps <= 0 || math.IsNaN(o.qps) || math.IsInf(o.qps, 0) || o.qps > math.MaxFloat32 || float32(o.qps) == 0 {
		return fmt.Errorf("--kube-api-qps must be positive, finite, and representable as float32")
	}
	if o.burst < 1 {
		return fmt.Errorf("--kube-api-burst must be >= 1")
	}
	return nil
}

func (o kubeAPIOptions) apply(config *rest.Config) {
	config.QPS = float32(o.qps)
	config.Burst = o.burst
}
