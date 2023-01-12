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

package controllers

import (
	"context"
	"time"

	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultRequeue = 30 * time.Second
)

// NodeProviderIDReconciler reconciles a AgentMachine object
type NodeProviderIDReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logrus.FieldLogger
	RemoteClientHandler
}

//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines,verbs=get;list;watch
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/status,verbs=get
func (r *NodeProviderIDReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithFields(
		logrus.Fields{
			"agent_machine":           req.Name,
			"agent_machine_namespace": req.Namespace,
		})

	defer func() {
		log.Info("NodeProviderID Reconcile ended")
	}()
	log.Info("NodeProviderID Reconcile start")

	agentMachine := &capiproviderv1alpha1.AgentMachine{}
	if err := r.Get(ctx, req.NamespacedName, agentMachine); err != nil {
		log.WithError(err).Errorf("Failed to get agentMachine %s", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If the AgentMachine isn't ready, we have nothing to do
	if !agentMachine.Status.Ready {
		return ctrl.Result{RequeueAfter: defaultRequeue}, nil
	}

	if err := r.setNodeProviderID(ctx, log, agentMachine); err != nil {
		log.Infof("can't set node provider ID, error: %s", err)
		return ctrl.Result{RequeueAfter: defaultRequeue}, nil
	}
	return ctrl.Result{}, nil
}

func (r *NodeProviderIDReconciler) setNodeProviderID(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine) error {
	log.Debug("Setting node provider ID")
	nodeName := ""
	for _, address := range agentMachine.Status.Addresses {
		if corev1.NodeAddressType(address.Type) == corev1.NodeInternalDNS {
			nodeName = address.Address
			break
		}
	}
	if nodeName == "" {
		return errors.New("failed to find agentMachine InternalDNS")
	}
	remoteClient, err := r.RemoteClientHandler.GetRemoteClient(ctx, agentMachine.Namespace)
	if err != nil {
		return errors.WithMessage(err, "failed to get remote client")
	}
	node := corev1.Node{}
	err = remoteClient.Get(ctx, types.NamespacedName{Name: nodeName, Namespace: ""}, &node)
	if err != nil {
		return errors.Errorf("failed to find node with name %s", nodeName)
	}
	if node.Spec.ProviderID != "" && node.Spec.ProviderID != *agentMachine.Spec.ProviderID {
		return errors.Errorf("node %s already providerID is set (%s) and doesn't match the agentMachine providerID (%s)",
			nodeName, node.Spec.ProviderID, *agentMachine.Spec.ProviderID)
	}
	if node.Spec.ProviderID == *agentMachine.Spec.ProviderID {
		log.Debugf("node: %s providerID is already set, nothing to do", node.Name)
		return nil
	}
	node.Spec.ProviderID = *agentMachine.Spec.ProviderID
	if err = remoteClient.Update(ctx, &node); err != nil {
		return errors.WithMessagef(err, "failed to update node %s providerID %s", nodeName, *agentMachine.Spec.ProviderID)
	}
	log.Infof("Successfully set providerID: %s on node: %s", node.Spec.ProviderID, node.Name)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeProviderIDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capiproviderv1alpha1.AgentMachine{}).
		Complete(r)
}
