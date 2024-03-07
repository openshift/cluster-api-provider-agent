/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"os"

	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	capiproviderv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	"github.com/openshift/cluster-api-provider-agent/controllers"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(capiproviderv1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var watchNamespace string
	var agentsNamespace string
	var agentClient client.Client
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&watchNamespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")
	flag.StringVar(&agentsNamespace, "agent-namespace", "",
		"Namespace that the controller watches to list Agents objects. If unspecified, the controller watches for Agents objects across all namespaces.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if watchNamespace != "" {
		setupLog.Info("Watching cluster-api objects only in namespace for reconciliation", "namespace", watchNamespace)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	scheme = controllers.GetKubeClientSchemes(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "7605f49b.agent-install.openshift.io",
		Cache: cache.Options{
			DefaultFieldSelector: fields.OneTermEqualSelector("metadata.namespace", watchNamespace),
			ByObject: map[client.Object]cache.ByObject{
				&aiv1beta1.Agent{}: {Field: fields.OneTermEqualSelector("metadata.namespace", agentsNamespace)},
			},
		},
	},
	)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	agentClient = mgr.GetClient()

	if agentsNamespace != "" {
		setupLog.Info("Watching Agents objects only in namespace for reconciliation", "agent-namespace", agentsNamespace)
		agentMgr, err2 := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
			Scheme:         scheme,
			LeaderElection: false,
			Cache: cache.Options{
				ByObject: map[client.Object]cache.ByObject{
					&aiv1beta1.Agent{}: {Field: fields.OneTermEqualSelector("metadata.namespace", agentsNamespace)},
				},
			},
		})
		if err2 != nil {
			setupLog.Error(err, "unable to start Agent manager")
			os.Exit(1)
		}
		agentClient = agentMgr.GetClient()
		go func() {
			setupLog.Info("starting Agent manager")
			if err2 = agentMgr.Start(context.Background()); err2 != nil {
				setupLog.Error(err, "problem running Agent manager")
				os.Exit(1)
			}
		}()
	}

	logger := logrus.New()
	logger.SetReportCaller(true)
	if err = (&controllers.AgentMachineReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Log:         logger,
		AgentClient: agentClient,
		APIReader:   mgr.GetAPIReader(),
	}).SetupWithManager(mgr, agentsNamespace); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentMachine")
		os.Exit(1)
	}

	if err = (&controllers.AgentClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    logger,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AgentCluster")
		os.Exit(1)
	}

	if err = (&controllers.NodeProviderIDReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Log:                 logger,
		RemoteClientHandler: controllers.NewRemoteClient(mgr.GetClient(), mgr.GetScheme()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NodeProviderID")
		os.Exit(1)
	}

	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
