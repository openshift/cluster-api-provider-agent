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
	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	openshiftconditionsv1 "github.com/openshift/custom-resource-status/conditions/v1"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	defaultRequeueAfterOnError                 = 10 * time.Second
	defaultRequeueWaitingForAgentToBeInstalled = 20 * time.Second
	defaultRequeueWaitingForAvailableAgent     = 30 * time.Second
	AgentMachineFinalizerName                  = "agentmachine." + aiv1beta1.Group + "/deprovision"
	AgentMachineRefLabelKey                    = "agentMachineRef"
)

// AgentMachineReconciler reconciles a AgentMachine object
type AgentMachineReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Log         logrus.FieldLogger
	AgentClient client.Client
}

//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=agent-install.openshift.io,resources=agents,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

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

	machine, err := clusterutil.GetOwnerMachine(ctx, r.Client, agentMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on AgentMachine")
		return r.updateStatus(ctx, log, agentMachine, ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, nil)
	}

	agentCluster, err := r.getAgentCluster(ctx, log, machine)
	if err != nil {
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	if agentCluster.Status.ClusterDeploymentRef.Name == "" {
		return r.updateStatus(ctx, log, agentMachine, ctrl.Result{RequeueAfter: defaultRequeueWaitingForAvailableAgent}, nil)
	}

	machineConfigPool, ignitionTokenSecretRef, err := r.processBootstrapDataSecret(ctx, log, machine)
	if err != nil {
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	// If the AgentMachine doesn't have an agent, find one and set the agentRef
	if agentMachine.Status.AgentRef == nil {
		foundAgent, result, err := r.findAgent(ctx, log, agentMachine, agentCluster.Status.ClusterDeploymentRef, machineConfigPool, ignitionTokenSecretRef)
		if err != nil || foundAgent == nil {
			// Update the status, err == nil indicates that we didn't find an agent because one didn't exist, not due to a retryable error
			return r.updateStatus(ctx, log, agentMachine, result, err)
		}

		err = r.updateAgentMachineWithFoundAgent(ctx, log, agentMachine, foundAgent)
		if err != nil {
			log.WithError(err).Error("failed to update AgentMachine with found agent")
			return r.updateStatus(ctx, log, agentMachine, ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err)
		} else {
			return r.updateStatus(ctx, log, agentMachine, ctrl.Result{Requeue: true}, nil)
		}
	}

	// If the AgentMachine has an agent, check its conditions and update ready/error
	return r.updateStatus(ctx, log, agentMachine, ctrl.Result{Requeue: true}, nil)
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
				agent, err := r.getAgent(ctx, log, agentMachine)
				if err != nil {
					if apierrors.IsNotFound(err) {
						log.WithError(err).Infof("Failed to get agent %s. assuming the agent no longer exists", agentMachine.Status.AgentRef.Name)
					} else {
						log.WithError(err).Errorf("Failed to get agent %s", agentMachine.Status.AgentRef.Name)
						return &ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
					}
				} else {
					// Remove the AgentMachineRefLabel and set the clusterDeployment to nil
					delete(agent.ObjectMeta.Labels, AgentMachineRefLabelKey)
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

func (r *AgentMachineReconciler) getAgentCluster(ctx context.Context, log logrus.FieldLogger, machine *clusterv1.Machine) (*capiproviderv1alpha1.AgentCluster, error) {
	cluster, err := clusterutil.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return nil, err
	}

	agentClusterRef := types.NamespacedName{Name: cluster.Spec.InfrastructureRef.Name, Namespace: cluster.Spec.InfrastructureRef.Namespace}
	agentCluster := &capiproviderv1alpha1.AgentCluster{}
	if err := r.Get(ctx, agentClusterRef, agentCluster); err != nil {
		log.WithError(err).Errorf("Failed to get agentCluster %s", agentClusterRef)
		return nil, err
	}

	return agentCluster, nil
}

func (r *AgentMachineReconciler) findAgent(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine,
	clusterDeploymentRef capiproviderv1alpha1.ClusterDeploymentReference, machineConfigPool string,
	ignitionTokenSecretRef *aiv1beta1.IgnitionEndpointTokenReference) (*aiv1beta1.Agent, ctrl.Result, error) {

	foundAgent, err := r.findAgentWithAgentMachineLabel(ctx, log, agentMachine)
	if err != nil {
		log.WithError(err).Error("failed while finding agents")
		return nil, ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}
	if foundAgent != nil {
		log.Infof("Found agent with AgentMachine label: %s/%s", foundAgent.Namespace, foundAgent.Name)
		return foundAgent, ctrl.Result{Requeue: true}, nil
	}

	var selector labels.Selector
	if agentMachine.Spec.AgentLabelSelector != nil {
		selector, err = metav1.LabelSelectorAsSelector(agentMachine.Spec.AgentLabelSelector)
		if err != nil {
			log.WithError(err).Error("failed to convert label selector to selector")
			return nil, ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
		}
	} else {
		selector = labels.NewSelector()
	}
	requirement, _ := labels.NewRequirement(AgentMachineRefLabelKey, selection.DoesNotExist, []string{})
	selector = selector.Add(*requirement)

	agents := &aiv1beta1.AgentList{}
	if err = r.AgentClient.List(ctx, agents, &client.ListOptions{LabelSelector: selector}); err != nil {
		log.WithError(err).Error("failed to list agents")
		return nil, ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	// Find an agent that is unbound and whose validations pass
	for i := 0; i < len(agents.Items) && foundAgent == nil; i++ {
		if isValidAgent(&agents.Items[i], agentMachine) {
			foundAgent = &agents.Items[i]
			log.Infof("Found agent to associate with AgentMachine: %s/%s", foundAgent.Namespace, foundAgent.Name)
			err = r.updateFoundAgent(ctx, log, agentMachine, foundAgent, clusterDeploymentRef, machineConfigPool, ignitionTokenSecretRef)
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
		return nil, ctrl.Result{RequeueAfter: defaultRequeueWaitingForAvailableAgent}, nil
	}

	return foundAgent, ctrl.Result{Requeue: true}, nil
}

func getAgentMachineRefLabel(agentMachine *capiproviderv1alpha1.AgentMachine) string {
	return string(agentMachine.GetUID())
}

// When we find an agent, we add a label to it in case we're interrupted before we can set agentMachine.Status.AgentRef
// Here we look for such an agent, and if we find one, set the AgentRef.
func (r *AgentMachineReconciler) findAgentWithAgentMachineLabel(ctx context.Context, log logrus.FieldLogger,
	agentMachine *capiproviderv1alpha1.AgentMachine) (*aiv1beta1.Agent, error) {

	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{AgentMachineRefLabelKey: getAgentMachineRefLabel(agentMachine)}}
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		log.WithError(err).Error("failed to convert label selector to selector")
		return nil, err
	}

	agents := &aiv1beta1.AgentList{}
	if err := r.AgentClient.List(ctx, agents, &client.ListOptions{LabelSelector: selector}); err != nil {
		log.WithError(err).Error("failed to list agents")
		return nil, err
	}

	if len(agents.Items) == 0 {
		return nil, nil
	}

	return &agents.Items[0], nil
}

func (r *AgentMachineReconciler) updateAgentMachineWithFoundAgent(ctx context.Context, log logrus.FieldLogger,
	agentMachine *capiproviderv1alpha1.AgentMachine, agent *aiv1beta1.Agent) error {

	log.Infof("Updating AgentMachine to reference Agent %s/%s", agent.Namespace, agent.Name)
	agentMachine.Spec.ProviderID = swag.String("agent://" + agent.Name)
	if err := r.Update(ctx, agentMachine); err != nil {
		log.WithError(err).Error("failed to update AgentMachine Spec")
		return err
	}

	// We will perform the actual update in updateStatus()
	agentMachine.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: agent.Namespace, Name: agent.Name}
	agentMachine.Status.Addresses = getAddresses(agent)
	agentMachine.Status.Ready = false

	return nil
}

func (r *AgentMachineReconciler) updateFoundAgent(ctx context.Context, log logrus.FieldLogger,
	agentMachine *capiproviderv1alpha1.AgentMachine, agent *aiv1beta1.Agent,
	clusterDeploymentRef capiproviderv1alpha1.ClusterDeploymentReference, machineConfigPool string,
	ignitionTokenSecretRef *aiv1beta1.IgnitionEndpointTokenReference) error {

	log.Infof("Updating Agent %s/%s to be referenced by AgentMachine", agent.Namespace, agent.Name)
	if agent.ObjectMeta.Labels == nil {
		agent.ObjectMeta.Labels = make(map[string]string)
	}
	agent.ObjectMeta.Labels[AgentMachineRefLabelKey] = getAgentMachineRefLabel(agentMachine)
	agent.Spec.ClusterDeploymentName = &aiv1beta1.ClusterReference{Namespace: clusterDeploymentRef.Namespace, Name: clusterDeploymentRef.Name}
	agent.Spec.MachineConfigPool = machineConfigPool
	agent.Spec.IgnitionEndpointTokenReference = ignitionTokenSecretRef

	if err := r.AgentClient.Update(ctx, agent); err != nil {
		log.WithError(err).Errorf("failed to update found Agent %s", agent.Name)
		return err
	}
	return nil
}

func (r *AgentMachineReconciler) processBootstrapDataSecret(ctx context.Context, log logrus.FieldLogger,
	machine *clusterv1.Machine) (string, *aiv1beta1.IgnitionEndpointTokenReference, error) {

	machineConfigPool := ""
	var ignitionTokenSecretRef *aiv1beta1.IgnitionEndpointTokenReference

	if machine.Spec.Bootstrap.DataSecretName == nil {
		log.Info("No data secret, continuing")
		return machineConfigPool, ignitionTokenSecretRef, nil
	}

	// For now we assume that if we have bootstrap data then it is an ignition config containing the ignition source and token.
	bootstrapDataSecret := &corev1.Secret{}
	bootstrapDataSecretRef := types.NamespacedName{Namespace: machine.Namespace, Name: *machine.Spec.Bootstrap.DataSecretName}
	if err := r.Get(ctx, bootstrapDataSecretRef, bootstrapDataSecret); err != nil {
		log.WithError(err).Errorf("Failed to get user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
		return machineConfigPool, ignitionTokenSecretRef, err
	}

	ignitionConfig := &ignitionapi.Config{}
	if err := json.Unmarshal(bootstrapDataSecret.Data["value"], ignitionConfig); err != nil {
		log.WithError(err).Errorf("Failed to unmarshal user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
		return machineConfigPool, ignitionTokenSecretRef, err
	}

	if len(ignitionConfig.Ignition.Config.Merge) != 1 {
		log.Errorf("expected one ignition source in secret %s but found %d", *machine.Spec.Bootstrap.DataSecretName, len(ignitionConfig.Ignition.Config.Merge))
		return machineConfigPool, ignitionTokenSecretRef, errors.New("did not find one ignition source as expected")
	}

	ignitionSource := ignitionConfig.Ignition.Config.Merge[0]
	machineConfigPool = (*ignitionSource.Source)[strings.LastIndex((*ignitionSource.Source), "/")+1:]

	token := ""
	for _, header := range ignitionSource.HTTPHeaders {
		if header.Name != "Authorization" {
			continue
		}
		expectedPrefix := "Bearer "
		if !strings.HasPrefix(*header.Value, expectedPrefix) {
			log.Errorf("did not find expected prefix for bearer token in user-data secret %s", *machine.Spec.Bootstrap.DataSecretName)
			return machineConfigPool, ignitionTokenSecretRef, errors.New("did not find expected prefix for bearer token")
		}
		token = (*header.Value)[len(expectedPrefix):]
	}

	ignitionTokenSecretName := fmt.Sprintf("agent-%s", *machine.Spec.Bootstrap.DataSecretName)
	ignitionTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: machine.Namespace,
			Name:      ignitionTokenSecretName,
			Labels:    map[string]string{"agent-install.openshift.io/watch": "true"},
		},
		Data: map[string][]byte{"ignition-token": []byte(token)},
	}
	ignitionTokenSecretRef = &aiv1beta1.IgnitionEndpointTokenReference{Namespace: machine.Namespace, Name: ignitionTokenSecretName}
	// TODO: Use a dedicated secret per host and Delete the secret upon cleanup,
	err := r.Client.Create(ctx, ignitionTokenSecret)
	if apierrors.IsAlreadyExists(err) {
		log.Infof("ignitionTokenSecret %s already exits, updating secret content",
			fmt.Sprintf("agent-%s", *machine.Spec.Bootstrap.DataSecretName))
		err = r.Client.Update(ctx, ignitionTokenSecret)
	}
	if err != nil {
		log.WithError(err).Error("Failed to create ignitionTokenSecret")
		return machineConfigPool, ignitionTokenSecretRef, err
	}

	return machineConfigPool, ignitionTokenSecretRef, nil
}

func isValidAgent(agent *aiv1beta1.Agent, agentMachine *capiproviderv1alpha1.AgentMachine) bool {
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

	return true
}

func (r *AgentMachineReconciler) updateStatus(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine, origRes ctrl.Result, origErr error) (ctrl.Result, error) {
	installationInProgress := false
	conditionPassed := setAgentReservedCondition(agentMachine, origErr)
	if !conditionPassed {
		conditions.MarkFalse(agentMachine, capiproviderv1alpha1.AgentSpecSyncedCondition, capiproviderv1alpha1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		conditions.MarkFalse(agentMachine, capiproviderv1alpha1.AgentValidatedCondition, capiproviderv1alpha1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		conditions.MarkFalse(agentMachine, capiproviderv1alpha1.AgentRequirementsMetCondition, capiproviderv1alpha1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		conditions.MarkFalse(agentMachine, capiproviderv1alpha1.InstalledCondition, capiproviderv1alpha1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, "Agent not yet reserved")
		return r.setStatus(ctx, log, agentMachine, origRes, origErr, installationInProgress)
	}

	agent, err := r.getAgent(ctx, log, agentMachine)
	if err != nil {
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, err
	}

	setConditionByAgentCondition(agentMachine, agent, capiproviderv1alpha1.AgentSpecSyncedCondition, aiv1beta1.SpecSyncedCondition, clusterv1.ConditionSeverityError)
	setConditionByAgentCondition(agentMachine, agent, capiproviderv1alpha1.AgentValidatedCondition, aiv1beta1.ValidatedCondition, clusterv1.ConditionSeverityError)
	setConditionByAgentCondition(agentMachine, agent, capiproviderv1alpha1.AgentRequirementsMetCondition, aiv1beta1.RequirementsMetCondition, clusterv1.ConditionSeverityError)
	installationInProgress = !setConditionByAgentCondition(agentMachine, agent, capiproviderv1alpha1.InstalledCondition, aiv1beta1.InstalledCondition, clusterv1.ConditionSeverityInfo)
	return r.setStatus(ctx, log, agentMachine, origRes, origErr, installationInProgress)
}

func (r *AgentMachineReconciler) getAgent(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine) (*aiv1beta1.Agent, error) {
	agent := &aiv1beta1.Agent{}
	agentRef := types.NamespacedName{Name: agentMachine.Status.AgentRef.Name, Namespace: agentMachine.Status.AgentRef.Namespace}
	if err := r.AgentClient.Get(ctx, agentRef, agent); err != nil {
		log.WithError(err).Errorf("Failed to get agent %s", agentRef)
		return nil, err
	}
	return agent, nil
}

func setConditionByAgentCondition(agentMachine *capiproviderv1alpha1.AgentMachine, agent *aiv1beta1.Agent,
	agentMachineConditionType clusterv1.ConditionType, agentConditionType openshiftconditionsv1.ConditionType,
	failSeverity clusterv1.ConditionSeverity) bool {
	agentCondition := openshiftconditionsv1.FindStatusCondition(agent.Status.Conditions, agentConditionType)
	if agentCondition == nil {
		conditions.MarkFalse(agentMachine, agentMachineConditionType, "", failSeverity, "")
		return false
	}
	if agentCondition.Status == "True" {
		conditions.MarkTrue(agentMachine, agentMachineConditionType)
		return true
	}
	// We have a special case where failed installation is higher severity
	if agentCondition.Type == aiv1beta1.InstalledCondition && agentCondition.Reason == aiv1beta1.InstallationFailedReason {
		failSeverity = clusterv1.ConditionSeverityError
	}
	conditions.MarkFalse(agentMachine, agentMachineConditionType, agentCondition.Reason, failSeverity, agentCondition.Message)
	return false
}

func setAgentReservedCondition(agentMachine *capiproviderv1alpha1.AgentMachine, origErr error) bool {
	if agentMachine.Status.AgentRef == nil {
		if origErr == nil {
			conditions.MarkFalse(agentMachine, capiproviderv1alpha1.AgentReservedCondition, capiproviderv1alpha1.NoSuitableAgentsReason, clusterv1.ConditionSeverityWarning, "")
		} else {
			conditions.MarkFalse(agentMachine, capiproviderv1alpha1.AgentReservedCondition, capiproviderv1alpha1.AgentNotYetFoundReason, clusterv1.ConditionSeverityInfo, origErr.Error())
		}
		return false
	}

	conditions.MarkTrue(agentMachine, capiproviderv1alpha1.AgentReservedCondition)
	return true
}

func (r *AgentMachineReconciler) setStatus(ctx context.Context, log logrus.FieldLogger, agentMachine *capiproviderv1alpha1.AgentMachine, origRes ctrl.Result, origErr error, installationInProgress bool) (ctrl.Result, error) {
	conditions.SetSummary(agentMachine,
		conditions.WithConditions(capiproviderv1alpha1.AgentReservedCondition,
			capiproviderv1alpha1.AgentSpecSyncedCondition,
			capiproviderv1alpha1.AgentValidatedCondition,
			capiproviderv1alpha1.AgentRequirementsMetCondition,
			capiproviderv1alpha1.InstalledCondition,
		),
		conditions.WithStepCounterIf(agentMachine.ObjectMeta.DeletionTimestamp.IsZero()),
		conditions.WithStepCounter())

	agentMachine.Status.Ready, _ = strconv.ParseBool(string(conditions.Get(agentMachine, clusterv1.ReadyCondition).Status))

	if updateErr := r.Status().Update(ctx, agentMachine); updateErr != nil {
		log.WithError(updateErr).Error("failed to update AgentMachine Status")
		return ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}, nil
	}
	if agentMachine.Status.Ready {
		// No need to requeue in case the agentMachine is ready
		return ctrl.Result{}, nil
	}
	if installationInProgress && origErr == nil {
		return ctrl.Result{RequeueAfter: defaultRequeueWaitingForAgentToBeInstalled}, nil
	}

	return origRes, origErr
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

// SetupWithManager sets up the controller with the Manager.
func (r *AgentMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capiproviderv1alpha1.AgentMachine{}).
		Complete(r)
}
