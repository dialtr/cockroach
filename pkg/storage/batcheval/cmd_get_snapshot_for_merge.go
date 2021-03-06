// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package batcheval

import (
	"bytes"
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/storage/rditer"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval/result"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
)

func init() {
	RegisterCommand(roachpb.GetSnapshotForMerge, declareKeysGetSnapshotForMerge, GetSnapshotForMerge)
}

func declareKeysGetSnapshotForMerge(
	desc roachpb.RangeDescriptor, header roachpb.Header, req roachpb.Request, spans *spanset.SpanSet,
) {
	// GetSnapshotForMerge must not run concurrently with any other command. It
	// declares that it reads and writes every addressable key in the range; this
	// guarantees that it conflicts with any other command because every command
	// must declare at least one addressable key. It does not, in fact, write any
	// keys.
	spans.Add(spanset.SpanReadWrite, roachpb.Span{
		Key:    desc.StartKey.AsRawKey(),
		EndKey: desc.EndKey.AsRawKey(),
	})
	spans.Add(spanset.SpanReadWrite, roachpb.Span{
		Key:    keys.MakeRangeKeyPrefix(desc.StartKey),
		EndKey: keys.MakeRangeKeyPrefix(desc.EndKey).PrefixEnd(),
	})
	rangeIDPrefix := keys.MakeRangeIDReplicatedPrefix(desc.RangeID)
	spans.Add(spanset.SpanReadWrite, roachpb.Span{
		Key:    rangeIDPrefix,
		EndKey: rangeIDPrefix.PrefixEnd(),
	})
}

// GetSnapshotForMerge notifies a range that its left-hand neighbor has
// initiated a merge and needs a snapshot of its data. When called correctly, it
// provides important guarantees that ensure there is no moment in time where
// the ranges involved in the merge could both process commands for the same
// keys.
//
// Specifically, the receiving replica guarantees that:
//
//   1. it is the leaseholder at the time the snapshot is taken,
//   2. when it responds, there are no commands in flight,
//   3. the snapshot in the response has the latest writes,
//   4. it, and all future leaseholders for the range, will not process another
//      command until they refresh their range descriptor with a consistent read
//      from meta2, and
//   5. if it or any future leaseholder for the range finds that its range
//      descriptor has been deleted, it self destructs.
//
// To achieve guarantees four and five, when issuing a GetSnapshotForMerge
// request, the caller must have a merge transaction open that has already
// placed deletion intents on both the local and meta2 copy of the right-hand
// range descriptor. The intent on the meta2 allows the leaseholder to block
// until the merge transaction completes by performing a consistent read for its
// meta2 descriptor. The intent on the local descriptor allows future
// leaseholders to efficiently check whether a merge is in progress by
// performing a read of its local descriptor after acquiring the lease.
//
// The period of time after intents have been placed but before the merge
// transaction is complete is called the merge's "critical phase".
func GetSnapshotForMerge(
	ctx context.Context, batch engine.ReadWriter, cArgs CommandArgs, resp roachpb.Response,
) (result.Result, error) {
	args := cArgs.Args.(*roachpb.GetSnapshotForMergeRequest)
	reply := resp.(*roachpb.GetSnapshotForMergeResponse)
	desc := cArgs.EvalCtx.Desc()

	// Sanity check that the requesting range is our left neighbor. The ordering
	// of operations in the AdminMerge transaction should make it impossible for
	// these ranges to be nonadjacent, but double check.
	if !bytes.Equal(args.LeftRange.EndKey, desc.StartKey) {
		return result.Result{}, errors.Errorf("ranges are not adjacent: %s != %s",
			args.LeftRange.EndKey, desc.StartKey)
	}

	// Sanity check the caller has initiated a merge transaction by checking for
	// a deletion intent on the local range descriptor.
	descKey := keys.RangeDescriptorKey(desc.StartKey)
	_, intents, err := engine.MVCCGet(ctx, batch, descKey, cArgs.Header.Timestamp,
		false /* consistent */, nil /* txn */)
	if err != nil {
		return result.Result{}, fmt.Errorf("fetching local range descriptor: %s", err)
	} else if len(intents) == 0 {
		return result.Result{}, errors.New("range missing intent on its local descriptor")
	} else if len(intents) > 1 {
		log.Fatalf(ctx, "MVCCGet returned an impossible number of intents (%d)", len(intents))
	}
	val, _, err := engine.MVCCGetAsTxn(ctx, batch, descKey, cArgs.Header.Timestamp, intents[0].Txn)
	if err != nil {
		return result.Result{}, fmt.Errorf("fetching local range descriptor as txn: %s", err)
	} else if val != nil {
		return result.Result{}, errors.New("non-deletion intent on local range descriptor")
	}

	// NOTE: the deletion intent on the range's meta2 descriptor is just as
	// important to correctness as the deletion intent on the local descriptor,
	// but the check is too expensive as it would involve a network roundtrip on
	// most nodes.

	eng := engine.NewInMem(roachpb.Attributes{}, 1<<20)
	defer eng.Close()

	// TODO(benesch): This command reads the whole replica into memory. We'll need
	// to be more careful when merging large ranges.
	snapBatch := eng.NewBatch()
	defer snapBatch.Close()

	iter := rditer.NewReplicaDataIterator(desc, batch, true /* replicatedOnly */)
	defer iter.Close()
	for ; ; iter.Next() {
		if ok, err := iter.Valid(); err != nil {
			return result.Result{}, err
		} else if !ok {
			break
		}
		if err := snapBatch.Put(iter.Key(), iter.Value()); err != nil {
			return result.Result{}, err
		}
	}
	reply.Data = snapBatch.Repr()

	return result.Result{
		Local: result.LocalResult{SetMerging: true},
	}, nil
}
