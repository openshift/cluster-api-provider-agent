package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ignitionapi "github.com/coreos/ignition/v2/config/v3_1/types"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	capiproviderv1alpha1 "github.com/openshift/cluster-api-provider-agent/api/v1alpha1"
	v1 "github.com/openshift/custom-resource-status/conditions/v1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	k8sutilspointer "k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func init() {
	_ = aiv1beta1.AddToScheme(scheme.Scheme)
	_ = hivev1.AddToScheme(scheme.Scheme)
	_ = hiveext.AddToScheme(scheme.Scheme)
	_ = capiproviderv1alpha1.AddToScheme(scheme.Scheme)
	_ = clusterv1.AddToScheme(scheme.Scheme)
}

func newAgentMachineRequest(agentMachine *capiproviderv1alpha1.AgentMachine) ctrl.Request {
	namespacedName := types.NamespacedName{
		Namespace: agentMachine.ObjectMeta.Namespace,
		Name:      agentMachine.ObjectMeta.Name,
	}
	return ctrl.Request{NamespacedName: namespacedName}
}

func newAgentMachine(name, namespace string, spec capiproviderv1alpha1.AgentMachineSpec, ctx context.Context, c client.Client, missingLink bool) *capiproviderv1alpha1.AgentMachine {
	clusterDeployment := hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-deployment-%s", name),
			Namespace: namespace,
		},
		Spec: hivev1.ClusterDeploymentSpec{},
	}
	Expect(c.Create(ctx, &clusterDeployment)).To(BeNil())

	agentCluster := capiproviderv1alpha1.AgentCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("agent-cluster-%s", name),
			Namespace: namespace,
		},
		Spec:   capiproviderv1alpha1.AgentClusterSpec{ClusterName: "foo", BaseDomain: "example.com"},
		Status: capiproviderv1alpha1.AgentClusterStatus{ClusterDeploymentRef: capiproviderv1alpha1.ClusterDeploymentReference{Namespace: namespace, Name: clusterDeployment.Name}},
	}
	Expect(c.Create(ctx, &agentCluster)).To(BeNil())

	cluster := clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-%s", name),
			Namespace: namespace,
		},
		Spec:   clusterv1.ClusterSpec{InfrastructureRef: &corev1.ObjectReference{Namespace: agentCluster.Namespace, Name: agentCluster.Name}},
		Status: clusterv1.ClusterStatus{},
	}
	Expect(c.Create(ctx, &cluster)).To(BeNil())

	machine := clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("machine-%s", name),
			Namespace: namespace,
			Labels:    make(map[string]string),
		},
		Spec:   clusterv1.MachineSpec{},
		Status: clusterv1.MachineStatus{},
	}
	machine.ObjectMeta.Labels[clusterv1.ClusterLabelName] = cluster.Name
	Expect(c.Create(ctx, &machine)).To(BeNil())

	machineOwnerRef := metav1.OwnerReference{APIVersion: "cluster.x-k8s.io/v1beta1", Kind: "Machine", Name: machine.Name}
	agentMachine := capiproviderv1alpha1.AgentMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec:   spec,
		Status: capiproviderv1alpha1.AgentMachineStatus{},
	}
	if !missingLink {
		agentMachine.ObjectMeta.OwnerReferences = append(agentMachine.ObjectMeta.OwnerReferences, machineOwnerRef)
	}
	return &agentMachine
}

func newAgent(name, namespace string, spec aiv1beta1.AgentSpec) *aiv1beta1.Agent {
	return &aiv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    make(map[string]string),
		},
		Spec:   spec,
		Status: aiv1beta1.AgentStatus{},
	}
}

func boolToConditionStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func newAgentWithProperties(name, namespace string, approved, bound, validated bool, cores, ramGiB int, labels map[string]string) *aiv1beta1.Agent {
	agent := newAgent(name, namespace, aiv1beta1.AgentSpec{Approved: approved})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: boolToConditionStatus(bound)})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: boolToConditionStatus(validated)})
	agent.Status.Inventory.Cpu.Count = int64(cores)
	agent.Status.Inventory.Memory.PhysicalBytes = int64(ramGiB) * 1024 * 1024 * 1024
	agent.ObjectMeta.SetLabels(labels)
	return agent
}

var _ = Describe("agentmachine reconcile", func() {
	var (
		c             client.Client
		amr           *AgentMachineReconciler
		ctx           = context.Background()
		mockCtrl      *gomock.Controller
		testNamespace = "test-namespace"
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())

		amr = &AgentMachineReconciler{
			Client: c,
			Scheme: scheme.Scheme,
			Log:    logrus.New(),
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	It("agentMachine update status ready", func() {
		agent := newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: true,
			IgnitionEndpointTokenReference: &aiv1beta1.IgnitionEndpointTokenReference{Namespace: testNamespace, Name: "token"},
			ClusterDeploymentName:          &aiv1beta1.ClusterReference{Namespace: testNamespace, Name: "dep"}})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "True"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.InstalledCondition, Status: "True"})
		Expect(c.Create(ctx, agent)).To(BeNil())

		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: testNamespace, Name: "agent-1"}
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.Ready).To(BeEquivalentTo(true))
	})

	It("agentMachine no agents", func() {
		// Agent0: not approved
		agent0 := newAgent("agent-0", testNamespace, aiv1beta1.AgentSpec{Approved: false})
		agent0.Status.Conditions = append(agent0.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "False"})
		agent0.Status.Conditions = append(agent0.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		Expect(c.Create(ctx, agent0)).To(BeNil())

		agentMachine := newAgentMachine("agentMachine-0", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueWaitingForAvailableAgent}))
	})

	It("agentMachine set clusterref later", func() {
		agent := newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: true})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "False"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		Expect(c.Create(ctx, agent)).To(BeNil())

		// Create AgentMachine with missing label in Machine
		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, true)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		// Find Agent
		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		// Try and fail to get Machine
		result, err = amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-1"))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-1"}, agent)).To(BeNil())
		Expect(agent.Spec.ClusterDeploymentName).To(BeNil())

		// Create missing link and reconcile again
		machineOwnerRef := metav1.OwnerReference{APIVersion: "cluster.x-k8s.io/v1beta1", Kind: "Machine", Name: "machine-agentMachine-1"}
		agentMachine.ObjectMeta.OwnerReferences = append(agentMachine.ObjectMeta.OwnerReferences, machineOwnerRef)
		err = c.Update(ctx, agentMachine)
		Expect(err).To(BeNil())

		result, err = amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-1"))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-1"}, agent)).To(BeNil())
		Expect(agent.Spec.ClusterDeploymentName.Name).To(BeEquivalentTo("cluster-deployment-agentMachine-1"))
	})

	It("agentMachine set addresses", func() {
		agent := newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: true})
		agent.Status.Inventory.Hostname = "agent1"
		agent.Status.Inventory.Interfaces = []aiv1beta1.HostInterface{
			{
				HasCarrier:    true,
				IPV4Addresses: []string{"1.2.3.4/24", "2.3.4.5/24"},
			},
			{
				HasCarrier:    false,
				IPV6Addresses: []string{"9.9.9.9/24"},
			},
			{
				HasCarrier:    true,
				IPV4Addresses: []string{"3.4.5.6/24"},
			},
		}
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "False"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		Expect(c.Create(ctx, agent)).To(BeNil())

		// Create AgentMachine with missing label in Machine
		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		_, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-1"))
		Expect(len(agentMachine.Status.Addresses)).To(BeEquivalentTo(4))
		expectedAddresses := []string{"1.2.3.4", "2.3.4.5", "3.4.5.6", "agent1"}
		expectedTypes := []string{string(clusterv1.MachineExternalIP), string(clusterv1.MachineInternalDNS)}
		for i := 0; i < len(agentMachine.Status.Addresses); i++ {
			Expect(funk.ContainsString(expectedAddresses, agentMachine.Status.Addresses[i].Address)).To(BeEquivalentTo(true))
			Expect(funk.ContainsString(expectedTypes, string(agentMachine.Status.Addresses[i].Type))).To(BeEquivalentTo(true))
		}
	})

	It("agentMachine set ignition endpoint", func() {
		agent := newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: true})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "False"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		Expect(c.Create(ctx, agent)).To(BeNil())

		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		ignConfig := ignitionapi.Config{
			Ignition: ignitionapi.Ignition{
				Version: "3.1.0",
				Security: ignitionapi.Security{
					TLS: ignitionapi.TLS{
						CertificateAuthorities: []ignitionapi.Resource{
							{
								Source: k8sutilspointer.StringPtr("data:text/plain;base64,encodedCACert"),
							},
						},
					},
				},
				Config: ignitionapi.IgnitionConfig{
					Merge: []ignitionapi.Resource{
						{
							Source: k8sutilspointer.StringPtr("https://endpoint/ignition"),
							HTTPHeaders: []ignitionapi.HTTPHeader{
								{
									Name:  "Authorization",
									Value: k8sutilspointer.StringPtr("Bearer encodedToken"),
								},
							},
						},
					},
				},
			},
		}
		userDataValue, err := json.Marshal(ignConfig)
		Expect(err).To(BeNil())
		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "userdata-secret",
				Namespace: testNamespace,
			},
			Data: map[string][]byte{
				"value": userDataValue,
			},
		}
		Expect(c.Create(ctx, &secret)).To(BeNil())

		machine := &clusterv1.Machine{}
		machineRef := types.NamespacedName{Namespace: agentMachine.Namespace, Name: agentMachine.ObjectMeta.OwnerReferences[0].Name}
		Expect(c.Get(ctx, machineRef, machine)).To(BeNil())
		machine.Spec.Bootstrap.DataSecretName = &secret.ObjectMeta.Name
		err = c.Update(ctx, machine)
		Expect(err).To(BeNil())

		// Set Agent
		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		// Set ignition
		result, err = amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-1"}, agent)).To(BeNil())
		Expect(agent.Spec.IgnitionEndpointTokenReference.Name).To(BeEquivalentTo("agent-userdata-secret"))
		Expect(agent.Spec.IgnitionEndpointTokenReference.Namespace).To(BeEquivalentTo(testNamespace))

		agentSecret := &corev1.Secret{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-userdata-secret"}, agentSecret)).To(BeNil())
		Expect(agentSecret.Data["ignition-token"]).To(BeEquivalentTo([]byte("encodedToken")))
	})

	It("agentMachine find agent end-to-end", func() {
		var labels map[string]string
		Expect(c.Create(ctx, newAgentWithProperties("agent-0", testNamespace, false, false, true, 32, 100, labels))).To(BeNil()) // Agent0: not approved
		Expect(c.Create(ctx, newAgentWithProperties("agent-1", testNamespace, true, true, true, 32, 100, labels))).To(BeNil())   // Agent1: already bound
		Expect(c.Create(ctx, newAgentWithProperties("agent-2", testNamespace, true, false, true, 32, 100, labels))).To(BeNil())  // Agent2: in use by another AgentMachine
		Expect(c.Create(ctx, newAgentWithProperties("agent-3", testNamespace, true, true, false, 32, 100, labels))).To(BeNil())  // Agent3: validations are failing
		Expect(c.Create(ctx, newAgentWithProperties("agent-4", testNamespace, true, false, true, 32, 100, labels))).To(BeNil())  // Agent4: should use this one
		Expect(c.Create(ctx, newAgentWithProperties("agent-5", testNamespace, true, false, true, 32, 100, labels))).To(BeNil())  // Agent5: in use by another AgentMachine

		// Create AgentMachines that are using agents 2 and 5
		agentMachine2 := newAgentMachine("agentMachine-2", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine2.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: testNamespace, Name: "agent-2"}
		Expect(c.Create(ctx, agentMachine2)).To(BeNil())
		agentMachine5 := newAgentMachine("agentMachine-5", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine5.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: testNamespace, Name: "agent-5"}
		Expect(c.Create(ctx, agentMachine5)).To(BeNil())

		agentMachine := newAgentMachine("agentMachine-4", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		agentMachineRequest := newAgentMachineRequest(agentMachine)

		// find agent
		result, err := amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-4"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-4"))

		// set clusterdeployment
		result, err = amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		agent := &aiv1beta1.Agent{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-4"}, agent)).To(BeNil())
		Expect(agent.Spec.ClusterDeploymentName.Name).To(BeEquivalentTo("cluster-deployment-agentMachine-4"))
	})

	It("agentMachine find agent according to properties", func() {
		spec := capiproviderv1alpha1.AgentMachineSpec{
			MinCPUs:      16,
			MinMemoryMiB: 64 * 1024,
			AgentLabelSelector: &metav1.LabelSelector{
				MatchLabels:      map[string]string{"hasGpu": "true"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "location", Operator: "In", Values: []string{"datacenter2", "datacenter3"}}},
			},
		}
		agentMachine := newAgentMachine("agentMachine", testNamespace, spec, ctx, c, false)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		agentMachineRequest := newAgentMachineRequest(agentMachine)

		goodLabels := map[string]string{"location": "datacenter2", "hasGpu": "true"}
		Expect(c.Create(ctx, newAgentWithProperties("agent-0", testNamespace, true, false, true, 8, 128, goodLabels))).To(BeNil()) // Agent0: insufficient cores
		Expect(c.Create(ctx, newAgentWithProperties("agent-1", testNamespace, true, false, true, 32, 32, goodLabels))).To(BeNil()) // Agent1: insufficient ram
		Expect(c.Create(ctx, newAgentWithProperties("agent-2", testNamespace, true, false, true, 8, 32, goodLabels))).To(BeNil())  // Agent2: insufficient cores and ram
		badLabels1 := map[string]string{"location": "datacenter1", "hasGpu": "false"}
		Expect(c.Create(ctx, newAgentWithProperties("agent-3", testNamespace, true, false, true, 32, 128, badLabels1))).To(BeNil()) // Agent3: bad labels
		badLabels2 := map[string]string{"location": "datacenter2", "hasGpu": "false"}
		Expect(c.Create(ctx, newAgentWithProperties("agent-4", testNamespace, true, false, true, 32, 128, badLabels2))).To(BeNil()) // Agent4: bad labels
		badLabels3 := map[string]string{"location": "datacenter1", "hasGpu": "true"}
		Expect(c.Create(ctx, newAgentWithProperties("agent-5", testNamespace, true, false, true, 32, 128, badLabels3))).To(BeNil()) // Agent5: bad labels
		badLabels4 := map[string]string{"location": "datacenter2"}
		Expect(c.Create(ctx, newAgentWithProperties("agent-6", testNamespace, true, false, true, 32, 128, badLabels4))).To(BeNil()) // Agent6: bad labels
		badLabels5 := map[string]string{"hasGpu": "true"}
		Expect(c.Create(ctx, newAgentWithProperties("agent-7", testNamespace, true, false, true, 32, 128, badLabels5))).To(BeNil()) // Agent7: bad labels
		Expect(c.Create(ctx, newAgentWithProperties("agent-8", testNamespace, true, false, true, 32, 128, goodLabels))).To(BeNil()) // Agent8: the chosen one

		// find agent
		result, err := amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-8"))

		// set clusterdeployment
		result, err = amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{Requeue: true}))

		agent := &aiv1beta1.Agent{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-8"}, agent)).To(BeNil())
		Expect(agent.Spec.ClusterDeploymentName.Name).To(BeEquivalentTo("cluster-deployment-agentMachine"))
	})

	It("non-existing agentMachine", func() {
		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		nonExistingAgentMachine := newAgentMachine("agentMachine-2", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(nonExistingAgentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("agentMachine ready status", func() {
		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine.Status.Ready = true
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("agentMachine deprovision with no agentRef", func() {
		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine.Status.Ready = true
		agentMachine.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		getErr := c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)
		Expect(getErr.(*errors.StatusError).Status().Code).To(BeEquivalentTo(404))
	})

	It("agentMachine deprovision with agentRef", func() {
		agent := newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: false})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "True"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		Expect(c.Create(ctx, agent)).To(BeNil())

		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine.Status.Ready = true
		agentMachine.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: agent.Namespace, Name: agent.Name}
		agentMachine.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		getErr := c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)
		Expect(getErr.(*errors.StatusError).Status().Code).To(BeEquivalentTo(404))
	})
	It("agentMachine deprovision with agentRef and no agent", func() {
		agentMachine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1alpha1.AgentMachineSpec{}, ctx, c, false)
		agentMachine.Status.Ready = true
		agentMachine.Status.AgentRef = &capiproviderv1alpha1.AgentReference{Namespace: testNamespace, Name: "missingAgent"}
		agentMachine.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		getErr := c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)
		Expect(getErr.(*errors.StatusError).Status().Code).To(BeEquivalentTo(404))
	})
})
