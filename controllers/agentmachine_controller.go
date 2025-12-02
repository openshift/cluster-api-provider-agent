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
	"strconv"
	"strings"
	"time"

	ignitionapi "github.com/coreos/ignition/v2/config/v3_1/types"
	"github.com/go-openapi/swag"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	aimodels "github.com/openshift/assisted-service/models"
	capiproviderv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	openshiftconditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	clusterv1beta1conditions "sigs.k8s.io/cluster-api/util/deprecated/v1beta1/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// AgentMachineFinalizerName is the finalizer name for the AgentMachine
	AgentMachineFinalizerName = "agentmachine." + aiv1beta1.Group + "/deprovision"
	// AgentMachineRefLabelKey is the label key so an Agent can have a reference to an AgentMachine
	AgentMachineRefLabelKey = "agentMachineRef"
	// AgentMachineRefNamespace is the namespace so an Agent can have a reference to an AgentMachine's namespace
	AgentMachineRefNamespace = "agentMachineRefNamespace"

	machineDeleteHookName = clusterv1.PreTerminateDeleteHookAnnotationPrefix + "/agentmachine"
)

// AgentMachineReconciler reconciles a AgentMachine object
type AgentMachineReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logrus.FieldLogger
	AgentClient client.Client
	APIReader   client.Reader
}

//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=agent-install.openshift.io,resources=agents,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *AgentMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := r.Log.WithFields(
		logrus.Fields{
			"agent_machine":           req.Name,
			"agent_machine_namespace": req.Namespace,
		})

	log.Info("AgentMachine Reconcile start")

	agentMachine := &capiproviderv1.AgentMachine{}
	if err := r.Get(ctx, req.NamespacedName, agentMachine); err != nil {
		log.WithError(err).Errorf("Failed to get agentMachine %s", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	patchHelper, err := patch.NewHelper(agentMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if rerr := patchHelper.Patch(ctx, agentMachine); rerr != nil {
			log.WithError(err).Errorf("failed patching agentMachine")
		}
		log.Info("AgentMachine Reconcile ended")
	}()

	machine, err := r.getMachine(ctx, agentMachine)
	if err != nil {
		// It's possible that the machine delete hook was removed but the finalizer failed to remove for some reason
		// In this case the machine would be gone, but the agent machine would not be able to be removed
		// Check for the finalizer in this case and remove it
		if !agentMachine.DeletionTimestamp.IsZero() {
			log.Infof("Removing finalizer on agent machine %s/%s with missing machine", agentMachine.Namespace, agentMachine.Name)
			if finalizerError := r.removeFinalizer(agentMachine); finalizerError != nil {
				log.Error(finalizerError)
			}
		}
		log.WithError(err).Error("failed to get owner machine")
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on AgentMachine")
		return ctrl.Result{}, r.updateStatus(ctx, log, agentMachine, nil)
	}

	res, err := r.handleDeletionHook(ctx, log, agentMachine, machine)
	if res != nil || err != nil {
		return *res, err
	}

	if paused := agentMachine.Annotations[clusterv1.PausedAnnotation]; paused == "true" {
		log.Info("Skipping reconcile of AgentMachine since it is paused")
		return ctrl.Result{}, nil
	}

	machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, err := r.processBootstrapDataSecret(ctx, log, machine, agentMachine.Status.Ready)
	if err != nil {
		return ctrl.Result{}, err
	}

	// If the AgentMachine is ready, we have nothing to do
	if agentMachine.Status.Ready {
		return ctrl.Result{}, nil
	}

	agentCluster, err := r.getAgentCluster(ctx, log, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	if agentCluster.Status.ClusterDeploymentRef.Name == "" {
		err = fmt.Errorf("no cluster deployment reference on agentCluster %s", agentCluster.GetName())
		log.Warning(err.Error())
		return ctrl.Result{}, r.updateStatus(ctx, log, agentMachine, err)
	}

	// If the AgentMachine doesn't have an agent, find one and set the agentRef
	if agentMachine.Status.AgentRef == nil {
		var foundAgent *aiv1beta1.Agent
		foundAgent, err = r.findAgent(ctx, log, agentMachine, agentCluster.Status.ClusterDeploymentRef, machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders)
		if foundAgent == nil || err != nil {
			return ctrl.Result{}, r.updateStatus(ctx, log, agentMachine, err)
		}
		r.updateAgentMachineWithFoundAgent(log, agentMachine, foundAgent)
	}

	// If the AgentMachine has an agent, check its conditions and update ready/error
	return ctrl.Result{}, r.updateStatus(ctx, log, agentMachine, nil)
}

func (r *AgentMachineReconciler) getMachine(ctx context.Context, agentMachine *capiproviderv1.AgentMachine) (*clusterv1.Machine, error) {
	machine := &clusterv1.Machine{}
	if agentMachine.ObjectMeta.OwnerReferences != nil {
		for _, owner := range agentMachine.ObjectMeta.OwnerReferences {
			if owner.Kind == "Machine" && owner.APIVersion == clusterv1.GroupVersion.String() {
				err := r.Get(ctx, types.NamespacedName{Namespace: agentMachine.Namespace, Name: owner.Name}, machine)
				if err != nil {
					return nil, err
				}
				return machine, nil
			}
		}
	}
	return nil, nil
}

func (r *AgentMachineReconciler) removeFinalizer(agentMachine *capiproviderv1.AgentMachine) error {
	if funk.ContainsString(agentMachine.GetFinalizers(), AgentMachineFinalizerName) {
		controllerutil.RemoveFinalizer(agentMachine, AgentMachineFinalizerName)
	}
	return nil
}

func (r *AgentMachineReconciler) removeHookAndFinalizer(ctx context.Context, machine *clusterv1.Machine, agentMachine *capiproviderv1.AgentMachine) error {
	annotations := machine.GetAnnotations()
	if _, haveMachineHookAnnotation := annotations[machineDeleteHookName]; haveMachineHookAnnotation {
		delete(annotations, machineDeleteHookName)
		machine.SetAnnotations(annotations)
		if err := r.Update(ctx, machine); err != nil {
			return fmt.Errorf("failed to remove machine delete hook annotation for machine %s/%s: %w", machine.Namespace, machine.Name, err)
		}
	}
	return r.removeFinalizer(agentMachine)
}

func (r *AgentMachineReconciler) handleDeletionHook(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1.AgentMachine, machine *clusterv1.Machine) (*ctrl.Result, error) {
	// set delete hook if not present and machine not being deleted
	annotations := machine.GetAnnotations()
	if _, haveMachineHookAnnotation := annotations[machineDeleteHookName]; !haveMachineHookAnnotation && machine.DeletionTimestamp == nil {
		if annotations == nil {
			annotations = make(map[string]string)
		}
		log.Info("Adding machine delete hook annotation")
		annotations[machineDeleteHookName] = ""
		machine.SetAnnotations(annotations)
		if err := r.Update(ctx, machine); err != nil {
			log.WithError(err).Error("failed to add machine delete hook annotation")
			return &ctrl.Result{}, err
		}
	}

	// Also set a finalizer so the agent machine can't be removed before the machine
	if agentMachine.ObjectMeta.DeletionTimestamp.IsZero() && !funk.ContainsString(agentMachine.GetFinalizers(), AgentMachineFinalizerName) {
		controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
	}

	// Skip agent unbind if AgentMachine is paused
	if paused := agentMachine.Annotations[clusterv1.PausedAnnotation]; paused == "true" {
		if agentMachine.ObjectMeta.DeletionTimestamp.IsZero() {
			log.Info("AgentMachine paused, but not being deleted yet")
			return nil, nil
		}
		log.Info("Skipping unbinding agent since agent machine is paused. Removing machine delete hook annotation")
		if err := r.removeHookAndFinalizer(ctx, machine, agentMachine); err != nil {
			log.Error(err)
			return &ctrl.Result{}, err
		}
		return &ctrl.Result{}, nil
	}

	// return if the machine is not waiting on this hook
	cond := clusterv1beta1conditions.Get(machine, clusterv1.PreTerminateDeleteHookSucceededCondition)
	if cond == nil {
		if !agentMachine.DeletionTimestamp.IsZero() {
			log.Warnf("Not starting agent machine removal until machine %s/%s pauses for delete hook", machine.Namespace, machine.Name)
		}
		return nil, nil
	}

	// If the hook was already processed and removed ensure the finalizer is removed and return
	if cond.Status == corev1.ConditionTrue {
		if removeFinalizerError := r.removeFinalizer(agentMachine); removeFinalizerError != nil {
			log.Error(removeFinalizerError)
			return &ctrl.Result{}, removeFinalizerError
		}
		return &ctrl.Result{}, nil
	}

	log.Infof("Machine is waiting on delete hook %s", clusterv1.PreTerminateDeleteHookSucceededCondition)
	if agentMachine.Status.AgentRef == nil {
		log.Info("Removing machine delete hook annotation - agent ref is nil")
		if removeHookAndFinalizerError := r.removeHookAndFinalizer(ctx, machine, agentMachine); removeHookAndFinalizerError != nil {
			log.Error(removeHookAndFinalizerError)
			return &ctrl.Result{}, removeHookAndFinalizerError
		}
		return &ctrl.Result{}, nil
	}

	agent, err := r.getAgent(ctx, log, agentMachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Infof("Failed to get agent %s. assuming the agent no longer exists", agentMachine.Status.AgentRef.Name)
			if hookErr := r.removeHookAndFinalizer(ctx, machine, agentMachine); hookErr != nil {
				log.Error(hookErr)
				return &ctrl.Result{}, hookErr
			}
			return &ctrl.Result{}, nil
		}
		log.WithError(err).Errorf("Failed to get agent %s", agentMachine.Status.AgentRef.Name)
		return &ctrl.Result{}, err
	}

	if funk.Contains(agent.ObjectMeta.Labels, AgentMachineRefLabelKey) || agent.Spec.ClusterDeploymentName != nil {
		r.Log.Info("Removing ClusterDeployment ref to unbind Agent")
		delete(agent.ObjectMeta.Labels, AgentMachineRefLabelKey)
		delete(agent.ObjectMeta.Annotations, AgentMachineRefNamespace)
		agent.Spec.MachineConfigPool = ""
		agent.Spec.IgnitionEndpointTokenReference = nil
		agent.Spec.IgnitionEndpointHTTPHeaders = nil
		agent.Spec.ClusterDeploymentName = nil
		if err := r.Update(ctx, agent); err != nil {
			log.WithError(err).Error("failed to remove the Agent's ClusterDeployment ref")
			return &ctrl.Result{}, err
		}
	}

	// Remove the hook when either the host is back to some kind of unbound state, reclaim fails, or is not enabled
	removeHookStates := []string{
		aimodels.HostStatusDiscoveringUnbound,
		aimodels.HostStatusKnownUnbound,
		aimodels.HostStatusDisconnectedUnbound,
		aimodels.HostStatusInsufficientUnbound,
		aimodels.HostStatusDisabledUnbound,
		aimodels.HostStatusUnbindingPendingUserAction,
		aimodels.HostStatusError,
	}
	if funk.Contains(removeHookStates, agent.Status.DebugInfo.State) {
		log.Infof("Removing machine delete hook annotation for agent in status %s", agent.Status.DebugInfo.State)
		if err := r.removeHookAndFinalizer(ctx, machine, agentMachine); err != nil {
			log.Error(err)
			return &ctrl.Result{}, err
		}
		return &ctrl.Result{}, nil
	}
	log.Infof("Waiting for agent %s to reboot into discovery", agent.Name)
	return &ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *AgentMachineReconciler) getCluster(ctx context.Context, machine *clusterv1.Machine) (*clusterv1.Cluster, error) {
	cluster := &clusterv1.Cluster{}
	if machine.ObjectMeta.Labels[clusterv1.ClusterNameLabel] == "" {
		return nil, errors.New("machine is missing cluster label")
	}
	err := r.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.ObjectMeta.Labels[clusterv1.ClusterNameLabel]}, cluster)
	if err != nil {
		return nil, err
	}
	return cluster, nil
}

func (r *AgentMachineReconciler) getAgentCluster(ctx context.Context, log logrus.FieldLogger, machine *clusterv1.Machine) (*capiproviderv1.AgentCluster, error) {
	cluster, err := r.getCluster(ctx, machine)
	if err != nil {
		log.WithError(err).Warn("failed to get cluster from metadata")
		return nil, err
	}

	agentClusterRef := types.NamespacedName{Name: cluster.Spec.InfrastructureRef.Name, Namespace: cluster.Namespace}
	agentCluster := &capiproviderv1.AgentCluster{}
	if err := r.Get(ctx, agentClusterRef, agentCluster); err != nil {
		log.WithError(err).Errorf("Failed to get agentCluster %s", agentClusterRef)
		return nil, err
	}

	return agentCluster, nil
}

func (r *AgentMachineReconciler) findAgent(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1.AgentMachine,
	clusterDeploymentRef capiproviderv1.ClusterDeploymentReference, machineConfigPool string,
	ignitionTokenSecretRef *aiv1beta1.IgnitionEndpointTokenReference, ignitionEndpointHTTPHeaders map[string]string) (*aiv1beta1.Agent, error) {

	// In the event this is a restored hub cluster, there will already be Agents
	// that have been attached to this AgentMachine, so we want to reassociate them
	foundAgent, err := r.findAgentWithAgentMachineLabel(ctx, log, agentMachine)
	if err != nil {
		log.WithError(err).Error("failed while finding agents")
		return nil, err
	}
	if foundAgent != nil {
		log.Infof("Found agent with AgentMachine label: %s/%s", foundAgent.Namespace, foundAgent.Name)
		return foundAgent, nil
	}

	var selector labels.Selector
	if agentMachine.Spec.AgentLabelSelector != nil {
		selector, err = metav1.LabelSelectorAsSelector(agentMachine.Spec.AgentLabelSelector)
		if err != nil {
			log.WithError(err).Error("failed to convert label selector to selector")
			return nil, err
		}
	} else {
		selector = labels.NewSelector()
	}
	requirement, _ := labels.NewRequirement(AgentMachineRefLabelKey, selection.DoesNotExist, []string{})
	selector = selector.Add(*requirement)

	agents := &aiv1beta1.AgentList{}
	if err = r.AgentClient.List(ctx, agents, &client.ListOptions{LabelSelector: selector}); err != nil {
		log.WithError(err).Error("failed to list agents")
		return nil, err
	}

	// Find an agent that is unbound and whose validations pass
	for i := 0; i < len(agents.Items) && foundAgent == nil; i++ {
		if isValidAgent(&agents.Items[i]) {
			foundAgent = &agents.Items[i]
			log.Infof("Found agent to associate with AgentMachine: %s/%s", foundAgent.Namespace, foundAgent.Name)
			err = r.updateFoundAgent(ctx, log, agentMachine, foundAgent, clusterDeploymentRef, machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders)
			if err != nil {
				// If we failed to update the agent then it might have already been taken, try the others
				log.WithError(err).Infof("failed to update found agent, trying other agents")
				foundAgent = nil
			} else {
				break
			}
		}
	}

	if foundAgent == nil {
		log.Info("Failed to own any available Agent")
		return nil, nil
	}

	return foundAgent, nil
}

func getAgentMachineRefLabel(agentMachine *capiproviderv1.AgentMachine) string {
	return agentMachine.Name
}

// When we find an agent, we add a label to it in case we're interrupted before we can set agentMachine.Status.AgentRef
// Here we look for such an agent, and if we find one, set the AgentRef.
func (r *AgentMachineReconciler) findAgentWithAgentMachineLabel(ctx context.Context, log logrus.FieldLogger,
	agentMachine *capiproviderv1.AgentMachine) (*aiv1beta1.Agent, error) {

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{AgentMachineRefLabelKey: getAgentMachineRefLabel(agentMachine)}}
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		log.WithError(err).Error("failed to convert label selector to selector")
		return nil, client.IgnoreNotFound(err)
	}

	agents := &aiv1beta1.AgentList{}
	if err := r.AgentClient.List(ctx, agents, &client.ListOptions{LabelSelector: selector}); err != nil {
		log.WithError(err).Error("failed to list agents")
		return nil, client.IgnoreNotFound(err)
	}

	if len(agents.Items) == 0 {
		return nil, nil
	}

	return &agents.Items[0], nil
}

func (r *AgentMachineReconciler) updateAgentMachineWithFoundAgent(log logrus.FieldLogger, agentMachine *capiproviderv1.AgentMachine, agent *aiv1beta1.Agent) {
	log.Infof("Updating AgentMachine to reference Agent %s/%s", agent.Namespace, agent.Name)
	agentMachine.Spec.ProviderID = swag.String("agent://" + agent.Name)
	agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Name: agent.Name, Namespace: agent.Namespace}
	agentMachine.Status.Addresses = getAddresses(agent)
}

func (r *AgentMachineReconciler) updateFoundAgent(ctx context.Context, log logrus.FieldLogger,
	agentMachine *capiproviderv1.AgentMachine, agent *aiv1beta1.Agent,
	clusterDeploymentRef capiproviderv1.ClusterDeploymentReference, machineConfigPool string,
	ignitionTokenSecretRef *aiv1beta1.IgnitionEndpointTokenReference, ignitionEndpointHTTPHeaders map[string]string) error {

	log.Infof("Updating Agent %s/%s to be referenced by AgentMachine", agent.Namespace, agent.Name)
	if agent.ObjectMeta.Labels == nil {
		agent.ObjectMeta.Labels = make(map[string]string)
	}
	if agent.ObjectMeta.Annotations == nil {
		agent.ObjectMeta.Annotations = make(map[string]string)
	}
	agent.ObjectMeta.Labels[AgentMachineRefLabelKey] = getAgentMachineRefLabel(agentMachine)
	agent.ObjectMeta.Annotations[AgentMachineRefNamespace] = agentMachine.GetNamespace()
	agent.Spec.ClusterDeploymentName = &aiv1beta1.ClusterReference{Namespace: clusterDeploymentRef.Namespace, Name: clusterDeploymentRef.Name}
	agent.Spec.MachineConfigPool = machineConfigPool
	agent.Spec.IgnitionEndpointTokenReference = ignitionTokenSecretRef
	agent.Spec.IgnitionEndpointHTTPHeaders = ignitionEndpointHTTPHeaders

	if err := r.AgentClient.Update(ctx, agent); err != nil {
		log.WithError(err).Errorf("failed to update found Agent %s", agent.Name)
		return err
	}
	return nil
}

func (r *AgentMachineReconciler) processBootstrapDataSecret(ctx context.Context, log logrus.FieldLogger,
	machine *clusterv1.Machine, agentMachineReady bool) (string, *aiv1beta1.IgnitionEndpointTokenReference, map[string]string, error) {

	machineConfigPool := ""
	var ignitionTokenSecretRef *aiv1beta1.IgnitionEndpointTokenReference
	ignitionEndpointHTTPHeaders := make(map[string]string)

	if machine.Spec.Bootstrap.DataSecretName == nil {
		log.Info("No data secret, continuing")
		return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, nil
	}

	// For now we assume that if we have bootstrap data then it is an ignition config containing the ignition source and token.
	bootstrapDataSecret := &corev1.Secret{}
	bootstrapDataSecretRef := types.NamespacedName{Namespace: machine.Namespace, Name: *machine.Spec.Bootstrap.DataSecretName}
	if err := r.Get(ctx, bootstrapDataSecretRef, bootstrapDataSecret); err != nil {
		log.WithError(err).Errorf("Failed to get user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
		return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, err
	}
	if err := ensureSecretLabel(ctx, r.AgentClient, bootstrapDataSecret); err != nil {
		log.WithError(err).Warnf("Failed to label secret %s/%s for backup", bootstrapDataSecret.Name, bootstrapDataSecret.Namespace)
	}

	ignitionConfig := &ignitionapi.Config{}
	if err := json.Unmarshal(bootstrapDataSecret.Data["value"], ignitionConfig); err != nil {
		log.WithError(err).Errorf("Failed to unmarshal user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
		return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, err
	}

	if len(ignitionConfig.Ignition.Config.Merge) != 1 {
		log.Errorf("expected one ignition source in secret %s but found %d", *machine.Spec.Bootstrap.DataSecretName, len(ignitionConfig.Ignition.Config.Merge))
		return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, errors.New("did not find one ignition source as expected")
	}

	ignitionSource := ignitionConfig.Ignition.Config.Merge[0]
	machineConfigPool = (*ignitionSource.Source)[strings.LastIndex((*ignitionSource.Source), "/")+1:]

	token := ""
	for _, header := range ignitionSource.HTTPHeaders {
		if header.Name == "Authorization" {
			expectedPrefix := "Bearer "
			if !strings.HasPrefix(*header.Value, expectedPrefix) {
				log.Errorf("did not find expected prefix for bearer token in user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
				return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, errors.New("did not find expected prefix for bearer token")
			}
			token = (*header.Value)[len(expectedPrefix):]
		} else {
			ignitionEndpointHTTPHeaders[header.Name] = *header.Value
		}

	}

	ignitionTokenSecretName := fmt.Sprintf("agent-%s", *machine.Spec.Bootstrap.DataSecretName)
	ignitionTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: machine.Namespace,
			Name:      ignitionTokenSecretName,
			Labels: map[string]string{
				"agent-install.openshift.io/watch": "true",
				BackupLabel:                        BackupLabelValue,
			},
		},
		Data: map[string][]byte{"ignition-token": []byte(token)},
	}
	ignitionTokenSecretRef = &aiv1beta1.IgnitionEndpointTokenReference{Namespace: machine.Namespace, Name: ignitionTokenSecretName}
	// TODO: Use a dedicated secret per host and Delete the secret upon cleanup,
	err := r.Client.Create(ctx, ignitionTokenSecret)
	if err == nil && agentMachineReady {
		log.Warnf("ignition token secret %s should not be deleted", ignitionTokenSecret)
	}
	if apierrors.IsAlreadyExists(err) {
		log.Infof("ignitionTokenSecret %s already exists, updating secret content",
			fmt.Sprintf("agent-%s", *machine.Spec.Bootstrap.DataSecretName))
		err = r.Client.Update(ctx, ignitionTokenSecret)
	}
	if err != nil {
		log.WithError(err).Error("Failed to create ignitionTokenSecret")
		return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, err
	}

	return machineConfigPool, ignitionTokenSecretRef, ignitionEndpointHTTPHeaders, nil
}

// An Agent is considered valid only if it meets all of the following:
// 1. it is not bound to a ClusterDeployment
// 2. it is still connected (contacting the hub)
// 3. it is approved
// 4. its validations are passing
func isValidAgent(agent *aiv1beta1.Agent) bool {
	var valid, notBound, connected bool

	for _, condition := range agent.Status.Conditions {
		switch condition.Type {
		case aiv1beta1.BoundCondition:
			notBound = condition.Status == "False"
		case aiv1beta1.ConnectedCondition:
			connected = condition.Status == "True"
		case aiv1beta1.ValidatedCondition:
			valid = condition.Status == "True"
		}
	}
	return agent.Spec.Approved && valid && connected && notBound
}

func (r *AgentMachineReconciler) updateStatus(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1.AgentMachine, err error) error {
	agentMachine.Status.Ready = false
	conditionPassed := setAgentReservedCondition(agentMachine, err)
	if !conditionPassed {
		clusterv1beta1conditions.MarkFalse(agentMachine, capiproviderv1.AgentSpecSyncedCondition, capiproviderv1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		clusterv1beta1conditions.MarkFalse(agentMachine, capiproviderv1.AgentValidatedCondition, capiproviderv1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		clusterv1beta1conditions.MarkFalse(agentMachine, capiproviderv1.AgentRequirementsMetCondition, capiproviderv1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		clusterv1beta1conditions.MarkFalse(agentMachine, capiproviderv1.InstalledCondition, capiproviderv1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		err = r.setStatus(agentMachine)
		return err
	}

	agent, getErr := r.getAgent(ctx, log, agentMachine)
	if getErr != nil {
		return getErr
	}

	if err = r.ensureAgentLabeled(ctx, agentMachine, agent); err != nil {
		log.WithError(err).Errorf("failed to label Agent %s with AgentMachineRef", agent.Name)
		return err
	}
	setConditionByAgentCondition(agentMachine, agent, capiproviderv1.AgentSpecSyncedCondition, aiv1beta1.SpecSyncedCondition, clusterv1.ConditionSeverityError)
	setConditionByAgentCondition(agentMachine, agent, capiproviderv1.AgentValidatedCondition, aiv1beta1.ValidatedCondition, clusterv1.ConditionSeverityError)
	setConditionByAgentCondition(agentMachine, agent, capiproviderv1.AgentRequirementsMetCondition, aiv1beta1.RequirementsMetCondition, clusterv1.ConditionSeverityError)
	setConditionByAgentCondition(agentMachine, agent, capiproviderv1.InstalledCondition, aiv1beta1.InstalledCondition, clusterv1.ConditionSeverityInfo)
	err = r.setStatus(agentMachine)
	return err
}

func (r *AgentMachineReconciler) getAgent(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1.AgentMachine) (*aiv1beta1.Agent, error) {
	agent := &aiv1beta1.Agent{}
	agentRef := types.NamespacedName{Name: agentMachine.Status.AgentRef.Name, Namespace: agentMachine.Status.AgentRef.Namespace}
	if err := r.APIReader.Get(ctx, agentRef, agent); err != nil {
		log.WithError(err).Errorf("Failed to get agent %s", agentRef)
		return nil, err
	}
	return agent, nil
}
func (r *AgentMachineReconciler) ensureAgentLabeled(ctx context.Context, agentMachine *capiproviderv1.AgentMachine, agent *aiv1beta1.Agent) error {
	agentMachineRef := agent.Labels[AgentMachineRefLabelKey]
	if agentMachineRef != "" && agentMachineRef == getAgentMachineRefLabel(agentMachine) {
		return nil
	}

	patch := client.MergeFrom(agent.DeepCopy())
	if agent.ObjectMeta.Labels == nil {
		agent.ObjectMeta.Labels = make(map[string]string)
	}
	agent.ObjectMeta.Labels[AgentMachineRefLabelKey] = getAgentMachineRefLabel(agentMachine)
	if err := r.Patch(ctx, agent, patch); err != nil {
		return err
	}
	return nil
}

func setConditionByAgentCondition(agentMachine *capiproviderv1.AgentMachine, agent *aiv1beta1.Agent,
	agentMachineConditionType clusterv1.ConditionType, agentConditionType openshiftconditionsv1.ConditionType,
	failSeverity clusterv1.ConditionSeverity) bool {
	agentCondition := openshiftconditionsv1.FindStatusCondition(agent.Status.Conditions, agentConditionType)
	if agentCondition == nil {
		clusterv1beta1conditions.MarkFalse(agentMachine, agentMachineConditionType, "", failSeverity, "")
		return false
	}
	if agentCondition.Status == "True" {
		clusterv1beta1conditions.MarkTrue(agentMachine, agentMachineConditionType)
		return true
	}
	// We have a special case where failed installation is higher severity
	if agentCondition.Type == aiv1beta1.InstalledCondition && agentCondition.Reason == aiv1beta1.InstallationFailedReason {
		failSeverity = clusterv1.ConditionSeverityError
	}
	msg := agentCondition.Message
	clusterv1beta1conditions.MarkFalse(agentMachine, agentMachineConditionType, agentCondition.Reason, failSeverity, "%s", msg)
	return false
}

func setAgentReservedCondition(agentMachine *capiproviderv1.AgentMachine, err error) bool {
	if agentMachine.Status.AgentRef == nil {
		if err == nil {
			clusterv1beta1conditions.MarkFalse(agentMachine, capiproviderv1.AgentReservedCondition, capiproviderv1.NoSuitableAgentsReason, clusterv1.ConditionSeverityWarning, "")
		} else {
			msg := err.Error()
			clusterv1beta1conditions.MarkFalse(agentMachine, capiproviderv1.AgentReservedCondition, capiproviderv1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "%s", msg)
		}
		return false
	}

	clusterv1beta1conditions.MarkTrue(agentMachine, capiproviderv1.AgentReservedCondition)
	return true
}

func (r *AgentMachineReconciler) setStatus(agentMachine *capiproviderv1.AgentMachine) error {
	clusterv1beta1conditions.SetSummary(agentMachine,
		clusterv1beta1conditions.WithConditions(capiproviderv1.AgentReservedCondition,
			capiproviderv1.AgentSpecSyncedCondition,
			capiproviderv1.AgentValidatedCondition,
			capiproviderv1.AgentRequirementsMetCondition,
			capiproviderv1.InstalledCondition,
		),
		clusterv1beta1conditions.WithStepCounterIf(agentMachine.ObjectMeta.DeletionTimestamp.IsZero()),
		clusterv1beta1conditions.WithStepCounter())

	agentMachine.Status.Ready, _ = strconv.ParseBool(string(clusterv1beta1conditions.Get(agentMachine, clusterv1.ReadyCondition).Status))
	return nil
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

func (r *AgentMachineReconciler) mapMachineToAgentMachine(ctx context.Context, machine client.Object) []reconcile.Request {
	log := r.Log.WithFields(
		logrus.Fields{
			"machine":           machine.GetName(),
			"machine_namespace": machine.GetNamespace(),
		},
	)

	amList := &capiproviderv1.AgentMachineList{}
	opts := &client.ListOptions{
		Namespace: machine.GetNamespace(),
	}
	if err := r.List(ctx, amList, opts); err != nil {
		log.Debugf("failed to list agent machines")
		return []reconcile.Request{}
	}

	for _, agentMachine := range amList.Items {
		for _, ref := range agentMachine.OwnerReferences {
			gv, err := schema.ParseGroupVersion(ref.APIVersion)
			if err != nil {
				continue
			}
			if ref.Kind == "Machine" && gv.Group == clusterv1.GroupVersion.Group && ref.Name == machine.GetName() {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{
					Namespace: agentMachine.Namespace,
					Name:      agentMachine.Name,
				}}}
			}
		}
	}

	return []reconcile.Request{}
}

func (r *AgentMachineReconciler) mapAgentToAgentMachine(ctx context.Context, a client.Object) []reconcile.Request {
	log := r.Log.WithFields(
		logrus.Fields{
			"agent":           a.GetName(),
			"agent_namespace": a.GetNamespace(),
		})

	amList := &capiproviderv1.AgentMachineList{}
	opts := &client.ListOptions{}
	mappedAgent := false

	// The annotation will only be present on an Agent that's mapped to an AgentMachine
	namespace, ok := a.GetAnnotations()[AgentMachineRefNamespace]
	if ok {
		opts.Namespace = namespace
		mappedAgent = true
	}

	if err := r.List(ctx, amList, opts); err != nil {
		log.WithError(err).Error("failed to list agent machines")
		return []reconcile.Request{}
	}

	reply := make([]reconcile.Request, 0, len(amList.Items))
	if mappedAgent {
		// If the Agent is mapped to an AgentMachine, return only that AgentMachine
		agent := a
		for _, agentMachine := range amList.Items {
			if agentMachine.Status.AgentRef != nil && agentMachine.Status.AgentRef.Namespace == agent.GetNamespace() &&
				agentMachine.Status.AgentRef.Name == agent.GetName() {
				reply = append(reply, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: agentMachine.Namespace,
					Name:      agentMachine.Name,
				}})
				break
			}
		}
	} else {
		// If the Agent isn't mapped to an AgentMachine and it's "valid" for use, return any AgentMachines
		// that don't have an Agent mapped to them yet
		agent := &aiv1beta1.Agent{}
		if err := r.Get(ctx, types.NamespacedName{Name: a.GetName(), Namespace: a.GetNamespace()}, agent); err != nil {
			return []reconcile.Request{}
		}
		if !isValidAgent(agent) {
			return []reconcile.Request{}
		}
		for _, agentMachine := range amList.Items {
			if agentMachine.Status.AgentRef == nil {
				reply = append(reply, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: agentMachine.Namespace,
					Name:      agentMachine.Name,
				}})
			}
		}
	}

	return reply
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentMachineReconciler) SetupWithManager(mgr ctrl.Manager, agentNamespace string) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentmachine-controller").
		For(&capiproviderv1.AgentMachine{}).
		Watches(&aiv1beta1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.mapAgentToAgentMachine)).
		Watches(&clusterv1.Machine{}, handler.EnqueueRequestsFromMapFunc(r.mapMachineToAgentMachine)).
		Complete(r)
}
