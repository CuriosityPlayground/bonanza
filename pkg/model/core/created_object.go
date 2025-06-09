package core

import (
	"context"

	"github.com/buildbarn/bonanza/pkg/storage/dag"
	"github.com/buildbarn/bonanza/pkg/storage/object"
)

// CreatedObject holds the contents of an object that was created using
// ReferenceMessagePatcher. It also holds the metadata that was provided
// to ReferenceMessagePatcher.AddReference(), provided in the same order
// as the outgoing references of the created object.
type CreatedObject[TMetadata any] struct {
	*object.Contents
	Metadata []TMetadata
}

// CreatedObjectTree is CreatedObject applied recursively. Namely, it
// can hold the contents of a tree of objects that were created using
// ReferenceMessagePatcher in memory.
type CreatedObjectTree CreatedObject[CreatedObjectTree]

var (
	_ CloneableReferenceMetadata = CreatedObjectTree{}
	_ WalkableReferenceMetadata  = CreatedObjectTree{}
	_ object.BasicReference      = CreatedObjectTree{}
)

// Discard any resources owned by the CreatedObjectTree.
func (CreatedObjectTree) Discard() {}

// IsCloneable indicates that instances of CreatedObjectTree may safely
// be placed in multiple ReferenceMessagePatchers.
func (CreatedObjectTree) IsCloneable() {}

// ToObjectContentsWalker returns a ObjectContentsWalker that allows
// traversing all objects contained in the CreatedObjectTree.
func (t CreatedObjectTree) ToObjectContentsWalker() dag.ObjectContentsWalker {
	return &createdObjectTreeWalker{
		tree: &t,
	}
}

type createdObjectTreeWalker struct {
	tree *CreatedObjectTree
}

func (tw *createdObjectTreeWalker) GetContents(ctx context.Context) (*object.Contents, []dag.ObjectContentsWalker, error) {
	contents := tw.tree.Contents
	metadata := tw.tree.Metadata
	children := make([]dag.ObjectContentsWalker, 0, len(metadata))
	for i := range metadata {
		children = append(children, &createdObjectTreeWalker{
			tree: &metadata[i],
		})
	}

	tw.tree = nil
	return contents, children, nil
}

func (tw *createdObjectTreeWalker) Discard() {
	tw.tree = nil
}
