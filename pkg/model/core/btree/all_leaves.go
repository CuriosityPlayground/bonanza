package btree

import (
	"context"
	"iter"

	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	model_parser "github.com/buildbarn/bonanza/pkg/model/parser"
	model_core_pb "github.com/buildbarn/bonanza/pkg/proto/model/core"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"google.golang.org/protobuf/proto"
)

// AllLeaves can be used to iterate all leaf entries contained in a B-tree.
func AllLeaves[
	TMessage any,
	TMessagePtr interface {
		*TMessage
		proto.Message
	},
](
	ctx context.Context,
	reader model_parser.ParsedObjectReader[object.LocalReference, model_core.Message[[]TMessagePtr]],
	root model_core.Message[[]TMessagePtr],
	traverser func(model_core.Message[TMessagePtr]) (*model_core_pb.Reference, error),
	errOut *error,
) iter.Seq[model_core.Message[TMessagePtr]] {
	lists := []model_core.Message[[]TMessagePtr]{root}
	return func(yield func(model_core.Message[TMessagePtr]) bool) {
		for len(lists) > 0 {
			lastList := &lists[len(lists)-1]
			if len(lastList.Message) == 0 {
				lists = lists[:len(lists)-1]
			} else {
				entry := lastList.Message[0]
				lastList.Message = lastList.Message[1:]
				if childReference, err := traverser(model_core.NewNestedMessage(*lastList, entry)); err != nil {
					*errOut = err
					return
				} else if childReference == nil {
					// Traverser wants us to yield a leaf.
					if !yield(model_core.NewNestedMessage(*lastList, entry)) {
						*errOut = nil
						return
					}
				} else {
					// Traverser wants us to enter a child.
					objectReference, err := lastList.GetOutgoingReference(childReference)
					if err != nil {
						*errOut = err
						return
					}
					child, _, err := reader.ReadParsedObject(ctx, objectReference)
					if err != nil {
						*errOut = err
						return
					}
					lists = append(lists, child)
				}
			}
		}
	}
}
