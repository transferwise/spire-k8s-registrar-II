/*

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
	"github.com/google/uuid"
	"github.com/spiffe/spire/proto/spire/api/registration"
	"github.com/spiffe/spire/proto/spire/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"path/filepath"
	ctrl "sigs.k8s.io/controller-runtime"
	"testing"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment
var spireClient registration.RegistrationClient
var k8sManager ctrl.Manager

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecsWithDefaultAndCustomReporters(t,
		"Controller Suite",
		[]Reporter{envtest.NewlineReporter{}})
}

var _ = BeforeSuite(func(done Done) {
	logf.SetLogger(zap.LoggerTo(GinkgoWriter, true))

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "config", "crd", "bases")},
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).ToNot(HaveOccurred())
	Expect(cfg).ToNot(BeNil())

	err = corev1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).ToNot(HaveOccurred())

	k8sClient = k8sManager.GetClient()
	Expect(k8sClient).ToNot(BeNil())

	spireClient = NewMockSpireService()
	Expect(spireClient).ToNot(BeNil())

	_, err = spireClient.CreateEntry(context.Background(), &common.RegistrationEntry{
		SpiffeId: "spiffe://foo.bar.spire/rootId",
	})
	Expect(err).ToNot(HaveOccurred())

	err = NewPodReconciler(
		k8sClient,
		ctrl.Log.WithName("controllers").WithName("Pod"),
		k8sManager.GetScheme(),
		"foo.bar.spire",
		"spiffe://foo.bar.spire/rootId",
		spireClient,
		PodReconcilerModeLabel,
		"spiffe",
		"",
		false,
	).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		err = k8sManager.Start(ctrl.SetupSignalHandler())
		Expect(err).ToNot(HaveOccurred())
	}()

	close(done)
}, 60)

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
})

type MockSpireService struct {
	entriesById        map[string]*common.RegistrationEntry
}

func NewMockSpireService() MockSpireService {
	return MockSpireService{
		entriesById:        map[string]*common.RegistrationEntry{},
	}
}

func (m MockSpireService) getMatchingEntry(in *common.RegistrationEntry) (*common.RegistrationEntry, error) {
nextEntry:
	for _, entry := range m.entriesById {
		if entry.SpiffeId != in.SpiffeId {
			continue nextEntry
		}

		if entry.ParentId != in.ParentId {
			continue nextEntry
		}
		if len(entry.Selectors) != len(in.Selectors) {
			continue nextEntry
		}
		var selectorMap map[string]map[string]bool
		for _, selector := range entry.Selectors {
			selectorMap[selector.Type][selector.Value] = true
		}
		for _, selector := range in.Selectors {
			if !selectorMap[selector.Type][selector.Value] {
				continue nextEntry
			}
		}
		return m.cloneEntry(entry)
	}
	return nil, nil
}

func (m MockSpireService) cloneEntry(in *common.RegistrationEntry) (*common.RegistrationEntry, error) {
	if in == nil {
		return nil, nil
	}

	encoded, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	re := common.RegistrationEntry{}
	err = json.Unmarshal(encoded, &re)
	if err != nil {
		return nil, err
	}
	return &re, nil
}

func (m MockSpireService) CreateEntry(ctx context.Context, in *common.RegistrationEntry, opts ...grpc.CallOption) (*registration.RegistrationEntryID, error) {
	ceif, err := m.CreateEntryIfNotExists(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	if ceif.Preexisting {
		return nil, status.Error(codes.AlreadyExists, "Entry already exists")
	}
	return &registration.RegistrationEntryID{Id: ceif.Entry.EntryId}, nil
}

func (m MockSpireService) CreateEntryIfNotExists(ctx context.Context, in *common.RegistrationEntry, opts ...grpc.CallOption) (*registration.CreateEntryIfNotExistsResponse, error) {
	e, err := m.getMatchingEntry(in)
	if err != nil {
		return nil, err
	}
	if e != nil {
		return &registration.CreateEntryIfNotExistsResponse{
			Entry:       e,
			Preexisting: true,
		}, nil
	}
	entryId, err := uuid.NewUUID()
	if err != nil {
		return nil, err
	}
	e, err = m.cloneEntry(in)
	if err != nil {
		return nil, err
	}
	e.EntryId = entryId.String()
	m.entriesById[e.EntryId] = e
	return &registration.CreateEntryIfNotExistsResponse{
		Entry:       e,
		Preexisting: false,
	}, nil
}

func (m MockSpireService) DeleteEntry(ctx context.Context, in *registration.RegistrationEntryID, opts ...grpc.CallOption) (*common.RegistrationEntry, error) {
	entry, ok := m.entriesById[in.Id]
	if !ok {
		return nil, status.Error(codes.NotFound, "Entry not found")
	}
	delete(m.entriesById, in.Id)

	return entry, nil
}

func (m MockSpireService) fetchByFilter(accept func(*common.RegistrationEntry) bool) (*common.RegistrationEntries, error) {
	results := common.RegistrationEntries{}

	for _, entry := range m.entriesById {
		if accept(entry) {
			clone, err := m.cloneEntry(entry)
			if err != nil {
				return nil, err
			}
			results.Entries = append(results.Entries, clone)
		}
	}
	return &results, nil
}

func (m MockSpireService) FetchEntry(ctx context.Context, in *registration.RegistrationEntryID, opts ...grpc.CallOption) (*common.RegistrationEntry, error) {
	panic("implement me")
}

func (m MockSpireService) FetchEntries(ctx context.Context, in *common.Empty, opts ...grpc.CallOption) (*common.RegistrationEntries, error) {
	return m.fetchByFilter(func(entry *common.RegistrationEntry) bool {
		return true
	})}

func (m MockSpireService) UpdateEntry(ctx context.Context, in *registration.UpdateEntryRequest, opts ...grpc.CallOption) (*common.RegistrationEntry, error) {
	panic("implement me")
}

func (m MockSpireService) ListByParentID(ctx context.Context, in *registration.ParentID, opts ...grpc.CallOption) (*common.RegistrationEntries, error) {
	return m.fetchByFilter(func(entry *common.RegistrationEntry) bool {
		return entry.ParentId == in.Id
	})
}

func (m MockSpireService) ListBySelector(ctx context.Context, in *common.Selector, opts ...grpc.CallOption) (*common.RegistrationEntries, error) {
	panic("implement me")
}

func (m MockSpireService) ListBySelectors(ctx context.Context, in *common.Selectors, opts ...grpc.CallOption) (*common.RegistrationEntries, error) {
	hunting := make(map[string]map[string]bool)
	for _, selector := range in.Entries {
		if _, ok := hunting[selector.Type]; !ok {
			hunting[selector.Type] = make(map[string]bool)
		}
		hunting[selector.Type][selector.Value] = true
	}

	return m.fetchByFilter(func(entry *common.RegistrationEntry) bool {
		if len(entry.Selectors) != len(in.Entries) {
			return false
		}

		for _, selector := range entry.Selectors {
			if !hunting[selector.Type][selector.Value] {
				return false
			}
		}
		return true
	})
}

func (m MockSpireService) ListBySpiffeID(ctx context.Context, in *registration.SpiffeID, opts ...grpc.CallOption) (*common.RegistrationEntries, error) {
	return m.fetchByFilter(func(entry *common.RegistrationEntry) bool {
		return entry.SpiffeId == in.Id
	})
}

func (m MockSpireService) ListAllEntriesWithPages(ctx context.Context, in *registration.ListAllEntriesRequest, opts ...grpc.CallOption) (*registration.ListAllEntriesResponse, error) {
	panic("implement me")
}

func (m MockSpireService) CreateFederatedBundle(ctx context.Context, in *registration.FederatedBundle, opts ...grpc.CallOption) (*common.Empty, error) {
	panic("implement me")
}

func (m MockSpireService) FetchFederatedBundle(ctx context.Context, in *registration.FederatedBundleID, opts ...grpc.CallOption) (*registration.FederatedBundle, error) {
	panic("implement me")
}

func (m MockSpireService) ListFederatedBundles(ctx context.Context, in *common.Empty, opts ...grpc.CallOption) (registration.Registration_ListFederatedBundlesClient, error) {
	panic("implement me")
}

func (m MockSpireService) UpdateFederatedBundle(ctx context.Context, in *registration.FederatedBundle, opts ...grpc.CallOption) (*common.Empty, error) {
	panic("implement me")
}

func (m MockSpireService) DeleteFederatedBundle(ctx context.Context, in *registration.DeleteFederatedBundleRequest, opts ...grpc.CallOption) (*common.Empty, error) {
	panic("implement me")
}

func (m MockSpireService) CreateJoinToken(ctx context.Context, in *registration.JoinToken, opts ...grpc.CallOption) (*registration.JoinToken, error) {
	panic("implement me")
}

func (m MockSpireService) FetchBundle(ctx context.Context, in *common.Empty, opts ...grpc.CallOption) (*registration.Bundle, error) {
	panic("implement me")
}

func (m MockSpireService) EvictAgent(ctx context.Context, in *registration.EvictAgentRequest, opts ...grpc.CallOption) (*registration.EvictAgentResponse, error) {
	panic("implement me")
}

func (m MockSpireService) ListAgents(ctx context.Context, in *registration.ListAgentsRequest, opts ...grpc.CallOption) (*registration.ListAgentsResponse, error) {
	panic("implement me")
}

func (m MockSpireService) MintX509SVID(ctx context.Context, in *registration.MintX509SVIDRequest, opts ...grpc.CallOption) (*registration.MintX509SVIDResponse, error) {
	panic("implement me")
}

func (m MockSpireService) MintJWTSVID(ctx context.Context, in *registration.MintJWTSVIDRequest, opts ...grpc.CallOption) (*registration.MintJWTSVIDResponse, error) {
	panic("implement me")
}

func (m MockSpireService) GetNodeSelectors(ctx context.Context, in *registration.GetNodeSelectorsRequest, opts ...grpc.CallOption) (*registration.GetNodeSelectorsResponse, error) {
	panic("implement me")
}
