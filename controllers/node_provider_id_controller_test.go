package controllers

import (
	"context"
	"errors"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func init() {
	_ = aiv1beta1.AddToScheme(scheme.Scheme)
	_ = hivev1.AddToScheme(scheme.Scheme)
	_ = hiveext.AddToScheme(scheme.Scheme)
	_ = capiproviderv1alpha1.AddToScheme(scheme.Scheme)
	_ = clusterv1.AddToScheme(scheme.Scheme)
}

func createAgentMachine(name, namespace string, ready bool, machineAddresses []clusterv1.MachineAddress) *capiproviderv1alpha1.AgentMachine {
	agentMachine := capiproviderv1alpha1.AgentMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec:   capiproviderv1alpha1.AgentMachineSpec{},
		Status: capiproviderv1alpha1.AgentMachineStatus{Ready: ready, Addresses: machineAddresses},
	}
	return &agentMachine
}

func getAgentAddresses(internalDNS string) []clusterv1.MachineAddress {
	addresses := []clusterv1.MachineAddress{
		{Type: clusterv1.MachineExternalIP, Address: "192.186.126.10"},
		{Type: clusterv1.MachineExternalIP, Address: "10.0.36.14"},
	}
	if internalDNS != "" {
		addresses = append(addresses, clusterv1.MachineAddress{Type: clusterv1.MachineInternalDNS, Address: internalDNS})
	}
	return addresses
}

func getNode(nodeName string) corev1.Node {
	return corev1.Node{}
}

var _ = Describe("node providerID reconcile", func() {
	var (
		c                       client.Client
		rc                      client.Client
		npid                    *NodeProviderIDReconciler
		mockCtrl                *gomock.Controller
		mockRemoteClientHandler *MockRemoteClientHandler
		ctx                     = context.Background()
		testNamespace           = "test-namespace"
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())
		mockRemoteClientHandler = NewMockRemoteClientHandler(mockCtrl)
		rc = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		npid = &NodeProviderIDReconciler{
			Client:              c,
			Scheme:              scheme.Scheme,
			Log:                 logrus.New(),
			RemoteClientHandler: mockRemoteClientHandler,
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("agentMachine isn't ready - requeue", func() {
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, false, getAgentAddresses("worker-1"))
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeue}))
	})
	It("happy flow set providerId success", func() {
		nodeName := "worker-1"
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, true, getAgentAddresses(nodeName))
		providerID := "agent://a5828c6e-b731-4c7f-94d6-399c2d6c7e59"
		agentMachine.Spec.ProviderID = &providerID
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		mockRemoteClientHandler.EXPECT().GetRemoteClient(gomock.Any(), agentMachine.Namespace).Return(rc, nil).Times(1)
		// create the node with a name that match the InternalDNS of the agentMachine
		node := corev1.Node{}
		node.Name = nodeName
		rc.Create(ctx, &node)
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		// validate the node providerID was updated
		err = rc.Get(ctx, types.NamespacedName{Namespace: "", Name: nodeName}, &node)
		Expect(err).To(BeNil())
		Expect(node.Spec.ProviderID).To(Equal(providerID))
	})
	It("happy flow set providerId - no op", func() {
		nodeName := "worker-1"
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, true, getAgentAddresses(nodeName))
		providerID := "agent://a5828c6e-b731-4c7f-94d6-399c2d6c7e59"
		agentMachine.Spec.ProviderID = &providerID
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		mockRemoteClientHandler.EXPECT().GetRemoteClient(gomock.Any(), agentMachine.Namespace).Return(rc, nil).Times(1)
		// create the node with a name that match the InternalDNS of the agentMachine
		node := corev1.Node{}
		node.Name = nodeName
		node.Spec.ProviderID = providerID
		rc.Create(ctx, &node)
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
		// validate the node providerID was updated
		err = rc.Get(ctx, types.NamespacedName{Namespace: "", Name: nodeName}, &node)
		Expect(err).To(BeNil())
		Expect(node.Spec.ProviderID).To(Equal(providerID))
	})
	It("failed to set providerId - can't find node with matching node name", func() {
		nodeName := "worker-1"
		internalDNS := "not-worker-1"
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, true, getAgentAddresses(internalDNS))
		providerID := "agent://a5828c6e-b731-4c7f-94d6-399c2d6c7e59"
		agentMachine.Spec.ProviderID = &providerID
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		mockRemoteClientHandler.EXPECT().GetRemoteClient(gomock.Any(), agentMachine.Namespace).Return(rc, nil).Times(1)
		// create the node with a name that match the InternalDNS of the agentMachine
		node := corev1.Node{}
		node.Name = nodeName
		rc.Create(ctx, &node)
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeue}))
		// validate the node providerID wasn't updated
		err = rc.Get(ctx, types.NamespacedName{Namespace: "", Name: nodeName}, &node)
		Expect(err).To(BeNil())
		Expect(node.Spec.ProviderID).To(Equal(""))
	})
	It("failed to set providerId - agentMachine missing internalDNS", func() {
		internalDNS := ""
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, true, getAgentAddresses(internalDNS))
		providerID := "agent://a5828c6e-b731-4c7f-94d6-399c2d6c7e59"
		agentMachine.Spec.ProviderID = &providerID
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeue}))
	})
	It("failed to set providerId - can't find node with matching node name", func() {
		internalDNS := "not-worker-1"
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, true, getAgentAddresses(internalDNS))
		providerID := "agent://a5828c6e-b731-4c7f-94d6-399c2d6c7e59"
		agentMachine.Spec.ProviderID = &providerID
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		mockRemoteClientHandler.EXPECT().GetRemoteClient(gomock.Any(), agentMachine.Namespace).Return(nil, errors.New("Failed to create remoteClient")).Times(1)
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeue}))
	})
	It("failed to set providerId - different providerID already set", func() {
		nodeName := "worker-1"
		agentMachine := createAgentMachine("agentMachine-1", testNamespace, true, getAgentAddresses(nodeName))
		providerID := "agent://a5828c6e-b731-4c7f-94d6-399c2d6c7e59"
		agentMachine.Spec.ProviderID = &providerID
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		mockRemoteClientHandler.EXPECT().GetRemoteClient(gomock.Any(), agentMachine.Namespace).Return(rc, nil).Times(1)
		// create the node with a name that match the InternalDNS of the agentMachine
		node := corev1.Node{}
		node.Name = nodeName
		differentProviderID := "different://providerID"
		node.Spec.ProviderID = differentProviderID
		rc.Create(ctx, &node)
		result, err := npid.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeue}))
		// validate the node providerID wasn't updated
		err = rc.Get(ctx, types.NamespacedName{Namespace: "", Name: nodeName}, &node)
		Expect(err).To(BeNil())
		Expect(node.Spec.ProviderID).To(Equal(differentProviderID))
	})
})
