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
	"fmt"
	"strings"
	"time"

	"github.com/go-openapi/swag"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	capiproviderv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/hive/apis/hive/v1/agent"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterutilv1 "sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	agentClusterDependenciesWaitTime = 5 * time.Second
)

// AgentClusterReconciler reconciles a AgentCluster object
type AgentClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logrus.FieldLogger
}

type ControlPlane struct {
	BaseDomain        string
	ClusterName       string
	PullSecret        string
	KubeConfig        string
	KubeadminPassword string
}

//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=capi-provider.agent-install.openshift.io,resources=agentclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=hive.openshift.io,resources=clusterdeployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=extensions.hive.openshift.io,resources=agentclusterinstalls,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedcontrolplanes,verbs=get;list;watch;

func (r *AgentClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithFields(
		logrus.Fields{
			"agent_cluster":           req.Name,
			"agent_cluster_namespace": req.Namespace,
		})

	defer func() {
		log.Info("AgentCluster Reconcile ended")
	}()
	log.Info("AgentCluster Reconcile start")

	agentCluster := &capiproviderv1.AgentCluster{}
	if err := r.Get(ctx, req.NamespacedName, agentCluster); err != nil {
		log.WithError(err).Errorf("Failed to get agentCluster %s", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If the agentCluster has no reference to a ClusterDeployment, create one
	if agentCluster.Status.ClusterDeploymentRef.Name == "" {
		return r.createClusterDeployment(ctx, log, agentCluster)
	}
	clusterDeployment := &hivev1.ClusterDeployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: agentCluster.Status.ClusterDeploymentRef.Namespace, Name: agentCluster.Status.ClusterDeploymentRef.Name}, clusterDeployment)
	if err != nil {
		log.WithError(err).Error("Failed to get ClusterDeployment")
		return ctrl.Result{}, err
	}

	err = r.ensureAgentClusterInstall(ctx, log, clusterDeployment, agentCluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !agentCluster.Spec.ControlPlaneEndpoint.IsValid() {
		log.Info("Waiting for agentCluster controlPlaneEndpoint")
		return ctrl.Result{RequeueAfter: agentClusterDependenciesWaitTime}, nil
	}

	// If the agentCluster has references a ClusterDeployment, sync from its status
	return r.updateClusterStatus(ctx, log, agentCluster)
}

func getNestedStringObject(log logrus.FieldLogger, obj *unstructured.Unstructured, baseFieldName string, fields ...string) (string, bool, error) {
	value, ok, err := unstructured.NestedString(obj.UnstructuredContent(), fields...)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("failed to get %s", baseFieldName))
		log.Error(err)
		return value, ok, err
	}
	if !ok {
		log.Infof("%s not found", baseFieldName)
		return value, ok, err
	}
	return value, ok, nil
}

func (r *AgentClusterReconciler) getControlPlane(ctx context.Context, log logrus.FieldLogger,
	agentCluster *capiproviderv1.AgentCluster) (*ControlPlane, error) {

	log.Info("Getting control plane")
	// Fetch the CAPI Cluster.
	cluster, err := clusterutilv1.GetOwnerCluster(ctx, r.Client, agentCluster.ObjectMeta)
	if err != nil {
		return nil, err
	}
	if cluster == nil {
		log.Infof("Waiting for Cluster Controller to set OwnerRef on AgentCluster %s %s", agentCluster.Name, agentCluster.Namespace)
		return nil, nil
	}

	if cluster.Spec.ControlPlaneRef == nil {
		log.Info("Waiting for Cluster to have OwnerRef on Control Plane for AgentCluster %s %s", agentCluster.Name, agentCluster.Namespace)
		return nil, nil
	}

	obj := clusterutilv1.ObjectReferenceToUnstructured(*cluster.Spec.ControlPlaneRef)
	key := client.ObjectKey{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	if err = r.Client.Get(ctx, key, obj); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve %s external object %q/%q", obj.GetKind(), key.Namespace, key.Name)
	}

	var ok bool
	var controlPlane ControlPlane

	controlPlane.BaseDomain, ok, err = getNestedStringObject(log, obj, "base domain", "spec", "dns", "baseDomain")
	if err != nil || !ok {
		return nil, err
	}

	controlPlane.PullSecret, ok, err = getNestedStringObject(log, obj, "pull secret name", "spec", "pullSecret", "name")
	if err != nil || !ok {
		return nil, err
	}

	controlPlane.KubeConfig, ok, err = getNestedStringObject(log, obj, "kubeconfig", "status", "kubeConfig", "name")
	if err != nil || !ok {
		return nil, err
	}

	controlPlane.KubeadminPassword, ok, err = getNestedStringObject(log, obj, "kubeadmin password", "status", "kubeadminPassword", "name")
	if err != nil || !ok {
		log.WithError(err).Info("Failed to get kubeadmin password")
	}

	controlPlane.ClusterName = cluster.Spec.ControlPlaneRef.Name
	return &controlPlane, nil
}

func (r *AgentClusterReconciler) createClusterDeploymentObject(agentCluster *capiproviderv1.AgentCluster,
	controlPlane *ControlPlane) *hivev1.ClusterDeployment {
	var kubeadminPassword *corev1.LocalObjectReference
	if controlPlane.KubeadminPassword != "" {
		kubeadminPassword = &corev1.LocalObjectReference{
			Name: controlPlane.KubeadminPassword,
		}
	}
	clusterDeployment := &hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentCluster.Name,
			Namespace: agentCluster.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       agentCluster.Kind,
				APIVersion: agentCluster.APIVersion,
				Name:       agentCluster.Name,
				UID:        agentCluster.UID,
			}},
		},
		Spec: hivev1.ClusterDeploymentSpec{
			Installed:   true,
			ClusterName: controlPlane.ClusterName,
			Platform: hivev1.Platform{
				AgentBareMetal: &agent.BareMetalPlatform{},
			},
			ClusterInstallRef: &hivev1.ClusterInstallLocalReference{
				Kind:    "AgentClusterInstall",
				Group:   hiveext.Group,
				Version: hiveext.Version,
				Name:    agentCluster.Name,
			},
			BaseDomain:    controlPlane.BaseDomain,
			PullSecretRef: &corev1.LocalObjectReference{Name: controlPlane.PullSecret},
			ClusterMetadata: &hivev1.ClusterMetadata{
				ClusterID: string(agentCluster.OwnerReferences[0].UID),
				InfraID:   string(agentCluster.OwnerReferences[0].UID),
				AdminKubeconfigSecretRef: corev1.LocalObjectReference{
					Name: controlPlane.KubeConfig,
				},
				AdminPasswordSecretRef: kubeadminPassword,
			},
		},
	}

	return clusterDeployment
}

func (r *AgentClusterReconciler) createClusterDeployment(ctx context.Context, log logrus.FieldLogger, agentCluster *capiproviderv1.AgentCluster) (ctrl.Result, error) {
	controlPlane, err := r.getControlPlane(ctx, log, agentCluster)
	if err != nil || controlPlane == nil {
		return ctrl.Result{RequeueAfter: agentClusterDependenciesWaitTime}, err
	}

	log.Info("Creating clusterDeployment")
	clusterDeployment := r.createClusterDeploymentObject(agentCluster, controlPlane)

	r.labelControlPlaneSecrets(ctx, controlPlane, agentCluster.Namespace)

	agentCluster.Status.ClusterDeploymentRef.Name = clusterDeployment.Name
	agentCluster.Status.ClusterDeploymentRef.Namespace = clusterDeployment.Namespace
	if err = r.Client.Create(ctx, clusterDeployment); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.Warn("ClusterDeployment already exists")
		} else {
			log.WithError(err).Error("Failed to create ClusterDeployment")
			return ctrl.Result{}, err
		}
	}
	if err = r.Client.Status().Update(ctx, agentCluster); err != nil {
		log.WithError(err).Error("Failed to update status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *AgentClusterReconciler) ensureAgentClusterInstall(ctx context.Context, log logrus.FieldLogger, clusterDeployment *hivev1.ClusterDeployment, agentCluster *capiproviderv1.AgentCluster) error {
	log.Info("Setting AgentClusterInstall")
	agentClusterInstall := &hiveext.AgentClusterInstall{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: clusterDeployment.Namespace, Name: clusterDeployment.Name}, agentClusterInstall); err != nil {
		if apierrors.IsNotFound(err) {
			err = r.createAgentClusterInstall(ctx, log, clusterDeployment, agentCluster)
			if err != nil {
				log.WithError(err).Error("failed to create AgentClusterInstall")
				return err
			}
			return nil
		} else {
			log.WithError(err).Error("failed to get AgentClusterInstall")
			return err
		}
	}
	return nil
}

func (r *AgentClusterReconciler) createAgentClusterInstall(ctx context.Context, log logrus.FieldLogger, clusterDeployment *hivev1.ClusterDeployment, agentCluster *capiproviderv1.AgentCluster) error {
	log.Infof("Creating AgentClusterInstall for clusterDeployment: %s %s", clusterDeployment.Namespace, clusterDeployment.Name)
	agentClusterInstall := &hiveext.AgentClusterInstall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterDeployment.Name,
			Namespace: clusterDeployment.Namespace,
		},
		Spec: hiveext.AgentClusterInstallSpec{
			ClusterDeploymentRef: corev1.LocalObjectReference{Name: clusterDeployment.Name},
			ProvisionRequirements: hiveext.ProvisionRequirements{
				ControlPlaneAgents: 3,
			},
			Networking: hiveext.Networking{UserManagedNetworking: swag.Bool(true)},
		},
	}

	// IgnitionEndpoint can only be set at AgentClusterInstall create time
	if agentCluster.Spec.IgnitionEndpoint != nil {
		url := agentCluster.Spec.IgnitionEndpoint.Url
		agentClusterInstall.Spec.IgnitionEndpoint = &hiveext.IgnitionEndpoint{
			// Currently assume something like https://1.2.3.4:555/ignition, otherwise this will fail
			// TODO: Replace with something more robust
			Url: url[0:strings.LastIndex(url, "/")],
		}
		if agentCluster.Spec.IgnitionEndpoint.CaCertificateReference != nil {
			agentClusterInstall.Spec.IgnitionEndpoint.CaCertificateReference = &hiveext.CaCertificateReference{
				Namespace: agentCluster.Spec.IgnitionEndpoint.CaCertificateReference.Namespace,
				Name:      agentCluster.Spec.IgnitionEndpoint.CaCertificateReference.Name,
			}
			r.ensureSecretLabel(ctx, agentCluster.Spec.IgnitionEndpoint.CaCertificateReference.Name, agentCluster.Spec.IgnitionEndpoint.CaCertificateReference.Namespace)
		}
	}

	return r.Client.Create(ctx, agentClusterInstall)
}

func (r *AgentClusterReconciler) updateClusterStatus(ctx context.Context, log logrus.FieldLogger, agentCluster *capiproviderv1.AgentCluster) (ctrl.Result, error) {
	log.Infof("Updating agentCluster status according to %s", agentCluster.Status.ClusterDeploymentRef.Name)
	// Once the cluster have clusterDeploymentRef and ClusterInstallRef we should set the status to Ready
	agentCluster.Status.Ready = true
	if err := r.Status().Update(ctx, agentCluster); err != nil {
		log.WithError(err).Error("Failed to set ready status")
		return ctrl.Result{}, err

	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capiproviderv1.AgentCluster{}).
		Complete(r)
}

func (r *AgentClusterReconciler) labelControlPlaneSecrets(ctx context.Context, controlPlane *ControlPlane, namespace string) {
	if controlPlane != nil {
		if controlPlane.PullSecret != "" {
			r.ensureSecretLabel(ctx, controlPlane.PullSecret, namespace)
		}
		if controlPlane.KubeConfig != "" {
			r.ensureSecretLabel(ctx, controlPlane.KubeConfig, namespace)

		}
		if controlPlane.KubeadminPassword != "" {
			r.ensureSecretLabel(ctx, controlPlane.KubeadminPassword, namespace)
		}
	}
}

func (r *AgentClusterReconciler) ensureSecretLabel(ctx context.Context, name, namespace string) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err != nil {
		r.Log.WithError(err).Warnf("Couldn't find secret %s/%s in cluster", name, namespace)
	}
	if err := ensureSecretLabel(ctx, r.Client, secret); err != nil {
		r.Log.WithError(err).Warn("Failed labeling secret")
	}
}
