package controllers

import (
	"context"
	"github.com/spiffe/spire/proto/spire/common"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spiffe/spire/proto/spire/api/registration"
)

var _ = Describe("Pod Controller", func() {

	const timeout = time.Second * 30
	const interval = time.Second * 1

	BeforeEach(func() {
	})

	AfterEach(func() {
		// Add any teardown steps that needs to be executed after each test
	})

	Context("Pod controller label mode", func() {
		It("Should create a Spire Entry", func() {
			toCreate := testPod(types.NamespacedName{
				Name:      "test-pod",
				Namespace: "default",
			})
			toCreate.ObjectMeta.Labels["spiffe"] = "testpod"
			toCreate.Spec.NodeName = "ip-123-123-123-123"

			By("Creating a pod with a label")
			Expect(k8sClient.Create(context.Background(), toCreate)).Should(Succeed())

			Eventually(func() ([]*common.RegistrationEntry, error) {
				entries, err := spireClient.ListBySpiffeID(context.Background(), &registration.SpiffeID{
					Id: "spiffe://foo.bar.spire/testpod",
				})
				if entries == nil {
					return nil, err
				}
				return entries.Entries, err
			}, timeout, interval).ShouldNot(BeEmpty())

			By("Deleting the pod")

			Expect(k8sClient.Delete(context.Background(), toCreate)).Should(Succeed())

			Eventually(func() ([]*common.RegistrationEntry, error) {
				entries, err := spireClient.ListBySpiffeID(context.Background(), &registration.SpiffeID{
					Id: "spiffe://foo.bar.spire/testpod",
				})
				if entries == nil {
					return nil, err
				}
				return entries.Entries, err
			}, timeout, interval).Should(BeEmpty())

		})
	})

})

func testPod(key types.NamespacedName) *corev1.Pod {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:  "foo",
				Image: "bar",
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				"spiffe": "testpod",
			},
		},
		Spec: spec,
	}
	return pod
}
