package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	ignitionapi "github.com/coreos/ignition/v2/config/v3_1/types"
	"github.com/go-openapi/swag"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	aiv1beta1 "github.com/openshift/assisted-service/api/v1beta1"
	aimodels "github.com/openshift/assisted-service/models"
	capiproviderv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	v1 "github.com/openshift/custom-resource-status/conditions/v1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func init() {
	_ = aiv1beta1.AddToScheme(scheme.Scheme)
	_ = hivev1.AddToScheme(scheme.Scheme)
	_ = hiveext.AddToScheme(scheme.Scheme)
	_ = capiproviderv1.AddToScheme(scheme.Scheme)
	_ = clusterv1.AddToScheme(scheme.Scheme)
}

func newAgentMachineRequest(agentMachine *capiproviderv1.AgentMachine) ctrl.Request {
	namespacedName := types.NamespacedName{
		Namespace: agentMachine.ObjectMeta.Namespace,
		Name:      agentMachine.ObjectMeta.Name,
	}
	return ctrl.Request{NamespacedName: namespacedName}
}

func newAgentMachine(name, namespace string, spec capiproviderv1.AgentMachineSpec, ctx context.Context, c client.Client) (*capiproviderv1.AgentMachine, *clusterv1.Machine) {
	clusterDeployment := hivev1.ClusterDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("cluster-deployment-%s", name),
			Namespace: namespace,
		},
		Spec: hivev1.ClusterDeploymentSpec{},
	}
	Expect(c.Create(ctx, &clusterDeployment)).To(BeNil())

	agentCluster := capiproviderv1.AgentCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("agent-cluster-%s", name),
			Namespace: namespace,
		},
		Status: capiproviderv1.AgentClusterStatus{ClusterDeploymentRef: capiproviderv1.ClusterDeploymentReference{Namespace: namespace, Name: clusterDeployment.Name}},
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

	ignConfig := ignitionapi.Config{
		Ignition: ignitionapi.Ignition{
			Version: "3.1.0",
			Security: ignitionapi.Security{
				TLS: ignitionapi.TLS{
					CertificateAuthorities: []ignitionapi.Resource{
						{
							Source: ptr.To("data:text/plain;base64,encodedCACert"),
						},
					},
				},
			},
			Config: ignitionapi.IgnitionConfig{
				Merge: []ignitionapi.Resource{
					{
						Source: ptr.To("https://endpoint/ignition"),
						HTTPHeaders: []ignitionapi.HTTPHeader{
							{
								Name:  "Authorization",
								Value: ptr.To("Bearer encodedToken"),
							},
						},
					},
				},
			},
		},
	}
	userDataValue, err := json.Marshal(ignConfig)
	Expect(err).To(BeNil())
	secretName := fmt.Sprintf("userdata-secret-%s", name)
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"value": userDataValue,
		},
	}
	Expect(c.Create(ctx, &secret)).To(BeNil())

	machine := clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("machine-%s", name),
			Namespace:   namespace,
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
		},
		Spec: clusterv1.MachineSpec{
			Bootstrap: clusterv1.Bootstrap{
				DataSecretName: swag.String(secretName),
			},
		},
		Status: clusterv1.MachineStatus{},
	}
	machine.ObjectMeta.Labels[clusterv1.ClusterNameLabel] = cluster.Name
	Expect(c.Create(ctx, &machine)).To(BeNil())

	machineOwnerRef := metav1.OwnerReference{APIVersion: "cluster.x-k8s.io/v1beta1", Kind: "Machine", Name: machine.Name}
	agentMachine := capiproviderv1.AgentMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec:   spec,
		Status: capiproviderv1.AgentMachineStatus{},
	}
	agentMachine.ObjectMeta.OwnerReferences = append(agentMachine.ObjectMeta.OwnerReferences, machineOwnerRef)

	return &agentMachine, &machine
}

func newAgent(name, namespace string, spec aiv1beta1.AgentSpec) *aiv1beta1.Agent {
	return &aiv1beta1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      make(map[string]string),
			Annotations: make(map[string]string),
		},
		Spec: spec,
		Status: aiv1beta1.AgentStatus{
			Inventory: aiv1beta1.HostInventory{
				Hostname: "agent-hostname",
				Interfaces: []aiv1beta1.HostInterface{
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
				},
			},
		},
	}
}

func boolToConditionStatus(b bool) corev1.ConditionStatus {
	if b {
		return corev1.ConditionTrue
	}
	return corev1.ConditionFalse
}

func newAgentWithProperties(name, namespace string, approved, bound, validated bool, labels map[string]string) *aiv1beta1.Agent {
	agent := newAgent(name, namespace, aiv1beta1.AgentSpec{Approved: approved})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: boolToConditionStatus(bound)})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: boolToConditionStatus(validated)})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.SpecSyncedCondition, Status: boolToConditionStatus(true)})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.RequirementsMetCondition, Status: boolToConditionStatus(validated)})
	agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.InstalledCondition, Status: boolToConditionStatus(false), Reason: aiv1beta1.InstallationNotStartedMsg, Message: ""})
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
		agentMachine := &capiproviderv1.AgentMachine{}
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).WithStatusSubresource(agentMachine).Build()
		mockCtrl = gomock.NewController(GinkgoT())

		amr = &AgentMachineReconciler{
			Client:      c,
			Scheme:      scheme.Scheme,
			Log:         logrus.New(),
			AgentClient: c,
			APIReader:   c,
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
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.SpecSyncedCondition, Status: "True"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.RequirementsMetCondition, Status: "True"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.InstalledCondition, Status: "True"})
		Expect(c.Create(ctx, agent)).To(BeNil())

		agentMachine, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Namespace: testNamespace, Name: "agent-1"}
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.Ready).To(BeEquivalentTo(true))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentReservedCondition).Status).To(BeEquivalentTo("True"))
		Expect(conditions.Get(agentMachine, capiproviderv1.InstalledCondition).Status).To(BeEquivalentTo("True"))
		Expect(conditions.Get(agentMachine, clusterv1.ReadyCondition).Status).To(BeEquivalentTo("True"))
	})

	It("sets the finalizer and the machine delete hook annotation", func() {
		agentMachine, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(Succeed())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(agentMachine, AgentMachineFinalizerName)).To(BeTrue())
		machine, err := clusterutil.GetOwnerMachine(ctx, c, agentMachine.ObjectMeta)
		Expect(err).To(BeNil())
		_, haveMachineHookAnnotation := machine.GetAnnotations()[machineDeleteHookName]
		Expect(haveMachineHookAnnotation).To(BeTrue())
	})

	It("agentMachine no agents", func() {
		// Agent0: not approved
		agent0 := newAgent("agent-0", testNamespace, aiv1beta1.AgentSpec{Approved: false})
		agent0.Status.Conditions = append(agent0.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "False"})
		agent0.Status.Conditions = append(agent0.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
		Expect(c.Create(ctx, agent0)).To(BeNil())

		agentMachine, _ := newAgentMachine("agentMachine-0", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-0"}, agentMachine)).To(BeNil())
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentReservedCondition).Status).To(BeEquivalentTo("False"))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentReservedCondition).Reason).To(Equal(capiproviderv1.NoSuitableAgentsReason))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentSpecSyncedCondition).Status).To(BeEquivalentTo("False"))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentValidatedCondition).Status).To(BeEquivalentTo("False"))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentRequirementsMetCondition).Status).To(BeEquivalentTo("False"))
		Expect(conditions.Get(agentMachine, capiproviderv1.InstalledCondition).Status).To(BeEquivalentTo("False"))
		Expect(conditions.Get(agentMachine, capiproviderv1.InstalledCondition).Severity).To(BeEquivalentTo(clusterv1.ConditionSeverityInfo))
		Expect(conditions.Get(agentMachine, clusterv1.ReadyCondition).Status).To(BeEquivalentTo("False"))
	})

	It("agentMachine find agent end-to-end", func() {
		spec := capiproviderv1.AgentMachineSpec{
			AgentLabelSelector: &metav1.LabelSelector{
				MatchLabels:      map[string]string{"hasGpu": "true"},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "location", Operator: "In", Values: []string{"datacenter2", "datacenter3"}}},
			},
		}
		agentMachine, _ := newAgentMachine("agentMachine", testNamespace, spec, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		agentMachineRequest := newAgentMachineRequest(agentMachine)

		goodLabels := map[string]string{"location": "datacenter2", "hasGpu": "true"}
		otherAgentMachineLabel := map[string]string{"location": "datacenter2", "hasGpu": "true", "agentMachineRef": "foo"}
		badLabels1 := map[string]string{"location": "datacenter1", "hasGpu": "false"}
		badLabels2 := map[string]string{"location": "datacenter2", "hasGpu": "false"}
		badLabels3 := map[string]string{"location": "datacenter1", "hasGpu": "true"}
		badLabels4 := map[string]string{"location": "datacenter2"}
		badLabels5 := map[string]string{"hasGpu": "true"}

		Expect(c.Create(ctx, newAgentWithProperties("agent-0", testNamespace, false, false, true, goodLabels))).To(BeNil())            // Agent0: not approved
		Expect(c.Create(ctx, newAgentWithProperties("agent-1", testNamespace, true, true, true, goodLabels))).To(BeNil())              // Agent1: already bound
		Expect(c.Create(ctx, newAgentWithProperties("agent-2", testNamespace, true, true, true, badLabels1))).To(BeNil())              // Agent2: bad labels
		Expect(c.Create(ctx, newAgentWithProperties("agent-3", testNamespace, true, true, false, goodLabels))).To(BeNil())             // Agent3: validations are failing
		Expect(c.Create(ctx, newAgentWithProperties("agent-4", testNamespace, true, false, true, badLabels2))).To(BeNil())             // Agent4: bad labels
		Expect(c.Create(ctx, newAgentWithProperties("agent-5", testNamespace, true, false, true, badLabels3))).To(BeNil())             // Agent5: bad labels
		Expect(c.Create(ctx, newAgentWithProperties("agent-6", testNamespace, true, false, true, badLabels4))).To(BeNil())             // Agent6: bad labels
		Expect(c.Create(ctx, newAgentWithProperties("agent-7", testNamespace, true, false, true, badLabels5))).To(BeNil())             // Agent7: bad labels
		Expect(c.Create(ctx, newAgentWithProperties("agent-8", testNamespace, true, false, true, otherAgentMachineLabel))).To(BeNil()) // Agent8: should be skipped
		Expect(c.Create(ctx, newAgentWithProperties("agent-9", testNamespace, true, false, true, goodLabels))).To(BeNil())             // Agent9: the chosen one

		// find agent
		result, err := amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine"}, agentMachine)).To(BeNil())
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentReservedCondition).Status).To(BeEquivalentTo("True"))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentSpecSyncedCondition).Status).To(BeEquivalentTo("True"))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentValidatedCondition).Status).To(BeEquivalentTo("True"))
		Expect(conditions.Get(agentMachine, capiproviderv1.AgentRequirementsMetCondition).Status).To(BeEquivalentTo("True"))
		Expect(conditions.Get(agentMachine, capiproviderv1.InstalledCondition).Status).To(BeEquivalentTo("False"))
		Expect(conditions.Get(agentMachine, capiproviderv1.InstalledCondition).Severity).To(BeEquivalentTo(clusterv1.ConditionSeverityInfo))
		Expect(conditions.Get(agentMachine, clusterv1.ReadyCondition).Status).To(BeEquivalentTo("False"))

		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-9"))
		Expect(len(agentMachine.Status.Addresses)).To(BeEquivalentTo(4))
		expectedAddresses := []string{"1.2.3.4", "2.3.4.5", "3.4.5.6", "agent-hostname"}
		expectedTypes := []string{string(clusterv1.MachineExternalIP), string(clusterv1.MachineInternalDNS)}
		for i := 0; i < len(agentMachine.Status.Addresses); i++ {
			Expect(funk.ContainsString(expectedAddresses, agentMachine.Status.Addresses[i].Address)).To(BeEquivalentTo(true))
			Expect(funk.ContainsString(expectedTypes, string(agentMachine.Status.Addresses[i].Type))).To(BeEquivalentTo(true))
		}

		agent := &aiv1beta1.Agent{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-9"}, agent)).To(BeNil())
		Expect(agent.Spec.ClusterDeploymentName.Name).To(BeEquivalentTo("cluster-deployment-agentMachine"))
		Expect(agent.Spec.IgnitionEndpointTokenReference.Name).To(BeEquivalentTo("agent-userdata-secret-agentMachine"))
		Expect(agent.Spec.IgnitionEndpointTokenReference.Namespace).To(BeEquivalentTo(testNamespace))

		agentSecret := &corev1.Secret{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent-userdata-secret-agentMachine"}, agentSecret)).To(BeNil())
		Expect(agentSecret.Data["ignition-token"]).To(BeEquivalentTo([]byte("encodedToken")))
	})

	It("agent bind-unbind cleanup", func() {
		spec := capiproviderv1.AgentMachineSpec{AgentLabelSelector: &metav1.LabelSelector{}}
		agentMachine, machine := newAgentMachine("agentMachine", testNamespace, spec, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		agentMachineRequest := newAgentMachineRequest(agentMachine)

		Expect(c.Create(ctx, newAgentWithProperties("agent", testNamespace, true, false, true, map[string]string{}))).To(BeNil())

		originalAgent := &aiv1beta1.Agent{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent"}, originalAgent)).To(BeNil())

		// associate agent with agentMachine
		result, err := amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent"))

		boundAgent := &aiv1beta1.Agent{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent"}, boundAgent)).To(BeNil())
		Expect(boundAgent.Spec.IgnitionEndpointTokenReference).ToNot(BeNil())
		Expect(boundAgent.Spec.MachineConfigPool).ToNot(BeEmpty())

		// mark the machine for deletion
		Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
		conditions.MarkFalse(machine, clusterv1.PreTerminateDeleteHookSucceededCondition, clusterv1.WaitingExternalHookReason, clusterv1.ConditionSeverityInfo, "")
		machine.Annotations[machineDeleteHookName] = ""
		Expect(c.Update(ctx, machine)).To(BeNil())

		result, err = amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result.RequeueAfter).To(Equal(5 * time.Second))

		// check that the now unbound agent looks like it did before binding
		unboundAgent := &aiv1beta1.Agent{}
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agent"}, unboundAgent)).To(BeNil())
		Expect(unboundAgent.Labels).To(BeEquivalentTo(originalAgent.Labels))
		Expect(unboundAgent.Annotations).To(BeEquivalentTo(originalAgent.Annotations))
		Expect(unboundAgent.Spec).To(BeEquivalentTo(originalAgent.Spec))
	})

	It("agentMachine agent with missing agentref", func() {
		agentMachine, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		agentMachineRequest := newAgentMachineRequest(agentMachine)

		agent := newAgentWithProperties("agent-1", testNamespace, false, false, true, map[string]string{})
		Expect(c.Create(ctx, agent)).To(BeNil())

		clusterDepRef := capiproviderv1.ClusterDeploymentReference{Namespace: testNamespace, Name: "my-cd"}
		log := amr.Log.WithFields(logrus.Fields{"agent_machine": "agentMachine-1", "agent_machine_namespace": testNamespace})
		Expect(amr.updateFoundAgent(ctx, log, agentMachine, agent, clusterDepRef, "", nil, nil)).To(BeNil())

		result, err := amr.Reconcile(ctx, agentMachineRequest)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(BeNil())
		Expect(agentMachine.Status.AgentRef.Name).To(BeEquivalentTo("agent-1"))
	})

	It("non-existing agentMachine", func() {
		agentMachine, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		nonExistingAgentMachine, _ := newAgentMachine("agentMachine-2", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(nonExistingAgentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("agentMachine ready status", func() {
		agentMachine, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		agentMachine.Status.Ready = true
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))
	})

	It("removes agentmachine finalizer if agent is missing", func() {
		agentMachine, machine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
		Expect(c.Create(ctx, agentMachine)).To(BeNil())
		Expect(c.Delete(ctx, machine)).To(BeNil())
		Expect(c.Delete(ctx, agentMachine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).ToNot(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		getError := c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)
		Expect(apierrors.IsNotFound(getError)).To(BeTrue())
	})

	It("removes the delete hook and finalizer when agent ref is not set and machine is waiting on delete hook", func() {
		agentMachine, machine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		agentMachine.Status.Ready = true
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		conditions.MarkFalse(machine, clusterv1.PreTerminateDeleteHookSucceededCondition, clusterv1.WaitingExternalHookReason, clusterv1.ConditionSeverityInfo, "")
		machine.Annotations[machineDeleteHookName] = ""
		Expect(c.Update(ctx, machine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
		Expect(machine.GetAnnotations()).ToNot(HaveKey(machineDeleteHookName))
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(agentMachine, AgentMachineFinalizerName)).To(BeFalse())
	})

	It("removes the delete hook and finalizer when agent is missing and machine is waiting on delete hook", func() {
		agentMachine, machine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		agentMachine.Status.Ready = true
		agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Namespace: testNamespace, Name: "missingAgent"}
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		conditions.MarkFalse(machine, clusterv1.PreTerminateDeleteHookSucceededCondition, clusterv1.WaitingExternalHookReason, clusterv1.ConditionSeverityInfo, "")
		machine.Annotations[machineDeleteHookName] = ""
		Expect(c.Update(ctx, machine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
		Expect(machine.GetAnnotations()).ToNot(HaveKey(machineDeleteHookName))
		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(agentMachine, AgentMachineFinalizerName)).To(BeFalse())
	})

	It("removes the finalizer when the delete hook has already been removed", func() {
		agentMachine, machine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
		agentMachine.Status.Ready = true
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		conditions.MarkTrue(machine, clusterv1.PreTerminateDeleteHookSucceededCondition)
		Expect(c.Update(ctx, machine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(agentMachine, AgentMachineFinalizerName)).To(BeFalse())
	})

	It("unbinds the agent and requeues when machine is waiting on delete hook", func() {
		agent := newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: false})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "True"})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})

		agentMachine, machine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		agentMachine.Status.Ready = true
		agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Namespace: agent.Namespace, Name: agent.Name}

		agent.ObjectMeta.Labels[AgentMachineRefLabelKey] = string(agentMachine.GetUID())
		agent.Spec.ClusterDeploymentName = &aiv1beta1.ClusterReference{
			Name:      fmt.Sprintf("cluster-deployment-%s", agentMachine.Name),
			Namespace: agentMachine.Namespace,
		}

		Expect(c.Create(ctx, agent)).To(BeNil())
		Expect(c.Create(ctx, agentMachine)).To(BeNil())

		conditions.MarkFalse(machine, clusterv1.PreTerminateDeleteHookSucceededCondition, clusterv1.WaitingExternalHookReason, clusterv1.ConditionSeverityInfo, "")
		machine.Annotations[machineDeleteHookName] = ""
		Expect(c.Update(ctx, machine)).To(BeNil())

		result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
		Expect(err).To(BeNil())
		Expect(result.RequeueAfter).To(Equal(5 * time.Second))

		Expect(c.Get(ctx, types.NamespacedName{Namespace: agent.Namespace, Name: agent.Name}, agent)).To(BeNil())
		Expect(agent.Labels).ToNot(HaveKey(AgentMachineRefLabelKey))
		Expect(agent.Spec.ClusterDeploymentName).To(BeNil())
	})

	Context("waiting for agent", func() {
		var (
			agent        *aiv1beta1.Agent
			agentMachine *capiproviderv1.AgentMachine
			machine      *clusterv1.Machine
		)

		BeforeEach(func() {
			agent = newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: false})
			agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "True"})
			agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})

			agentMachine, machine = newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
			agentMachine.Status.Ready = true
			agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Namespace: agent.Namespace, Name: agent.Name}

			Expect(c.Create(ctx, agent)).To(BeNil())
			Expect(c.Create(ctx, agentMachine)).To(BeNil())

			conditions.MarkFalse(machine, clusterv1.PreTerminateDeleteHookSucceededCondition, clusterv1.WaitingExternalHookReason, clusterv1.ConditionSeverityInfo, "")
			machine.Annotations[machineDeleteHookName] = ""
			Expect(c.Update(ctx, machine)).To(BeNil())
		})

		It("removes the delete hook and finalizer when the agent reaches discovering unbound rebooting", func() {
			agent.Status.DebugInfo.State = aimodels.HostStatusDiscoveringUnbound
			Expect(c.Update(ctx, agent)).To(BeNil())

			result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
			Expect(machine.GetAnnotations()).ToNot(HaveKey(machineDeleteHookName))
			Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(agentMachine, AgentMachineFinalizerName)).To(BeFalse())
		})

		It("removes the delete hook and finalizer when the agent reaches unbinding pending user action", func() {
			agent.Status.DebugInfo.State = aimodels.HostStatusUnbindingPendingUserAction
			Expect(c.Update(ctx, agent)).To(BeNil())

			result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
			Expect(machine.GetAnnotations()).ToNot(HaveKey(machineDeleteHookName))
			Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(agentMachine, AgentMachineFinalizerName)).To(BeFalse())
		})
	})
	Context("pausing agentmachine", func() {
		var (
			agent        *aiv1beta1.Agent
			agentMachine *capiproviderv1.AgentMachine
			machine      *clusterv1.Machine
		)

		BeforeEach(func() {
			agent = newAgent("agent-1", testNamespace, aiv1beta1.AgentSpec{Approved: false})
			agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: "True"})
			agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: "True"})
			agent.Spec.ClusterDeploymentName = &aiv1beta1.ClusterReference{Name: "cluster-deployment-agentMachine-1", Namespace: testNamespace}
			agentMachine, machine = newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
			agentMachine.Status.Ready = true
			agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Namespace: agent.Namespace, Name: agent.Name}

			Expect(c.Create(ctx, agent)).To(BeNil())
			Expect(c.Create(ctx, agentMachine)).To(BeNil())
		})

		AfterEach(func() {
			mockCtrl.Finish()
		})
		It("skips reconcile of a paused agent machine", func() {
			agentMachine.ObjectMeta.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
			Expect(c.Update(ctx, agentMachine)).To(Succeed())
			machine.Labels = nil
			Expect(c.Update(ctx, machine)).To(Succeed())

			result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("removes the delete hook and finalizer when the agentmachine is paused and being deleted without unbinding the agent", func() {
			agentMachine.ObjectMeta.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
			controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
			Expect(c.Update(ctx, agentMachine)).To(Succeed())
			Expect(c.Delete(ctx, agentMachine)).To(Succeed())

			result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
			Expect(machine.GetAnnotations()).ToNot(HaveKey(machineDeleteHookName))
			Expect(c.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "agentMachine-1"}, agentMachine)).NotTo(Succeed())
			Expect(c.Get(ctx, types.NamespacedName{Name: "agent-1", Namespace: testNamespace}, agent)).To(Succeed())
			Expect(agent.Spec.ClusterDeploymentName).NotTo(BeNil())
			Expect(agent.Spec.ClusterDeploymentName.Name).To(BeEquivalentTo("cluster-deployment-agentMachine-1"))
		})

		It("doesn't unbind the agent if the AgentMachine doesn't have a deletion timestamp", func() {
			agentMachine.ObjectMeta.Annotations = map[string]string{clusterv1.PausedAnnotation: "true"}
			controllerutil.AddFinalizer(agentMachine, AgentMachineFinalizerName)
			Expect(c.Update(ctx, agentMachine)).To(Succeed())

			// mark the machine for deletion
			Expect(c.Get(ctx, types.NamespacedName{Namespace: machine.Namespace, Name: machine.Name}, machine)).To(BeNil())
			conditions.MarkFalse(machine, clusterv1.PreTerminateDeleteHookSucceededCondition, clusterv1.WaitingExternalHookReason, clusterv1.ConditionSeverityInfo, "")
			machine.Annotations = map[string]string{machineDeleteHookName: ""}
			Expect(c.Update(ctx, machine)).To(BeNil())

			result, err := amr.Reconcile(ctx, newAgentMachineRequest(agentMachine))
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			Expect(c.Get(ctx, types.NamespacedName{Name: "agent-1", Namespace: testNamespace}, agent)).To(Succeed())
			Expect(agent.Spec.ClusterDeploymentName).NotTo(BeNil())
			Expect(agent.Spec.ClusterDeploymentName.Name).To(BeEquivalentTo("cluster-deployment-agentMachine-1"))
		})
	})
})

var _ = Describe("mapMachineToAgentMachine", func() {
	var (
		c             client.Client
		amr           *AgentMachineReconciler
		ctx           = context.Background()
		testNamespace = "test-namespace"
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		amr = &AgentMachineReconciler{
			Client:      c,
			Scheme:      scheme.Scheme,
			Log:         logrus.New(),
			AgentClient: c,
		}
	})

	It("returns a request for the related agent machine", func() {
		agentMachine, machine := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine)).To(Succeed())

		requests := amr.mapMachineToAgentMachine(ctx, machine)
		Expect(len(requests)).To(Equal(1))

		agentMachineKey := types.NamespacedName{
			Name:      "agentMachine-1",
			Namespace: testNamespace,
		}
		Expect(requests[0].NamespacedName).To(Equal(agentMachineKey))
	})

	It("returns an empty list when no agent machine is found", func() {
		// creates all the resource except the agent machine
		newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)

		key := types.NamespacedName{
			Name:      "machine-agentMachine-1",
			Namespace: testNamespace,
		}
		machine := clusterv1.Machine{}
		Expect(c.Get(ctx, key, &machine)).To(Succeed())

		Expect(amr.mapMachineToAgentMachine(ctx, &machine)).To(BeEmpty())
	})
})

var _ = Describe("mapAgentToAgentMachine", func() {
	var (
		c             client.Client
		amr           *AgentMachineReconciler
		ctx           = context.Background()
		testNamespace = "test-namespace"
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		amr = &AgentMachineReconciler{
			Client:      c,
			Scheme:      scheme.Scheme,
			Log:         logrus.New(),
			AgentClient: c,
		}
	})

	It("returns a request for the related agent machine", func() {
		agent := newAgent("agent", testNamespace, aiv1beta1.AgentSpec{})
		agent.ObjectMeta.Annotations[AgentMachineRefNamespace] = testNamespace
		Expect(c.Create(ctx, agent)).To(BeNil())
		agentMachine, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		agentMachine.Status.AgentRef = &capiproviderv1.AgentReference{Namespace: testNamespace, Name: "agent"}
		Expect(c.Create(ctx, agentMachine)).To(Succeed())

		requests := amr.mapAgentToAgentMachine(ctx, agent)
		Expect(len(requests)).To(Equal(1))

		agentMachineKey := types.NamespacedName{
			Name:      "agentMachine-1",
			Namespace: testNamespace,
		}
		Expect(requests[0].NamespacedName).To(Equal(agentMachineKey))
	})

	It("returns nothing if the agent isn't valid", func() {
		agent := newAgent("agent", testNamespace, aiv1beta1.AgentSpec{Approved: true})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: boolToConditionStatus(false)})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: boolToConditionStatus(false)})
		Expect(c.Create(ctx, agent)).To(BeNil())
		agentMachine1, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine1)).To(Succeed())
		agentMachine2, _ := newAgentMachine("agentMachine-2", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine2)).To(Succeed())

		requests := amr.mapAgentToAgentMachine(ctx, agent)
		Expect(len(requests)).To(Equal(0))
	})

	It("returns unmatched agent machines if no match is found", func() {
		agent := newAgent("agent", testNamespace, aiv1beta1.AgentSpec{Approved: true})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.BoundCondition, Status: boolToConditionStatus(false)})
		agent.Status.Conditions = append(agent.Status.Conditions, v1.Condition{Type: aiv1beta1.ValidatedCondition, Status: boolToConditionStatus(true)})
		Expect(c.Create(ctx, agent)).To(BeNil())
		agentMachine1, _ := newAgentMachine("agentMachine-1", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine1)).To(Succeed())
		agentMachine2, _ := newAgentMachine("agentMachine-2", testNamespace, capiproviderv1.AgentMachineSpec{}, ctx, c)
		Expect(c.Create(ctx, agentMachine2)).To(Succeed())

		requests := amr.mapAgentToAgentMachine(ctx, agent)
		Expect(len(requests)).To(Equal(2))
	})
})
