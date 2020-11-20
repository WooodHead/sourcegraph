package campaigns

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pkg/errors"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/backend"
	"github.com/sourcegraph/sourcegraph/cmd/frontend/types"
	"github.com/sourcegraph/sourcegraph/cmd/repo-updater/repos"
	"github.com/sourcegraph/sourcegraph/internal/actor"
	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/campaigns"
	"github.com/sourcegraph/sourcegraph/internal/db"
	"github.com/sourcegraph/sourcegraph/internal/trace"
)

// ErrApplyClosedCampaign is returned by ApplyCampaign when the campaign
// matched by the campaign spec is already closed.
var ErrApplyClosedCampaign = errors.New("existing campaign matched by campaign spec is closed")

// ErrMatchingCampaignExists is returned by ApplyCampaign if a campaign matching the
// campaign spec already exists and FailIfExists was set.
var ErrMatchingCampaignExists = errors.New("a campaign matching the given campaign spec already exists")

type ApplyCampaignOpts struct {
	CampaignSpecRandID string
	EnsureCampaignID   int64

	// When FailIfCampaignExists is true, ApplyCampaign will fail if a Campaign
	// matching the given CampaignSpec already exists.
	FailIfCampaignExists bool
}

func (o ApplyCampaignOpts) String() string {
	return fmt.Sprintf(
		"CampaignSpec %s, EnsureCampaignID %d",
		o.CampaignSpecRandID,
		o.EnsureCampaignID,
	)
}

// ApplyCampaign creates the CampaignSpec.
func (s *Service) ApplyCampaign(ctx context.Context, opts ApplyCampaignOpts) (campaign *campaigns.Campaign, err error) {
	tr, ctx := trace.New(ctx, "Service.ApplyCampaign", opts.String())
	defer func() {
		tr.SetError(err)
		tr.Finish()
	}()

	campaignSpec, err := s.store.GetCampaignSpec(ctx, GetCampaignSpecOpts{
		RandID: opts.CampaignSpecRandID,
	})
	if err != nil {
		return nil, err
	}

	// 🚨 SECURITY: Only site-admins or the creator of campaignSpec can apply
	// campaignSpec.
	if err := backend.CheckSiteAdminOrSameUser(ctx, campaignSpec.UserID); err != nil {
		return nil, err
	}

	campaign, err = s.GetCampaignMatchingCampaignSpec(ctx, s.store, campaignSpec)
	if err != nil {
		return nil, err
	}
	if campaign == nil {
		campaign = &campaigns.Campaign{}
	} else if opts.FailIfCampaignExists {
		return nil, ErrMatchingCampaignExists
	}

	if opts.EnsureCampaignID != 0 && campaign.ID != opts.EnsureCampaignID {
		return nil, ErrEnsureCampaignFailed
	}

	if campaign.Closed() {
		return nil, ErrApplyClosedCampaign
	}

	if campaign.CampaignSpecID == campaignSpec.ID {
		return campaign, nil
	}

	// Before we write to the database in a transaction, we cancel all
	// currently enqueued/errored-and-retryable changesets the campaign might
	// have.
	// We do this so we don't continue to possibly create changesets on the
	// codehost while we're applying a new campaign spec.
	// This is blocking, because the changeset rows currently being processed by the
	// reconciler are locked.
	if err := s.store.CancelQueuedCampaignChangesets(ctx, campaign.ID); err != nil {
		return campaign, nil
	}

	tx, err := s.store.Transact(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = tx.Done(err) }()

	// Populate the campaign with the values from the campaign spec.
	campaign.CampaignSpecID = campaignSpec.ID
	campaign.NamespaceOrgID = campaignSpec.NamespaceOrgID
	campaign.NamespaceUserID = campaignSpec.NamespaceUserID
	campaign.Name = campaignSpec.Spec.Name
	actor := actor.FromContext(ctx)
	if campaign.InitialApplierID == 0 {
		campaign.InitialApplierID = actor.UID
	}
	campaign.LastApplierID = actor.UID
	campaign.LastAppliedAt = s.clock()
	campaign.Description = campaignSpec.Spec.Description

	if campaign.ID == 0 {
		err := tx.CreateCampaign(ctx, campaign)
		if err != nil {
			return nil, err
		}
	}

	rstore := repos.NewDBStore(tx.DB(), sql.TxOptions{})
	// Now we need to wire up the ChangesetSpecs of the new CampaignSpec
	// correctly with the Changesets so that the reconciler can create/update
	// them.
	rewirer := &changesetRewirer{
		tx:       tx,
		rstore:   rstore,
		campaign: campaign,
	}

	if err := rewirer.Rewire(ctx); err != nil {
		return nil, err
	}

	return campaign, nil
}

type changesetRewirer struct {
	campaign *campaigns.Campaign
	tx       *Store
	rstore   repos.Store

	// These fields are populated by loadAssociations
	changesets          campaigns.Changesets
	newChangesetSpecs   campaigns.ChangesetSpecs
	accessibleReposByID map[api.RepoID]*types.Repo

	// These are populated by indexAssociations
	changesetsByID     map[int64]*campaigns.Changeset
	changesetSpecsByID map[int64]*campaigns.ChangesetSpec
}

// Rewire loads the current changesets of the given campaign, the changeset
// specs attached to the new campaign spec and rewires them so that the
// changesets are associated with the correct changeset specs and with the
// campaign.
//
// It also updates the ChangesetIDs on the campaign.
func (r *changesetRewirer) Rewire(ctx context.Context) (err error) {
	// First we need to load the associations
	if err := r.loadAssociations(ctx); err != nil {
		return err
	}

	// Now we put them into buckets so we can match easily
	if err := r.indexAssociations(ctx); err != nil {
		return err
	}

	mappings, err := r.tx.GetChangesetSpecRewireData(ctx, r.campaign.CampaignSpecID, r.campaign.ID)
	if err != nil {
		return err
	}

	// Now we have two lists, the current changesets and the new changeset specs:

	// ┌───────────────────────────────────────┐   ┌───────────────────────────────┐
	// │Changeset 1 | Repo A | #111 | run-gofmt│   │  Spec 1 | Repo A | run-gofmt  │
	// └───────────────────────────────────────┘   └───────────────────────────────┘
	// ┌───────────────────────────────────────┐   ┌───────────────────────────────┐
	// │Changeset 2 | Repo B |      | run-gofmt│   │  Spec 2 | Repo B | run-gofmt  │
	// └───────────────────────────────────────┘   └───────────────────────────────┘
	// ┌───────────────────────────────────────┐   ┌───────────────────────────────────┐
	// │Changeset 3 | Repo C | #222 | run-gofmt│   │  Spec 3 | Repo C | run-goimports  │
	// └───────────────────────────────────────┘   └───────────────────────────────────┘
	// ┌───────────────────────────────────────┐   ┌───────────────────────────────┐
	// │Changeset 4 | Repo C | #333 | older-pr │   │    Spec 4 | Repo C | #333     │
	// └───────────────────────────────────────┘   └───────────────────────────────┘

	// We need to:
	// 1. Find out whether our new specs should _update_ an existing
	//    changeset, or whether we need to create a new one.
	// 2. Since we can have multiple changesets per repository, we need to match
	//    based on repo and external ID.
	// 3. But if a changeset wasn't published yet, it doesn't have an external ID.
	//    In that case, we need to check whether the branch on which we _might_
	//    push the commit (because the changeset might not be published
	//    yet) is the same.

	// What we want:
	//
	// ┌───────────────────────────────────────┐    ┌───────────────────────────────┐
	// │Changeset 1 | Repo A | #111 | run-gofmt│───▶│  Spec 1 | Repo A | run-gofmt  │
	// └───────────────────────────────────────┘    └───────────────────────────────┘
	// ┌───────────────────────────────────────┐    ┌───────────────────────────────┐
	// │Changeset 2 | Repo B |      | run-gofmt│───▶│  Spec 2 | Repo B | run-gofmt  │
	// └───────────────────────────────────────┘    └───────────────────────────────┘
	// ┌───────────────────────────────────────┐
	// │Changeset 3 | Repo C | #222 | run-gofmt│
	// └───────────────────────────────────────┘
	// ┌───────────────────────────────────────┐    ┌───────────────────────────────┐
	// │Changeset 4 | Repo C | #333 | older-pr │───▶│    Spec 4 | Repo C | #333     │
	// └───────────────────────────────────────┘    └───────────────────────────────┘
	// ┌───────────────────────────────────────┐    ┌───────────────────────────────────┐
	// │Changeset 5 | Repo C | | run-goimports │───▶│  Spec 3 | Repo C | run-goimports  │
	// └───────────────────────────────────────┘    └───────────────────────────────────┘
	//
	// Spec 1 should be attached to Changeset 1 and (possibly) update its title/body/diff.
	// Spec 2 should be attached to Changeset 2 and publish it on the code host.
	// Spec 3 should get a new Changeset, since its branch doesn't match Changeset 3's branch.
	// Spec 4 should be attached to Changeset 4, since it tracks PR #333 in Repo C.
	// Changeset 3 doesn't have a matching spec and should be detached from the campaign (and closed).

	attachedChangesets := map[int64]bool{}
	for _, m := range mappings {
		spec, specFound := r.changesetSpecsByID[m.ChangesetSpecID]
		if !specFound {
			return errors.New("spec not found")
		}
		changeset, changesetFound := r.changesetsByID[m.ChangesetID]
		repo, repoFound := r.accessibleReposByID[m.RepoID]
		if !repoFound {
			return &db.RepoNotFoundErr{ID: m.RepoID}
		}

		if err := checkRepoSupported(repo); err != nil {
			return err
		}

		if spec.Spec.IsImportingExisting() {
			if m.ChangesetID != 0 {
				// TODO: We don't fetch imported changesets.
				changeset, err := r.tx.GetChangeset(ctx, GetChangesetOpts{ID: m.ChangesetID})
				if err != nil {
					return err
				}

				// If it's already attached to the campaign and errored, we re-enqueue it.
				if changeset.ReconcilerState == campaigns.ReconcilerStateErrored {
					if err := r.updateAndReenqueue(ctx, changeset); err != nil {
						return err
					}
				}
				changeset.CampaignIDs = append(changeset.CampaignIDs, r.campaign.ID)
				if err := r.tx.UpdateChangeset(ctx, changeset); err != nil {
					return err
				}
				attachedChangesets[changeset.ID] = true
			} else {
				c, err := r.updateOrCreateTrackingChangeset(ctx, repo, spec.Spec.ExternalID)
				if err != nil {
					return err
				}
				attachedChangesets[c.ID] = true
			}
		} else if spec.Spec.IsBranch() {
			if m.ChangesetID == 0 {
				c, err := r.createChangesetForSpec(ctx, repo, spec)
				if err != nil {
					return err
				}
				attachedChangesets[c.ID] = true
			} else {
				if !changesetFound {
					return errors.New("changeset not found")
				}
				if err = r.updateChangesetToNewSpec(ctx, changeset, spec); err != nil {
					return err
				}
				attachedChangesets[changeset.ID] = true
			}
		}
	}

	// We went through all the new changeset specs and either created or
	// updated a changeset.
	// Their IDs are all the IDs of changesets that should be in the campaign:
	r.campaign.ChangesetIDs = []int64{}
	for changesetID := range attachedChangesets {
		r.campaign.ChangesetIDs = append(r.campaign.ChangesetIDs, changesetID)
	}

	// But it's possible that changesets are now detached, like Changeset 3 in
	// the example above.
	// This we need to detach and close.
	for _, c := range r.changesets {
		if _, ok := attachedChangesets[c.ID]; ok {
			continue
		}

		// If we don't have access to a repository, we don't detach nor close the changeset.
		_, ok := r.accessibleReposByID[c.RepoID]
		if !ok {
			continue
		}

		if c.CurrentSpecID != 0 && c.OwnedByCampaignID == r.campaign.ID {
			// If we have a current spec ID and the changeset was created by
			// _this_ campaign that means we should detach and close it.

			// But only if it was created on the code host:
			if c.Published() {
				c.Closing = true
				c.ReconcilerState = campaigns.ReconcilerStateQueued
			} else {
				// otherwise we simply delete it.
				if err = r.tx.DeleteChangeset(ctx, c.ID); err != nil {
					return err
				}
				continue
			}
		}

		c.RemoveCampaignID(r.campaign.ID)
		if err = r.tx.UpdateChangeset(ctx, c); err != nil {
			return err
		}
	}

	return r.tx.UpdateCampaign(ctx, r.campaign)
}

func (r *changesetRewirer) createChangesetForSpec(ctx context.Context, repo *types.Repo, spec *campaigns.ChangesetSpec) (*campaigns.Changeset, error) {
	newChangeset := &campaigns.Changeset{
		RepoID:              spec.RepoID,
		ExternalServiceType: repo.ExternalRepo.ServiceType,

		CampaignIDs:       []int64{r.campaign.ID},
		OwnedByCampaignID: r.campaign.ID,
		CurrentSpecID:     spec.ID,

		PublicationState: campaigns.ChangesetPublicationStateUnpublished,
		ReconcilerState:  campaigns.ReconcilerStateQueued,
	}

	// Copy over diff stat from the spec.
	diffStat := spec.DiffStat()
	newChangeset.SetDiffStat(&diffStat)

	return newChangeset, r.tx.CreateChangeset(ctx, newChangeset)
}

func (r *changesetRewirer) updateChangesetToNewSpec(ctx context.Context, c *campaigns.Changeset, spec *campaigns.ChangesetSpec) error {
	c.PreviousSpecID = c.CurrentSpecID
	c.CurrentSpecID = spec.ID

	// Ensure that the changeset is attached to the campaign
	c.CampaignIDs = append(c.CampaignIDs, r.campaign.ID)

	// Copy over diff stat from the new spec.
	diffStat := spec.DiffStat()
	c.SetDiffStat(&diffStat)

	// We need to enqueue it for the changeset reconciler, so the
	// reconciler wakes up, compares old and new spec and, if
	// necessary, updates the changesets accordingly.
	return r.updateAndReenqueue(ctx, c)
}

// loadAssociations populates the chagnesets, newChangesetSpecs and
// accessibleReposByID on changesetRewirer.
func (r *changesetRewirer) loadAssociations(ctx context.Context) (err error) {
	// Load all of the new ChangesetSpecs
	r.newChangesetSpecs, _, err = r.tx.ListChangesetSpecs(ctx, ListChangesetSpecsOpts{
		CampaignSpecID: r.campaign.CampaignSpecID,
	})
	if err != nil {
		return err
	}

	// Load all Changesets attached to this campaign, or owned by this campaign but detached.
	r.changesets, err = r.tx.ListChangesetsAttachedOrOwnedByCampaign(ctx, r.campaign.ID)
	if err != nil {
		return err
	}

	repoIDs := append(r.newChangesetSpecs.RepoIDs(), r.changesets.RepoIDs()...)
	// 🚨 SECURITY: db.Repos.GetRepoIDsSet uses the authzFilter under the hood and
	// filters out repositories that the user doesn't have access to.
	r.accessibleReposByID, err = db.Repos.GetReposSetByIDs(ctx, repoIDs...)
	return err
}

func (r *changesetRewirer) indexAssociations(ctx context.Context) (err error) {
	r.changesetsByID = map[int64]*campaigns.Changeset{}
	r.changesetSpecsByID = map[int64]*campaigns.ChangesetSpec{}

	for _, c := range r.changesets {
		r.changesetsByID[c.ID] = c
	}
	for _, c := range r.newChangesetSpecs {
		r.changesetSpecsByID[c.ID] = c
	}

	return nil
}

func (r *changesetRewirer) updateOrCreateTrackingChangeset(ctx context.Context, repo *types.Repo, externalID string) (*campaigns.Changeset, error) {
	existing, err := r.tx.GetChangeset(ctx, GetChangesetOpts{
		RepoID:              repo.ID,
		ExternalID:          externalID,
		ExternalServiceType: repo.ExternalRepo.ServiceType,
	})
	if err != nil && err != ErrNoResults {
		return nil, err
	}

	if existing != nil {
		// We already have a changeset with the given repoID and
		// externalID, so we can track it.
		existing.AddedToCampaign = true
		existing.CampaignIDs = append(existing.CampaignIDs, r.campaign.ID)

		// If it errored, we re-enqueue it.
		if existing.ReconcilerState == campaigns.ReconcilerStateErrored {
			return existing, r.updateAndReenqueue(ctx, existing)
		}

		return existing, r.tx.UpdateChangeset(ctx, existing)
	}

	newChangeset := &campaigns.Changeset{
		RepoID:              repo.ID,
		ExternalServiceType: repo.ExternalRepo.ServiceType,

		CampaignIDs:     []int64{r.campaign.ID},
		ExternalID:      externalID,
		AddedToCampaign: true,
		// Note: no CurrentSpecID, because we merely track this one

		PublicationState: campaigns.ChangesetPublicationStatePublished,

		// Enqueue it so the reconciler syncs it.
		ReconcilerState: campaigns.ReconcilerStateQueued,
		Unsynced:        true,
	}

	return newChangeset, r.tx.CreateChangeset(ctx, newChangeset)
}

func (r *changesetRewirer) updateAndReenqueue(ctx context.Context, ch *campaigns.Changeset) error {
	ch.ResetQueued()
	return r.tx.UpdateChangeset(ctx, ch)
}
