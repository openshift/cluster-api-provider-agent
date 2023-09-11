package controllers

import (
	"context"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("ensureSecretLabel", func() {
	var (
		c                    client.Client
		ctx                  = context.Background()
		ctrl                 *gomock.Controller
		secret               *corev1.Secret
		testNamespace        = "test-namespace"
		testSecretName       = "test"
		secretNamespacedName = types.NamespacedName{Name: testSecretName, Namespace: testNamespace}
	)
	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		ctrl = gomock.NewController(GinkgoT())
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testSecretName,
				Namespace: testNamespace,
			},
		}
	})
	AfterEach(func() {
		Expect(c.Delete)
		ctrl.Finish()
	})
	It("successfully labels secret when the secret exists and doesn't have labels", func() {
		Expect(c.Create(ctx, secret)).To(BeNil())
		Expect(ensureSecretLabel(ctx, c, secret)).To(Succeed())
		labeledSecret := &corev1.Secret{}
		Expect(c.Get(ctx, secretNamespacedName, labeledSecret)).To(Succeed())
		Expect(metav1.HasLabel(labeledSecret.ObjectMeta, BackupLabel)).To(BeTrue())
	})

	It("successfully labels secret when the secret exists and doesn't have the correct labels", func() {
		secret.ObjectMeta.Labels = map[string]string{"test-label": "test"}
		Expect(c.Create(ctx, secret)).To(BeNil())
		Expect(ensureSecretLabel(ctx, c, secret)).To(Succeed())
		labeledSecret := &corev1.Secret{}
		Expect(c.Get(ctx, secretNamespacedName, labeledSecret)).To(Succeed())
		Expect(metav1.HasLabel(labeledSecret.ObjectMeta, BackupLabel)).To(BeTrue())
	})

	It("doesn't label existing secret that already has the correct labels", func() {
		secret.ObjectMeta.Labels = map[string]string{BackupLabel: BackupLabelValue}
		Expect(c.Create(ctx, secret)).To(BeNil())
		Expect(ensureSecretLabel(ctx, c, secret)).To(Succeed())
		labeledSecret := &corev1.Secret{}
		Expect(c.Get(ctx, secretNamespacedName, labeledSecret)).To(Succeed())
		Expect(metav1.HasLabel(labeledSecret.ObjectMeta, BackupLabel)).To(BeTrue())
	})

	It("doesn't label a non-existing secret", func() {
		err := ensureSecretLabel(ctx, c, secret)
		Expect(err).To(HaveOccurred())
	})
})
