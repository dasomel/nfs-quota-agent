package main

import (
	"flag"
	"testing"

	"k8s.io/client-go/rest"
)

func TestKubeAPIOptions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		qps     float32
		burst   int
		invalid bool
	}{
		{name: "defaults", qps: 50, burst: 100},
		{name: "overrides", args: []string{"--kube-api-qps=20", "--kube-api-burst=40"}, qps: 20, burst: 40},
		{name: "fractional and smaller burst", args: []string{"--kube-api-qps=50.5", "--kube-api-burst=1"}, qps: 50.5, burst: 1},
		{name: "zero qps", args: []string{"--kube-api-qps=0"}, invalid: true},
		{name: "negative qps", args: []string{"--kube-api-qps=-1"}, invalid: true},
		{name: "nan", args: []string{"--kube-api-qps=NaN"}, invalid: true},
		{name: "infinite", args: []string{"--kube-api-qps=Inf"}, invalid: true},
		{name: "overflow", args: []string{"--kube-api-qps=1e40"}, invalid: true},
		{name: "underflow", args: []string{"--kube-api-qps=1e-50"}, invalid: true},
		{name: "zero burst", args: []string{"--kube-api-burst=0"}, invalid: true},
		{name: "negative burst", args: []string{"--kube-api-burst=-1"}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var options kubeAPIOptions
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			options.addFlags(fs)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			err := options.validate()
			if (err != nil) != tt.invalid {
				t.Fatalf("validate() = %v, invalid = %v", err, tt.invalid)
			}
			if tt.invalid {
				return
			}
			config := &rest.Config{Host: "https://example.invalid", QPS: 5, Burst: 10}
			options.apply(config)
			if config.QPS != tt.qps || config.Burst != tt.burst || config.Host != "https://example.invalid" {
				t.Fatalf("unexpected config: %+v", config)
			}
		})
	}
}
