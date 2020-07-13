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
	"fmt"
	"github.com/go-logr/logr"
	"github.com/spiffe/spire/proto/spire/api/registration"
	"github.com/spiffe/spire/proto/spire/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"strings"
	"time"
)

type ObjectReconciler interface {
	// Returns an instance of the object type to be reconciled
	getObject() ObjectWithMetadata
	// Return a SPIFFE ID to register for the object, or "" if no registration should be created
	makeSpiffeId(ObjectWithMetadata) string
	// Return the SPIFFE ID to be used as a parent for the object, or "" if no registration should be created
	makeParentId(ObjectWithMetadata) string
	// Return all registration entries owned by the controller
	getAllEntries(context.Context) ([]*common.RegistrationEntry, error)
	// Return the selectors that should be used for a name
	// For example, given a name of "foo" a reconciler might return a `k8s_psat:node-name:foo` selector.
	getSelectors(types.NamespacedName) []*common.Selector
	// Parse the selectors to extract a namespaced name.
	// For example, a list containing a `k8s_psat:node-name:foo` selector might result in a NamespacedName of "foo"
	selectorsToNamespacedName([]*common.Selector) *types.NamespacedName
}

// BaseReconciler reconciles... something
// This implements the polling solution documented here: https://docs.google.com/document/d/19BDGrCRh9rjj09to1D2hlDJZXRuwOlY4hL5c4n7_bVc
// By using name+namespace as a key we are able to maintain a 1:1 mapping from k8s resources to SPIRE registration entries.
// The base reconciler implements the common functionality required to maintain that mapping, including a watcher on the
// given resource, and a watcher which receives notifications from polling the registration api.
type BaseReconciler struct {
	client.Client
	ObjectReconciler
	Scheme      *runtime.Scheme
	TrustDomain string
	RootId      string
	SpireClient registration.RegistrationClient
	Log         logr.Logger
}

type RuntimeObject = runtime.Object
type V1Object = v1.Object

type ObjectWithMetadata interface {
	RuntimeObject
	V1Object
}

func (r *BaseReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	reqLogger := r.Log.WithValues("request", req.NamespacedName)

	obj := r.getObject()
	err := r.Get(ctx, req.NamespacedName, obj)
	if err != nil && !errors.IsNotFound(err) {
		reqLogger.Error(err, "Unable to fetch resource")
		return ctrl.Result{}, err
	}

	isDeleted := errors.IsNotFound(err) || !obj.GetDeletionTimestamp().IsZero()

	matchedEntries, err := r.getMatchingEntries(ctx, reqLogger, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if isDeleted {
		reqLogger.V(1).Info("Deleting entries for deleted object", "count", len(matchedEntries))
		err := r.deleteAllEntries(ctx, reqLogger, matchedEntries)
		return ctrl.Result{}, err
	}

	myEntry := r.makeEntryForObject(obj)

	if myEntry == nil {
		// Object does not need an entry. This might be a change, so we need to delete any hanging entries.
		reqLogger.V(1).Info("Deleting entries for object that no longer needs an ID", "count", len(matchedEntries))
		err := r.deleteAllEntries(ctx, reqLogger, matchedEntries)
		return ctrl.Result{}, err
	}

	var myEntryId string

	if len(matchedEntries) == 0 {
		createEntryIfNotExistsResponse, err := r.SpireClient.CreateEntryIfNotExists(ctx, myEntry)
		if err != nil {
			reqLogger.Error(err, "Failed to create or update spire entry")
			return ctrl.Result{}, err
		}
		if createEntryIfNotExistsResponse.Preexisting {
			// This can only happen if multiple controllers are running, since any entry returned here should also have
			// been in matchedEntries!
			reqLogger.V(1).Info("Found existing identical spire entry", "entry", createEntryIfNotExistsResponse.Entry)
		} else {
			reqLogger.Info("Created new spire entry", "entry", createEntryIfNotExistsResponse.Entry)
		}
		myEntryId = createEntryIfNotExistsResponse.Entry.EntryId
	} else {
		// matchedEntries contains all entries created by this controller (based on parent ID) whose selectors match the object
		// being reconciled. Typically there will be only one. One of these existing entries might already be just right, but
		// if not, we choose one and update it (e.g. change spiffe ID or dns names, avoiding causing a period where the workload
		// has no SVID.) We then delete all the others.
		requiresUpdate := true
		for _, entry := range matchedEntries {
			if r.entryEquals(myEntry, entry) {
				reqLogger.V(1).Info("Found existing identical enough spire entry", "entry", entry.EntryId)
				myEntryId = entry.EntryId
				requiresUpdate = false
				break
			}
		}
		if requiresUpdate {
			reqLogger.V(1).Info("Updating existing spire entry to match desired state", "entry", matchedEntries[0].EntryId)
			// It's important that if multiple instances are running they all pick the same entry here, otherwise
			// we could have two instances of the registrar delete each others changes. This can only happen if both are
			// also working off an up to date cache (otherwise the lagging one will pick up the other change later and correct
			// the mistake.) If that happens then as long as they'd both pick the same entry to keep off the list, we can
			// guarantee they wont end up deleting all the entries and not noticing: so we'll pick the entry with the
			// "lowest" Entry ID.
			myEntryId := matchedEntries[0].EntryId
			for _, entry := range matchedEntries {
				if entry.EntryId < myEntryId {
					myEntryId = entry.EntryId
				}
			}

			// myEntry is the entry we'd have created if we weren't updating an existing one, by giving it the chosen EntryId
			// we can use it to update the existing entry to perfectly match what we want.
			myEntry.EntryId = myEntryId
			_, err := r.SpireClient.UpdateEntry(ctx, &registration.UpdateEntryRequest{
				Entry: myEntry,
			})
			if err != nil {
				reqLogger.Error(err, "Failed to update existing spire entry", "existingEntry", matchedEntries[0].EntryId)
				return ctrl.Result{}, err
			}
		}
	}

	err = r.deleteAllEntriesExcept(ctx, reqLogger, matchedEntries, myEntryId)

	return ctrl.Result{}, err
}

func (r *BaseReconciler) makeEntryForObject(obj ObjectWithMetadata) *common.RegistrationEntry {
	spiffeId := r.makeSpiffeId(obj)
	parentId := r.makeParentId(obj)

	if spiffeId == "" || parentId == "" {
		return nil
	}

	return &common.RegistrationEntry{
		Selectors: r.getSelectors(types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		}),
		ParentId: parentId,
		SpiffeId: spiffeId,
	}
}

func (r *BaseReconciler) entryEquals(myEntry *common.RegistrationEntry, b *common.RegistrationEntry) bool {
	// TODO: Maybe this should be stricter on the other fields, but right now if you're editing entries on the server, I'll accept that odd things happen to you.
	return b.SpiffeId == myEntry.GetSpiffeId()
}

func (r *BaseReconciler) deleteAllEntries(ctx context.Context, reqLogger logr.Logger, entries []*common.RegistrationEntry) error {
	var errs []error
	for _, entry := range entries {
		err := r.ensureDeleted(ctx, reqLogger, entry.EntryId)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unable to delete all entries: %v", errs)
	}
	return nil
}

func (r *BaseReconciler) deleteAllEntriesExcept(ctx context.Context, reqLogger logr.Logger, entries []*common.RegistrationEntry, exceptEntryId string) error {
	var errs []error
	for _, entry := range entries {
		if entry.EntryId != exceptEntryId {
			err := r.ensureDeleted(ctx, reqLogger, entry.EntryId)
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("unable to delete all entries: %v", errs)
	}
	return nil
}

func (r *BaseReconciler) getMatchingEntries(ctx context.Context, reqLogger logr.Logger, namespacedName types.NamespacedName) ([]*common.RegistrationEntry, error) {
	entries, err := r.SpireClient.ListBySelectors(ctx, &common.Selectors{
		Entries: r.getSelectors(namespacedName),
	})
	if err != nil {
		reqLogger.Error(err, "Failed to load entries")
		return nil, err
	}
	var result []*common.RegistrationEntry
	for _, entry := range entries.Entries {
		if strings.HasPrefix(entry.ParentId, r.RootId) {
			result = append(result, entry)
		}
	}
	return result, nil
}

func (r *NodeReconciler) k8sNodeSelector(selector NodeSelectorSubType, value string) *common.Selector {
	return &common.Selector{
		Type:  "k8s_psat",
		Value: fmt.Sprintf("%s:%s", selector, value),
	}
}

func (r *BaseReconciler) ensureDeleted(ctx context.Context, reqLogger logr.Logger, entryId string) error {
	if _, err := r.SpireClient.DeleteEntry(ctx, &registration.RegistrationEntryID{Id: entryId}); err != nil {
		if status.Code(err) != codes.NotFound {
			if status.Code(err) == codes.Internal {
				// Spire server currently returns a 500 if delete fails due to the entry not existing. This is probably a bug.
				// We work around it by attempting to fetch the entry, and if it's not found then all is good.
				if _, err := r.SpireClient.FetchEntry(ctx, &registration.RegistrationEntryID{Id: entryId}); err != nil {
					if status.Code(err) == codes.NotFound {
						reqLogger.V(1).Info("Entry already deleted", "entry", entryId)
						return nil
					}
				}
			}
			return err
		}
	}
	reqLogger.Info("deleted entry", "entry", entryId)
	return nil
}

func (r *BaseReconciler) pollSpire(out chan event.GenericEvent, s <-chan struct{}) error {
	ctx := context.Background()
	log := r.Log
	for {
		select {
		case <-s:
			return nil
		case <-time.After(10 * time.Second):
			log.Info("Syncing spire entries")
			start := time.Now()
			seen := make(map[string]bool)
			entries, err := r.getAllEntries(ctx)
			if err != nil {
				log.Error(err, "Unable to fetch entries")
				break
			}
			queued := 0
			for _, entry := range entries {
				if namespacedName := r.selectorsToNamespacedName(entry.Selectors); namespacedName != nil {
					reconcile := false
					if seen[namespacedName.String()] {
						// More than one entry found
						reconcile = true
					} else {
						obj := r.getObject()
						err := r.Get(ctx, *namespacedName, obj)
						if err != nil {
							if errors.IsNotFound(err) {
								// resource has been deleted
								reconcile = true
							} else {
								log.Error(err, "Unable to fetch resource", "name", namespacedName)
							}
						} else {
							myEntry := r.makeEntryForObject(obj)
							if myEntry == nil || !r.entryEquals(myEntry, entry) {
								// No longer needs an entry or it doesn't match the expected entry
								// This can trigger for various reasons, but it's OK to accidentally queue more than entirely necessary
								reconcile = true
							}
						}
					}
					seen[namespacedName.String()] = true
					if reconcile {
						queued++
						log.V(1).Info("Triggering reconciliation for resource", "name", namespacedName)
						out <- event.GenericEvent{Meta: &v1.ObjectMeta{
							Name:      namespacedName.Name,
							Namespace: namespacedName.Namespace,
						}}
					}
				}
			}
			log.Info("Synced spire entries", "took", time.Since(start), "found", len(entries), "queued", queued)
		}
	}
}

type SpirePoller struct {
	r   *BaseReconciler
	out chan event.GenericEvent
}

// Start implements Runnable
func (p *SpirePoller) Start(s <-chan struct{}) error {
	return p.r.pollSpire(p.out, s)
}

func (r *BaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	events := make(chan event.GenericEvent)

	err := mgr.Add(&SpirePoller{
		r:   r,
		out: events,
	})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(r.getObject()).
		Watches(&source.Channel{Source: events}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}
