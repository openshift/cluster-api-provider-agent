package controllers

import (
	"context"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/hive/apis/hive/v1/agent"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	_ = hivev1.AddToScheme(scheme.Scheme)
	_ = hiveext.AddToScheme(scheme.Scheme)
	_ = capiproviderv1alpha1.AddToScheme(scheme.Scheme)
	_ = clusterv1.AddToScheme(scheme.Scheme)
}

func newAgentClusterRequest(agentCluster *capiproviderv1alpha1.AgentCluster) ctrl.Request {
	namespacedName := types.NamespacedName{
		Namespace: agentCluster.ObjectMeta.Namespace,
		Name:      agentCluster.ObjectMeta.Name,
	}
	return ctrl.Request{NamespacedName: namespacedName}
}

func newAgentCluster(name, namespace string, spec capiproviderv1alpha1.AgentClusterSpec) *capiproviderv1alpha1.AgentCluster {
	return &capiproviderv1alpha1.AgentCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: spec,
	}
}

// newCluster return a CAPI cluster object.
func newCluster(namespacedName *types.NamespacedName) *clusterv1.Cluster {
	spec := clusterv1.ClusterSpec{
		ControlPlaneRef: &corev1.ObjectReference{
			Kind:       "HostedControlPlane",
			Namespace:  namespacedName.Namespace,
			Name:       namespacedName.Name,
			APIVersion: schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta1"}.String(),
		},
		InfrastructureRef: &corev1.ObjectReference{
			Kind:       "AgentCluster",
			Namespace:  namespacedName.Namespace,
			Name:       namespacedName.Name,
			APIVersion: schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta1"}.String(),
		},
	}

	return &clusterv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Cluster",
			APIVersion: clusterv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespacedName.Namespace,
			Name:      namespacedName.Name,
		},
		Spec: spec,
	}
}

func createControlPlane(namespacedName *types.NamespacedName, baseDomain, pullSecretName, kubeconfig, kubeadminPassword string) *unstructured.Unstructured {

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "HostedControlPlane",
			"apiVersion": schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta1"}.String(),
			"metadata": map[string]interface{}{
				"name":      namespacedName.Name,
				"namespace": namespacedName.Namespace,
			},
			"spec": map[string]interface{}{
				"dns": map[string]interface{}{
					"baseDomain": baseDomain,
				},
				"pullSecret": map[string]interface{}{
					"name": pullSecretName,
				},
			},
			"status": map[string]interface{}{
				"externalManagedControlPlane": true,
				"kubeadminPassword": map[string]interface{}{
					"name": kubeadminPassword,
				},
			},
		}}

	if kubeconfig != "" {
		kubeconfigMap := map[string]interface{}{
			"name": kubeconfig,
			"key":  "kubeconfig",
		}
		err := unstructured.SetNestedMap(obj.UnstructuredContent(), kubeconfigMap, "status", "kubeConfig")
		Expect(err).To(BeNil())
	}

	return obj
}

func createDefaultResources(ctx context.Context, c client.Client, clusterName, testNamespace, baseDomain, pullSecretName, kubeconfig, kubeadminPassword string) *capiproviderv1alpha1.AgentCluster {
	namespaced := &types.NamespacedName{Name: clusterName, Namespace: testNamespace}
	cluster := newCluster(namespaced)
	agentCluster := newAgentCluster(clusterName, testNamespace, capiproviderv1alpha1.AgentClusterSpec{
		IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
	})

	agentCluster.OwnerReferences = []metav1.OwnerReference{{Name: cluster.Name, Kind: cluster.Kind, APIVersion: cluster.APIVersion}}
	controlPlane := createControlPlane(namespaced, baseDomain, pullSecretName, kubeconfig, kubeadminPassword)

	Expect(c.Create(ctx, cluster)).To(BeNil())
	Expect(c.Create(ctx, agentCluster)).To(BeNil())
	Expect(c.Create(ctx, controlPlane)).To(BeNil())
	return agentCluster
}

var _ = Describe("agentcluster reconcile", func() {
	var (
		c                 client.Client
		acr               *AgentClusterReconciler
		ctx               = context.Background()
		mockCtrl          *gomock.Controller
		testNamespace     = "test-namespace"
		clusterName       = "test-cluster-name"
		baseDomain        = "test.com"
		pullSecret        = "pull-secret"
		kubeconfig        = "hostedKubeconfig"
		kubeadminPassword = "kubeadmin"
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())

		acr = &AgentClusterReconciler{
			Client: c,
			Scheme: scheme.Scheme,
			Log:    logrus.New(),
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("none existing agentCluster", func() {
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{})
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		noneExistingAgentCluster := newAgentCluster("agentCluster-2", testNamespace, capiproviderv1alpha1.AgentClusterSpec{})

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(noneExistingAgentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})
	It("agentCluster ready status", func() {
		agentCluster := createDefaultResources(ctx, c, clusterName, testNamespace, baseDomain, pullSecret, kubeconfig, kubeadminPassword)
		agentCluster.Status.Ready = true
		Expect(c.Update(ctx, agentCluster)).To(BeNil())

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})
	It("no kubeconfig", func() {
		agentCluster := createDefaultResources(ctx, c, clusterName, testNamespace, baseDomain, pullSecret, "", kubeadminPassword)
		agentCluster.Status.Ready = true
		Expect(c.Update(ctx, agentCluster)).To(BeNil())

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: agentClusterDependenciesWaitTime}))
	})
	It("create clusterDeployment for agentCluster", func() {
		agentCluster := createDefaultResources(ctx, c, clusterName, testNamespace, baseDomain, pullSecret, kubeconfig, kubeadminPassword)
		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      clusterName,
		}
		Expect(c.Get(ctx, key, agentCluster)).To(BeNil())
		Expect(agentCluster.Status.ClusterDeploymentRef.Name).ToNot(Equal(""))

		clusterDeployment := &hivev1.ClusterDeployment{}
		err = c.Get(ctx, key, clusterDeployment)
		Expect(err).To(BeNil())
		Expect(clusterDeployment.Spec.BaseDomain).To(Equal(baseDomain))
		Expect(clusterDeployment.Spec.PullSecretRef.Name).To(Equal(pullSecret))
		Expect(clusterDeployment.Spec.ClusterName).To(Equal(clusterName))
		Expect(clusterDeployment.Spec.ClusterMetadata.AdminPasswordSecretRef.Name).To(Equal(kubeadminPassword))
		Expect(clusterDeployment.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name).To(Equal(kubeconfig))
		Expect(clusterDeployment.Spec.ClusterMetadata.ClusterID).To(Equal(string(agentCluster.OwnerReferences[0].UID)))
		Expect(clusterDeployment.Spec.ClusterMetadata.InfraID).To(Equal(string(agentCluster.OwnerReferences[0].UID)))
		Expect(clusterDeployment.OwnerReferences[0].UID).To(Equal(agentCluster.UID))
		Expect(clusterDeployment.OwnerReferences[0].Name).To(Equal(agentCluster.Name))
		Expect(clusterDeployment.OwnerReferences[0].Kind).To(Equal(agentCluster.Kind))
		Expect(clusterDeployment.OwnerReferences[0].APIVersion).To(Equal(agentCluster.APIVersion))

	})
	It("failed to find cluster", func() {
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})
		Expect(c.Create(ctx, agentCluster)).To(BeNil())
		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: agentClusterDependenciesWaitTime}))
	})
	It("no control plane reference in cluster", func() {
		clusterName := "test-cluster-name"

		cluster := newCluster(&types.NamespacedName{Name: clusterName, Namespace: testNamespace})
		cluster.Spec.ControlPlaneRef = nil

		agentCluster := newAgentCluster(clusterName, testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})
		agentCluster.OwnerReferences = []metav1.OwnerReference{{Name: cluster.Name, Kind: cluster.Kind, APIVersion: cluster.APIVersion}}

		Expect(c.Create(ctx, agentCluster)).To(BeNil())
		Expect(c.Create(ctx, cluster)).To(BeNil())
		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: agentClusterDependenciesWaitTime}))

	})

	It("failed to find clusterDeployment", func() {
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})
		agentCluster.Status.ClusterDeploymentRef.Name = "missing-cluster-deployment-name"
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).ToNot(BeNil())
		Expect(err.Error()).To(MatchRegexp("not found"))
		Expect(result).To(Equal(ctrl.Result{}))
	})
	It("create AgentClusterInstall for agentCluster", func() {
		agentCluster := createDefaultResources(ctx, c, "agentCluster-1", testNamespace, baseDomain, pullSecret, kubeconfig, kubeadminPassword)

		_, _ = acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))

		_, _ = acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      agentCluster.Name,
		}
		Expect(c.Get(ctx, key, agentCluster)).To(BeNil())
		Expect(agentCluster.Status.ClusterDeploymentRef.Name).To(Equal("agentCluster-1"))

		agentClusterInstall := &hiveext.AgentClusterInstall{}
		Expect(c.Get(ctx, key, agentClusterInstall)).To(BeNil())
		Expect(*agentClusterInstall.Spec.Networking.UserManagedNetworking).To(BeTrue())
	})
	It("agentCluster missing controlPlaneEndpoint", func() {
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})

		agentCluster.Status.ClusterDeploymentRef.Name = agentCluster.Name
		agentCluster.Status.ClusterDeploymentRef.Namespace = agentCluster.Namespace
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		createAgentClusterInstall(c, ctx, agentCluster.Namespace, agentCluster.Name)
		createClusterDeployment(c, ctx, agentCluster, "agentCluster-1", baseDomain, pullSecret)

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: agentClusterDependenciesWaitTime}))
	})
})

func createClusterDeployment(c client.Client, ctx context.Context, agentCluster *capiproviderv1alpha1.AgentCluster, clusterName, baseDomain, pullSecretName string) {
	clusterDeployment := &hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentCluster.Name,
			Namespace: agentCluster.Namespace,
		},
		Spec: hivev1.ClusterDeploymentSpec{
			Installed:     true,
			BaseDomain:    baseDomain,
			ClusterName:   clusterName,
			PullSecretRef: &corev1.LocalObjectReference{Name: pullSecretName},
			ClusterInstallRef: &hivev1.ClusterInstallLocalReference{
				Kind:    "AgentClusterInstall",
				Group:   hiveext.Group,
				Version: hiveext.Version,
				Name:    agentCluster.Name,
			},
			Platform: hivev1.Platform{
				AgentBareMetal: &agent.BareMetalPlatform{},
			},
		},
	}
	Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
}

func createAgentClusterInstall(c client.Client, ctx context.Context, namespace string, name string) {
	agentClusterInstall := &hiveext.AgentClusterInstall{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: hiveext.AgentClusterInstallSpec{
			ClusterDeploymentRef: corev1.LocalObjectReference{Name: name},
		},
	}
	Expect(c.Create(ctx, agentClusterInstall)).To(BeNil())
}
