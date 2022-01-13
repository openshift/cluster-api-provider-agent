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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	_ = hivev1.AddToScheme(scheme.Scheme)
	_ = hiveext.AddToScheme(scheme.Scheme)
	_ = capiproviderv1alpha1.AddToScheme(scheme.Scheme)
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

var _ = Describe("agentcluster reconcile", func() {
	var (
		c             client.Client
		acr           *AgentClusterReconciler
		ctx           = context.Background()
		mockCtrl      *gomock.Controller
		testNamespace = "test-namespace"
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
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{})
		agentCluster.Status.Ready = true
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})
	It("create clusterDeployment for agentCluster", func() {
		domain := "test-domain.com"
		clusterName := "test-cluster-name"
		pullSecretName := "test-pull-secret-name"
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			BaseDomain:  domain,
			ClusterName: clusterName,
			PullSecretRef: &corev1.LocalObjectReference{
				Name: pullSecretName,
			},
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "agentCluster-1",
		}
		Expect(c.Get(ctx, key, agentCluster)).To(BeNil())
		Expect(agentCluster.Status.ClusterDeploymentRef.Name).ToNot(Equal(""))
	})
	It("failed to find clusterDeployment", func() {
		domain := "test-domain.com"
		clusterName := "test-cluster-name"
		pullSecretName := "test-pull-secret-name"
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			BaseDomain:  domain,
			ClusterName: clusterName,
			PullSecretRef: &corev1.LocalObjectReference{
				Name: pullSecretName,
			},
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})
		agentCluster.Status.ClusterDeploymentRef.Name = "missing-cluster-deployment-name"
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).ToNot(BeNil())
		Expect(err.Error()).To(MatchRegexp("not found"))
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))
	})
	It("create AgentClusterInstall for agentCluster", func() {
		domain := "test-domain.com"
		clusterName := "test-cluster-name"
		pullSecretName := "test-pull-secret-name"
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			BaseDomain:  domain,
			ClusterName: clusterName,
			PullSecretRef: &corev1.LocalObjectReference{
				Name: pullSecretName,
			},
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})
		agentCluster.Status.ClusterDeploymentRef.Name = agentCluster.Name
		agentCluster.Status.ClusterDeploymentRef.Namespace = agentCluster.Namespace
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		createClusterDeployment(c, ctx, agentCluster)

		_, _ = acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))

		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      "agentCluster-1",
		}
		Expect(c.Get(ctx, key, agentCluster)).To(BeNil())
		Expect(agentCluster.Status.ClusterDeploymentRef.Name).To(Equal("agentCluster-1"))

		agentClusterInstall := &hiveext.AgentClusterInstall{}
		Expect(c.Get(ctx, key, agentClusterInstall)).To(BeNil())
	})
	It("agentCluster missing controlPlaneEndpoint", func() {
		domain := "test-domain.com"
		clusterName := "test-cluster-name"
		pullSecretName := "test-pull-secret-name"
		agentCluster := newAgentCluster("agentCluster-1", testNamespace, capiproviderv1alpha1.AgentClusterSpec{
			BaseDomain:  domain,
			ClusterName: clusterName,
			PullSecretRef: &corev1.LocalObjectReference{
				Name: pullSecretName,
			},
			IgnitionEndpoint: &capiproviderv1alpha1.IgnitionEndpoint{Url: "https://1.2.3.4:555/ignition"},
		})

		agentCluster.Status.ClusterDeploymentRef.Name = agentCluster.Name
		agentCluster.Status.ClusterDeploymentRef.Namespace = agentCluster.Namespace
		Expect(c.Create(ctx, agentCluster)).To(BeNil())

		createAgentClusterInstall(c, ctx, agentCluster.Namespace, agentCluster.Name)
		createClusterDeployment(c, ctx, agentCluster)

		result, err := acr.Reconcile(ctx, newAgentClusterRequest(agentCluster))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
	})
})

func createClusterDeployment(c client.Client, ctx context.Context, agentCluster *capiproviderv1alpha1.AgentCluster) {
	clusterDeployment := &hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentCluster.Name,
			Namespace: agentCluster.Namespace,
		},
		Spec: hivev1.ClusterDeploymentSpec{
			Installed:     true,
			BaseDomain:    agentCluster.Spec.BaseDomain,
			ClusterName:   agentCluster.Spec.ClusterName,
			PullSecretRef: agentCluster.Spec.PullSecretRef,
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
