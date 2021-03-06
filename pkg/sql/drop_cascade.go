// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/resolver"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/typedesc"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

type dropCascadeState struct {
	schemasToDelete []*catalog.ResolvedSchema

	objectNamesToDelete []tree.ObjectName

	td                      []toDelete
	allTableObjectsToDelete []*tabledesc.Mutable
	typesToDelete           []*typedesc.Mutable

	droppedNames []string
}

func newDropCascadeState() *dropCascadeState {
	return &dropCascadeState{
		// We ensure droppedNames is not nil when creating the dropCascadeState.
		// This makes it so that data in the event log is at least an empty list,
		// not NULL.
		droppedNames: []string{},
	}
}

func (d *dropCascadeState) collectObjectsInSchema(
	ctx context.Context, p *planner, db catalog.DatabaseDescriptor, schema *catalog.ResolvedSchema,
) error {
	names, err := resolver.GetObjectNames(ctx, p.txn, p, p.ExecCfg().Codec, db, schema.Name, true /* explicitPrefix */)
	if err != nil {
		return err
	}
	for i := range names {
		d.objectNamesToDelete = append(d.objectNamesToDelete, &names[i])
	}
	d.schemasToDelete = append(d.schemasToDelete, schema)
	return nil
}

func (d *dropCascadeState) resolveCollectedObjects(
	ctx context.Context, p *planner, db catalog.DatabaseDescriptor,
) error {
	d.td = make([]toDelete, 0, len(d.objectNamesToDelete))
	// Resolve each of the collected names.
	for i := range d.objectNamesToDelete {
		objName := d.objectNamesToDelete[i]
		// First try looking up objName as a table.
		found, desc, err := p.LookupObject(
			ctx,
			tree.ObjectLookupFlags{
				// Note we set required to be false here in order to not error out
				// if we don't find the object.
				CommonLookupFlags: tree.CommonLookupFlags{
					Required:       false,
					RequireMutable: true,
					IncludeOffline: true,
				},
				DesiredObjectKind: tree.TableObject,
			},
			objName.Catalog(),
			objName.Schema(),
			objName.Object(),
		)
		if err != nil {
			return err
		}
		if found {
			tbDesc, ok := desc.(*tabledesc.Mutable)
			if !ok {
				return errors.AssertionFailedf(
					"descriptor for %q is not Mutable",
					objName.Object(),
				)
			}
			if tbDesc.State == descpb.DescriptorState_OFFLINE {
				dbName := db.GetName()
				return pgerror.Newf(pgcode.ObjectNotInPrerequisiteState,
					"cannot drop a database with OFFLINE tables, ensure %s is"+
						" dropped or made public before dropping database %s",
					objName.FQString(), tree.AsString((*tree.Name)(&dbName)))
			}
			if err := p.canDropTable(ctx, tbDesc); err != nil {
				return err
			}
			// Recursively check permissions on all dependent views, since some may
			// be in different databases.
			for _, ref := range tbDesc.DependedOnBy {
				if err := p.canRemoveDependentView(ctx, tbDesc, ref, tree.DropCascade); err != nil {
					return err
				}
			}
			d.td = append(d.td, toDelete{objName, tbDesc})
		} else {
			// If we couldn't resolve objName as a table, try a type.
			found, desc, err := p.LookupObject(
				ctx,
				tree.ObjectLookupFlags{
					CommonLookupFlags: tree.CommonLookupFlags{
						Required:       true,
						RequireMutable: true,
						IncludeOffline: true,
					},
					DesiredObjectKind: tree.TypeObject,
				},
				objName.Catalog(),
				objName.Schema(),
				objName.Object(),
			)
			if err != nil {
				return err
			}
			// If we couldn't find the object at all, then continue.
			if !found {
				continue
			}
			typDesc, ok := desc.(*typedesc.Mutable)
			if !ok {
				return errors.AssertionFailedf(
					"descriptor for %q is not Mutable",
					objName.Object(),
				)
			}
			// Types can only depend on objects within this database, so we don't
			// need to do any more verification about whether or not we can drop
			// this type.
			d.typesToDelete = append(d.typesToDelete, typDesc)
		}
	}

	allObjectsToDelete, implicitDeleteMap, err := p.accumulateAllObjectsToDelete(ctx, d.td)
	if err != nil {
		return err
	}
	d.allTableObjectsToDelete = allObjectsToDelete
	d.td = filterImplicitlyDeletedObjects(d.td, implicitDeleteMap)
	return nil
}

func (d *dropCascadeState) dropAllCollectedObjects(ctx context.Context, p *planner) error {
	// Delete all of the collected tables.
	for _, toDel := range d.td {
		desc := toDel.desc
		var cascadedObjects []string
		var err error
		if desc.IsView() {
			// TODO(knz): The names of dependent dropped views should be qualified here.
			cascadedObjects, err = p.dropViewImpl(ctx, desc, false /* queueJob */, "", tree.DropCascade)
		} else if desc.IsSequence() {
			err = p.dropSequenceImpl(ctx, desc, false /* queueJob */, "", tree.DropCascade)
		} else {
			// TODO(knz): The names of dependent dropped tables should be qualified here.
			cascadedObjects, err = p.dropTableImpl(ctx, desc, true /* droppingParent */, "")
		}
		if err != nil {
			return err
		}
		d.droppedNames = append(d.droppedNames, cascadedObjects...)
		d.droppedNames = append(d.droppedNames, toDel.tn.FQString())
	}

	// Now delete all of the types.
	for _, typ := range d.typesToDelete {
		// Drop the types. Note that we set queueJob to be false because the types
		// will be dropped in bulk as part of the DROP DATABASE job.
		if err := p.dropTypeImpl(ctx, typ, "", false /* queueJob */); err != nil {
			return err
		}
	}

	return nil
}

func (d *dropCascadeState) getDroppedTableDetails() []jobspb.DroppedTableDetails {
	res := make([]jobspb.DroppedTableDetails, len(d.allTableObjectsToDelete))
	for i := range d.allTableObjectsToDelete {
		tbl := d.allTableObjectsToDelete[i]
		res[i] = jobspb.DroppedTableDetails{
			ID:   tbl.ID,
			Name: tbl.Name,
		}
	}
	return res
}
