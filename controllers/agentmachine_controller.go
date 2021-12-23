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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	ignitionapi "github.com/coreos/ignition/v2/config/v3_1/types"

	"github.com/go-openapi/swag"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clustererrors "sigs.k8s.io/cluster-api/errors"
	clusterutil "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
)

const (
	defaultRequeueAfterOnError                 = 10 * time.Second
	defaultRequeueWaitingForAgentToBeInstalled = 20 * time.Second
	defaultRequeueWaitingForAvailableAgent     = 30 * time.Second
	AgentMachineFinalizerName                  = "agentmachine." + aiv1beta1.Group + "/deprovision"
)

// AgentMachineReconciler reconciles a AgentMachine object
type AgentMachineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logrus.FieldLogger
}

//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=agent-install.openshift.io,resources=agents,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *AgentMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithFields(
		logrus.Fields{
			"agent_machine":           req.Name,
			"agent_machine_namespace": req.Namespace,
		})

	defer func() {
		log.Info("AgentMachine Reconcile ended")
	}()

	agentMachine := &capiproviderv1alpha1.AgentMachine{}
	if err := r.Get(ctx, req.NamespacedName, agentMachine); err != nil {
		log.WithError(err).Errorf("Failed to get agentMachine %s", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	res, err := r.handleDeletionFinalizer(ctx, log, agentMachine)
	if res != nil || err != nil {
		return *res, err
	}

	// If the AgentMachine is ready, we have nothing to do
	if agentMachine.Status.Ready {
		return ctrl.Result{}, nil
	}

	// If the AgentMachine doesn't have an agent, find one and set the agentRef
	if agentMachine.Status.AgentRef == nil {
		return r.findAgent(ctx, log, agentMachine)
	}

	agent := &aiv1beta1.Agent{}
	agentRef := types.NamespacedName{Name: agentMachine.Status.AgentRef.Name, Namespace: agentMachine.Status.AgentRef.Namespace}
	if err := r.Get(ctx, agentRef, agent); err != nil {
		log.WithError(err).Errorf("Failed to get agent %s", agentRef)
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	machine, err := clusterutil.GetOwnerMachine(ctx, r.Client, agentMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on AgentMachine")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, nil
	}

	if machine.Spec.Bootstrap.DataSecretName != nil && agent.Spec.MachineConfigPool == "" {
		return r.setAgentIgnitionEndpoint(ctx, log, agent, agentMachine, machine)
	}

	// If the AgentMachine has an Agent but the Agent doesn't reference the ClusterDeployment,
	// then set it. At this point we might find that the Agent is already bound and we'll need
	// to find a new one.
	if agent.Spec.ClusterDeploymentName == nil {
		return r.setAgentClusterDeploymentRef(ctx, log, agentMachine, agent)
	}

	// If the AgentMachine has an agent, check its conditions and update ready/error
	return r.updateAgentStatus(ctx, log, agentMachine, agent)
}

func (r *AgentMachineReconciler) handleDeletionFinalizer(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine) (*ctrl.Result, error) {
	if agentMachine.ObjectMeta.DeletionTimestamp.IsZero() { // AgentMachine not being deleted
		// Register a finalizer if it is absent.
		if !funk.ContainsString(agentMachine.GetFinalizers(), AgentMachineFinalizerName) {
			controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
			if err := r.Update(ctx, agentMachine); err != nil {
				log.WithError(err).Errorf("failed to add finalizer %s to resource %s %s", AgentMachineFinalizerName, agentMachine.Name, agentMachine.Namespace)
				return &ctrl.Result{Requeue: true}, err
			}
		}
	} else { // AgentMachine is being deleted
		r.Log.Info("Found deletion timestamp on AgentMachine")
		if funk.ContainsString(agentMachine.GetFinalizers(), AgentMachineFinalizerName) {
			// deletion finalizer found, unbind the Agent from the ClusterDeployment
			if agentMachine.Status.AgentRef != nil {
				r.Log.Info("Removing ClusterDeployment ref to unbind Agent")
				agent := &aiv1beta1.Agent{}
				agentRef := types.NamespacedName{Name: agentMachine.Status.AgentRef.Name, Namespace: agentMachine.Status.AgentRef.Namespace}
				err := r.Get(ctx, agentRef, agent)
				if err != nil {
					if apierrors.IsNotFound(err) {
						log.WithError(err).Infof("Failed to get agent %s. assuming the agent no longer exists", agentRef)
					} else {
						log.WithError(err).Errorf("Failed to get agent %s", agentRef)
						return &ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
					}
				} else {
					agent.Spec.ClusterDeploymentName = nil
					if err := r.Update(ctx, agent); err != nil {
						log.WithError(err).Error("failed to remove the Agent's ClusterDeployment ref")
						return &ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
					}
				}
			}

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(agentMachine, AgentMachineFinalizerName)
			if err := r.Update(ctx, agentMachine); err != nil {
				log.WithError(err).Errorf("failed to remove finalizer %s from resource %s %s", AgentMachineFinalizerName, agentMachine.Name, agentMachine.Namespace)
				return &ctrl.Result{Requeue: true}, err
			}
		}
		r.Log.Info("AgentMachine is ready for deletion")

		return &ctrl.Result{}, nil
	}

	return nil, nil
}

func (r *AgentMachineReconciler) findAgent(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine) (ctrl.Result, error) {
	var selector labels.Selector
	if agentMachine.Spec.AgentLabelSelector != nil {
		var err error
		selector, err = metav1.LabelSelectorAsSelector(agentMachine.Spec.AgentLabelSelector)
		if err != nil {
			log.WithError(err).Error("failed to convert label selector to selector")
			return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
		}
	} else {
		selector = labels.Everything()
	}

	agents := &aiv1beta1.AgentList{}
	if err := r.List(ctx, agents, &client.ListOptions{LabelSelector: selector}); err != nil {
		log.WithError(err).Error("failed to list agents")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	agentMachines := &capiproviderv1alpha1.AgentMachineList{}
	if err := r.List(ctx, agentMachines); err != nil {
		log.WithError(err).Error("failed to list agent machines")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}
	var foundAgent *aiv1beta1.Agent

	// Find an agent that is unbound and whose validations pass
	for i := 0; i < len(agents.Items) && foundAgent == nil; i++ {
		if isValidAgent(&agents.Items[i], agentMachine, agentMachines) {
			foundAgent = &agents.Items[i]
		}
	}

	if foundAgent == nil {
		log.Info("Did not find any available Agents")
		return ctrl.Result{RequeueAfter: defaultRequeueWaitingForAvailableAgent}, nil
	}

	log.Infof("Found agent to associate with AgentMachine: %s/%s", foundAgent.Namespace, foundAgent.Name)

	agentMachine.Spec.ProviderID = swag.String("agent://" + foundAgent.Name)
	if err := r.Update(ctx, agentMachine); err != nil {
		log.WithError(err).Error("failed to update AgentMachine Spec")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	agentMachine.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: foundAgent.Namespace, Name: foundAgent.Name}
	agentMachine.Status.Addresses = getAddresses(foundAgent)
	agentMachine.Status.Ready = false

	if err := r.Status().Update(ctx, agentMachine); err != nil {
		log.WithError(err).Error("failed to update AgentMachine Status")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

func (r *AgentMachineReconciler) setAgentIgnitionEndpoint(ctx context.Context, log logrus.FieldLogger, agent *aiv1beta1.Agent, agentMachine *capiproviderv1alpha1.AgentMachine, machine *clusterv1.Machine) (ctrl.Result, error) {
	log.Debug("Setting Ignition endpoint info")

	// For now we assume that we have bootstrap data that is an ignition config containing the ignition source and token.
	// We also assume that once set, it will not change, so we only need to reconcile this once.
	bootstrapDataSecret := &corev1.Secret{}
	bootstrapDataSecretRef := types.NamespacedName{Namespace: machine.Namespace, Name: *machine.Spec.Bootstrap.DataSecretName}
	if err := r.Get(ctx, bootstrapDataSecretRef, bootstrapDataSecret); err != nil {
		log.WithError(err).Errorf("Failed to get user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	ignitionConfig := &ignitionapi.Config{}
	if err := json.Unmarshal(bootstrapDataSecret.Data["value"], ignitionConfig); err != nil {
		log.WithError(err).Errorf("Failed to unmarshal user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	if len(ignitionConfig.Ignition.Config.Merge) != 1 {
		log.Errorf("expected one ignition source in secret %s but found %d", *machine.Spec.Bootstrap.DataSecretName, len(ignitionConfig.Ignition.Config.Merge))
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, errors.New("did not find one ignition source as expected")
	}

	ignitionSource := ignitionConfig.Ignition.Config.Merge[0]
	ignitionSourceSuffix := (*ignitionSource.Source)[strings.LastIndex((*ignitionSource.Source), "/")+1:]

	// If the MachineConfigPool was set then assume we already reconciled and can return
	if agent.Spec.MachineConfigPool == ignitionSourceSuffix {
		return ctrl.Result{Requeue: true}, nil
	}

	log.Infof("Setting MachineConfigPool to %s", agent.Spec.MachineConfigPool)
	agent.Spec.MachineConfigPool = ignitionSourceSuffix

	token := ""
	for _, header := range ignitionSource.HTTPHeaders {
		if header.Name != "Authorization" {
			continue
		}
		expectedPrefix := "Bearer "
		if !strings.HasPrefix(*header.Value, expectedPrefix) {
			log.Errorf("did not find expected prefix for bearer token in user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
			return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, errors.New("did not find expected prefix for bearer token")
		}
		token = (*header.Value)[len(expectedPrefix):]
	}

	ignitionTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: machine.Namespace,
			Name:      fmt.Sprintf("agent-%s", *machine.Spec.Bootstrap.DataSecretName),
			Labels:    map[string]string{"agent-install.openshift.io/watch": "true"},
		},
		Data: map[string][]byte{"ignition-token": []byte(token)},
	}
	// TODO Delete secret upon cleanup
	if err := r.Client.Create(ctx, ignitionTokenSecret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			log.WithError(err).Error("Failed to create ignitionTokenSecret")
			return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
		}
	}

	agent.Spec.IgnitionEndpointTokenReference = &aiv1beta1.IgnitionEndpointTokenReference{
		Namespace: ignitionTokenSecret.Namespace,
		Name:      ignitionTokenSecret.Name,
	}

	if agentUpdateErr := r.Update(ctx, agent); agentUpdateErr != nil {
		log.WithError(agentUpdateErr).Error("failed to update Agent with ignition endpoint")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, agentUpdateErr
	}
	log.Info("Successfully updated Ignition endpoint token and MachineConfigPool")

	return ctrl.Result{Requeue: true}, nil
}

func isValidAgent(agent *aiv1beta1.Agent, agentMachine *capiproviderv1alpha1.AgentMachine, agentMachines *capiproviderv1alpha1.AgentMachineList) bool {
	if !agent.Spec.Approved {
		return false
	}
	for _, condition := range agent.Status.Conditions {
		if condition.Type == aiv1beta1.BoundCondition && condition.Status != "False" {
			return false
		}
		if condition.Type == aiv1beta1.ValidatedCondition && condition.Status != "True" {
			return false
		}
	}

	if agent.Status.Inventory.Cpu.Count < int64(agentMachine.Spec.MinCPUs) {
		return false
	}
	if int(agent.Status.Inventory.Memory.PhysicalBytes/1024/1024) < int(agentMachine.Spec.MinMemoryMiB) {
		return false
	}

	// Make sure no other AgentMachine took this Agent already
	for _, agentMachinePtr := range agentMachines.Items {
		if agentMachinePtr.Status.AgentRef != nil && agentMachinePtr.Status.AgentRef.Namespace == agent.Namespace && agentMachinePtr.Status.AgentRef.Name == agent.Name {
			return false
		}
	}
	return true
}

func (r *AgentMachineReconciler) setAgentClusterDeploymentRef(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine, agent *aiv1beta1.Agent) (ctrl.Result, error) {
	clusterDeploymentRef, err := r.getClusterDeploymentFromAgentMachine(ctx, log, agentMachine)
	if err != nil {
		log.WithError(err).Error("Failed to find ClusterDeploymentRef")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}
	if clusterDeploymentRef == nil {
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, nil
	}

	agent.Spec.ClusterDeploymentName = &aiv1beta1.ClusterReference{Namespace: clusterDeploymentRef.Namespace, Name: clusterDeploymentRef.Name}

	if err := r.Update(ctx, agent); err != nil {
		log.WithError(err).Error("failed to update Agent with ClusterDeployment ref")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

func (r *AgentMachineReconciler) getClusterDeploymentFromAgentMachine(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine) (*capiproviderv1alpha1.ClusterDeploymentReference, error) {
	// AgentMachine -> CAPI Machine -> Cluster -> ClusterDeployment
	machine, err := clusterutil.GetOwnerMachine(ctx, r.Client, agentMachine.ObjectMeta)
	if err != nil {
		return nil, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on AgentMachine")
		return nil, nil
	}

	cluster, err := clusterutil.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return nil, nil
	}

	agentClusterRef := types.NamespacedName{Name: cluster.Spec.InfrastructureRef.Name, Namespace: cluster.Spec.InfrastructureRef.Namespace}
	agentCluster := &capiproviderv1alpha1.AgentCluster{}
	if err := r.Get(ctx, agentClusterRef, agentCluster); err != nil {
		log.WithError(err).Errorf("Failed to get agentCluster %s", agentClusterRef)
		return nil, err
	}

	return &agentCluster.Status.ClusterDeploymentRef, nil
}

func (r *AgentMachineReconciler) updateAgentStatus(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine, agent *aiv1beta1.Agent) (ctrl.Result, error) {
	log.Info("Updating agentMachine status")
	for _, condition := range agent.Status.Conditions {
		if condition.Type == aiv1beta1.InstalledCondition {
			if condition.Status == "True" {
				log.Info("Updating agentMachine status to Ready=true")
				agentMachine.Status.Ready = true
			} else if condition.Status == "False" {
				if condition.Reason == aiv1beta1.InstallationFailedReason {
					agentMachine.Status.FailureReason = (*clustererrors.MachineStatusError)(&condition.Reason)
					agentMachine.Status.FailureMessage = &condition.Message
				}
			}
			break
		}
	}

	if updateErr := r.Status().Update(ctx, agentMachine); updateErr != nil {
		log.WithError(updateErr).Error("failed to update AgentMachine Status")
		return ctrl.Result{Requeue: true}, nil
	}
	if agentMachine.Status.Ready {
		// No need to requeue in case the agentMachine is ready
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: defaultRequeueWaitingForAgentToBeInstalled}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capiproviderv1alpha1.AgentMachine{}).
		Complete(r)
}

func getAddresses(foundAgent *aiv1beta1.Agent) []clusterv1.MachineAddress {
	var machineAddresses []clusterv1.MachineAddress
	for _, iface := range foundAgent.Status.Inventory.Interfaces {
		if !iface.HasCarrier {
			continue
		}
		for _, addr := range iface.IPV4Addresses {
			machineAddresses = append(machineAddresses, clusterv1.MachineAddress{
				Type:    clusterv1.MachineExternalIP,
				Address: addr[:strings.LastIndex(addr, "/")],
			})
		}
	}
	// use requested hostname
	hostname := foundAgent.Spec.Hostname
	// in case the requested hostname is empty use the hostname from the status
	if hostname == "" {
		hostname = foundAgent.Status.Inventory.Hostname
	}
	machineAddresses = append(machineAddresses, clusterv1.MachineAddress{
		Type:    clusterv1.MachineInternalDNS,
		Address: hostname,
	})
	return machineAddresses
}
