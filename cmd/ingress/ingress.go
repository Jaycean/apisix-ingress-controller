// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements.  See the NOTICE file distributed with
// this work for additional information regarding copyright ownership.
// The ASF licenses this file to You under the Apache License, Version 2.0
// (the "License"); you may not use this file except in compliance with
// the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package ingress

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/api7/ingress-controller/pkg/metrics"
	api6Informers "github.com/gxthrj/apisix-ingress-types/pkg/client/informers/externalversions"
	"github.com/spf13/cobra"

	"github.com/api7/ingress-controller/pkg/api"
	"github.com/api7/ingress-controller/pkg/config"
	"github.com/api7/ingress-controller/pkg/ingress/controller"
	"github.com/api7/ingress-controller/pkg/kube"
	"github.com/api7/ingress-controller/pkg/log"
	"github.com/api7/ingress-controller/pkg/seven/conf"
)

func dief(template string, args ...interface{}) {
	if !strings.HasSuffix(template, "\n") {
		template += "\n"
	}
	fmt.Fprintf(os.Stderr, template, args...)
	os.Exit(1)
}

func waitForSignal(stopCh chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	log.Infof("signal %d (%s) received", sig, sig.String())
	close(stopCh)
}

// NewIngressCommand creates the ingress sub command for apisix-ingress-controller.
func NewIngressCommand() *cobra.Command {
	var configPath string
	cfg := config.NewDefaultConfig()

	cmd := &cobra.Command{
		Use: "ingress [flags]",
		Long: `launch the ingress controller

You can run apisix-ingress-controller from configuration file or command line options,
if you run it from configuration file, other command line options will be ignored.

Run from configuration file:

    apisix-ingress-controller ingress --config-path /path/to/config.json

Both json and yaml are supported as the configuration file format.

Run from command line options:

    apisix-ingress-controller ingress --apisix-base-url http://apisix-service:9180/apisix/admin --kubeconfig /path/to/kubeconfig

If you run apisix-ingress-controller outside the Kubernetes cluster, --kubeconfig option (or kubeconfig item in configuration file) should be specified explicitly,
or if you run it inside cluster, leave it alone and in-cluster configuration will be discovered and used.

Before you run apisix-ingress-controller, be sure all related resources, like CRDs (ApisixRoute, ApisixUpstream and etc),
the apisix cluster and others are created`,
		Run: func(cmd *cobra.Command, args []string) {
			if configPath != "" {
				c, err := config.NewConfigFromFile(configPath)
				if err != nil {
					dief("failed to initialize configuration: %s", err)
				}
				cfg = c
			}
			logger, err := log.NewLogger(
				log.WithLogLevel(cfg.LogLevel),
				log.WithOutputFile(cfg.LogOutput),
			)
			if err != nil {
				dief("failed to initialize logging: %s", err)
			}
			log.DefaultLogger = logger
			log.Info("apisix ingress controller started")

			data, err := json.MarshalIndent(cfg, "", "\t")
			if err != nil {
				dief("failed to show configuration: %s", string(data))
			}
			log.Info("use configuration\n", string(data))

			// TODO: Move these logics to the inside of pkg/ingress/controller.
			conf.SetBaseUrl(cfg.APISIX.BaseURL)
			if err := kube.InitInformer(cfg); err != nil {
				dief("failed to initialize kube informers: %s", err)
			}

			// TODO: logics about metrics should be moved inside ingress controller,
			// after we  refactoring it.
			podName := os.Getenv("POD_NAME")
			podNamespace := os.Getenv("POD_NAMESPACE")
			if podNamespace == "" {
				podNamespace = "default"
			}

			collector := metrics.NewPrometheusCollector(podName, podNamespace)
			collector.ResetLeader(true)

			kubeClientSet := kube.GetKubeClient()
			apisixClientset := kube.GetApisixClient()
			sharedInformerFactory := api6Informers.NewSharedInformerFactory(apisixClientset, 0)
			stop := make(chan struct{})
			c := &controller.Api6Controller{
				KubeClientSet:             kubeClientSet,
				Api6ClientSet:             apisixClientset,
				SharedInformerFactory:     sharedInformerFactory,
				CoreSharedInformerFactory: kube.CoreSharedInformerFactory,
				Stop:                      stop,
			}
			epInformer := c.CoreSharedInformerFactory.Core().V1().Endpoints()
			kube.EndpointsInformer = epInformer
			// endpoint
			c.Endpoint()
			go c.CoreSharedInformerFactory.Start(stop)

			// ApisixRoute
			c.ApisixRoute()
			// ApisixUpstream
			c.ApisixUpstream()
			// ApisixService
			c.ApisixService()
			// ApisixTLS
			c.ApisixTLS()

			go func() {
				time.Sleep(time.Duration(10) * time.Second)
				c.SharedInformerFactory.Start(stop)
			}()

			srv, err := api.NewServer(cfg)
			if err != nil {
				dief("failed to create API Server: %s", err)
			}

			// TODO add sync.WaitGroup
			go func() {
				if err := srv.Run(stop); err != nil {
					dief("failed to launch API Server: %s", err)
				}
			}()

			waitForSignal(stop)
			log.Info("apisix ingress controller exited")
		},
	}

	cmd.PersistentFlags().StringVar(&configPath, "config-path", "", "configuration file path for apisix-ingress-controller")
	cmd.PersistentFlags().StringVar(&cfg.LogLevel, "log-level", "info", "error log level")
	cmd.PersistentFlags().StringVar(&cfg.LogOutput, "log-output", "stderr", "error log output file")
	cmd.PersistentFlags().StringVar(&cfg.HTTPListen, "http-listen", ":8080", "the HTTP Server listen address")
	cmd.PersistentFlags().BoolVar(&cfg.EnableProfiling, "enable-profiling", true, "enable profiling via web interface host:port/debug/pprof")
	cmd.PersistentFlags().StringVar(&cfg.Kubernetes.Kubeconfig, "kubeconfig", "", "Kubernetes configuration file (by default in-cluster configuration will be used)")
	cmd.PersistentFlags().DurationVar(&cfg.Kubernetes.ResyncInterval.Duration, "resync-interval", time.Minute, "the controller resync (with Kubernetes) interval, the minimum resync interval is 30s")
	cmd.PersistentFlags().StringVar(&cfg.APISIX.BaseURL, "apisix-base-url", "", "the base URL for APISIX admin api / manager api")
	cmd.PersistentFlags().StringVar(&cfg.APISIX.AdminKey, "apisix-admin-key", "", "admin key used for the authorization of APISIX admin api / manager api")

	return cmd
}
