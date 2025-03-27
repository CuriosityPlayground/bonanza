package starlark

import (
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"slices"

	pg_label "github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	"github.com/buildbarn/bonanza/pkg/model/core/btree"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type Depset[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata] struct {
	children any
	order    model_starlark_pb.Depset_Order
	hash     uint32
}

var (
	_ EncodableValue[object.LocalReference, model_core.CloneableReferenceMetadata] = (*Depset[object.LocalReference, model_core.CloneableReferenceMetadata])(nil)
	_ starlark.Comparable                                                          = (*Depset[object.LocalReference, model_core.CloneableReferenceMetadata])(nil)
	_ starlark.HasAttrs                                                            = (*Depset[object.LocalReference, model_core.CloneableReferenceMetadata])(nil)
)

func deduplicateAndAddDirect(thread *starlark.Thread, children *[]any, direct iter.Seq2[int, starlark.Value], valuesSeen *valueSet) error {
	for _, v := range direct {
		if alreadySeen, err := valuesSeen.testAndAdd(thread, v); err != nil {
			return err
		} else if !alreadySeen {
			*children = append(*children, v)
		}
	}
	return nil
}

func deduplicateAndAddTransitive[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata](thread *starlark.Thread, children *[]any, transitive iter.Seq2[int, *Depset[TReference, TMetadata]], valuesSeen *valueSet, encodedListsSeen map[object.LocalReference]struct{}, depsetsSeen map[*any]struct{}, order model_starlark_pb.Depset_Order) error {
	for _, d := range transitive {
		switch v := d.children.(type) {
		case nil:
			// Empty child. Ignore it.
		case starlark.Value:
			// Single child that is decoded. Add it directly.
			if alreadySeen, err := valuesSeen.testAndAdd(thread, v); err != nil {
				return err
			} else if !alreadySeen {
				*children = append(*children, v)
			}
		case model_core.Message[*model_starlark_pb.List_Element, TReference]:
			switch level := v.Message.Level.(type) {
			case *model_starlark_pb.List_Element_Leaf:
				// Encoded child.
				// TODO: Do we want to deduplicate these
				// as well?
				*children = append(*children, v)
			case *model_starlark_pb.List_Element_Parent_:
				// Multiple encoded children. Deduplicate
				// them by list object reference.
				listReference, err := model_core.FlattenReference(model_core.Nested(v, level.Parent.Reference))
				if err != nil {
					return err
				}
				localReference := listReference.GetLocalReference()
				if _, ok := encodedListsSeen[localReference]; !ok {
					*children = append(*children, v)
					encodedListsSeen[localReference] = struct{}{}
				}
			default:
				return errors.New("not a valid list element")
			}
		case []any:
			// Multiple children. Reference it.
			if order != d.order && order != model_starlark_pb.Depset_DEFAULT && d.order != model_starlark_pb.Depset_DEFAULT {
				return errors.New("depsets have incompatible orders")
			}
			if _, ok := depsetsSeen[&v[0]]; !ok {
				*children = append(*children, v)
				depsetsSeen[&v[0]] = struct{}{}
			}
		}
	}
	return nil
}

func NewDepset[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata](thread *starlark.Thread, direct []starlark.Value, transitive []*Depset[TReference, TMetadata], order model_starlark_pb.Depset_Order) (*Depset[TReference, TMetadata], error) {
	var directIter iter.Seq2[int, starlark.Value]
	var transitiveIter iter.Seq2[int, *Depset[TReference, TMetadata]]
	preorder := false
	switch order {
	case model_starlark_pb.Depset_DEFAULT, model_starlark_pb.Depset_POSTORDER:
		directIter = slices.All(direct)
		transitiveIter = slices.All(transitive)
	case model_starlark_pb.Depset_PREORDER:
		directIter = slices.All(direct)
		transitiveIter = slices.All(transitive)
		preorder = true
	case model_starlark_pb.Depset_TOPOLOGICAL:
		// Insert elements in reverse order, because
		// deduplication needs to happen from the back to the
		// front.
		directIter = slices.Backward(direct)
		transitiveIter = slices.Backward(transitive)
	default:
		return nil, errors.New("unknown order")
	}

	var valuesSeen valueSet
	encodedListsSeen := map[object.LocalReference]struct{}{}
	depsetsSeen := map[*any]struct{}{}
	children := make([]any, 0, len(direct)+len(transitive))
	if preorder {
		if err := deduplicateAndAddDirect(thread, &children, directIter, &valuesSeen); err != nil {
			return nil, err
		}
		if err := deduplicateAndAddTransitive(thread, &children, transitiveIter, &valuesSeen, encodedListsSeen, depsetsSeen, order); err != nil {
			return nil, err
		}
	} else {
		if err := deduplicateAndAddTransitive(thread, &children, transitiveIter, &valuesSeen, encodedListsSeen, depsetsSeen, order); err != nil {
			return nil, err
		}
		if err := deduplicateAndAddDirect(thread, &children, directIter, &valuesSeen); err != nil {
			return nil, err
		}
	}

	return NewDepsetFromList[TReference, TMetadata](children, order), nil
}

func NewDepsetFromList[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata](children []any, order model_starlark_pb.Depset_Order) *Depset[TReference, TMetadata] {
	// As depsets only provide reference equality, give each
	// instance a random hash.
	switch len(children) {
	case 0:
		return &Depset[TReference, TMetadata]{}
	case 1:
		return &Depset[TReference, TMetadata]{
			children: children[0],
			hash:     rand.Uint32(),
		}
	default:
		return &Depset[TReference, TMetadata]{
			children: children,
			order:    order,
			hash:     rand.Uint32(),
		}
	}
}

func (Depset[TReference, TMetadata]) String() string {
	return "<depset>"
}

func (Depset[TReference, TMetadata]) Type() string {
	return "depset"
}

func (Depset[TReference, TMetadata]) Freeze() {}

func (Depset[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (d *Depset[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return d.hash, nil
}

func (d *Depset[TReference, TMetadata]) CompareSameType(thread *starlark.Thread, op syntax.Token, other starlark.Value, depth int) (bool, error) {
	switch op {
	case syntax.EQL:
		return d == other.(*Depset[TReference, TMetadata]), nil
	case syntax.NEQ:
		return d != other.(*Depset[TReference, TMetadata]), nil
	default:
		return false, errors.New("depsets cannot be compared for inequality")
	}
}

type depsetChildrenEncoder[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata] struct {
	path        map[starlark.Value]struct{}
	options     *ValueEncodingOptions[TReference, TMetadata]
	treeBuilder btree.Builder[*model_starlark_pb.List_Element, TMetadata]
	needsCode   bool
}

func (e *depsetChildrenEncoder[TReference, TMetadata]) encode(children any) error {
	switch v := children.(type) {
	case nil:
	case starlark.Value:
		encodedValue, valueNeedsCode, err := EncodeValue[TReference, TMetadata](v, e.path, nil, e.options)
		if err != nil {
			return err
		}
		e.needsCode = e.needsCode || valueNeedsCode
		// TODO: Should we also deduplicate elements at this point?
		if err := e.treeBuilder.PushChild(
			model_core.NewPatchedMessage(
				&model_starlark_pb.List_Element{
					Level: &model_starlark_pb.List_Element_Leaf{
						Leaf: encodedValue.Message,
					},
				},
				encodedValue.Patcher,
			),
		); err != nil {
			return err
		}
	case model_core.Message[*model_starlark_pb.List_Element, TReference]:
		if err := e.treeBuilder.PushChild(
			model_core.Patch(e.options.ObjectCapturer, v),
		); err != nil {
			return err
		}
	case []any:
		for _, child := range v {
			if err := e.encode(child); err != nil {
				return err
			}
		}
	default:
		panic("unexpected element type")
	}
	return nil
}

func (d *Depset[TReference, TMetadata]) Encode(path map[starlark.Value]struct{}, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Depset, TMetadata], bool, error) {
	treeBuilder := newSplitBTreeBuilder(
		options,
		/* parentNodeComputer = */ func(createdObject model_core.CreatedObject[TMetadata], childNodes []*model_starlark_pb.List_Element) (model_core.PatchedMessage[*model_starlark_pb.List_Element, TMetadata], error) {
			patcher := model_core.NewReferenceMessagePatcher[TMetadata]()
			return model_core.NewPatchedMessage(
				&model_starlark_pb.List_Element{
					Level: &model_starlark_pb.List_Element_Parent_{
						Parent: &model_starlark_pb.List_Element_Parent{
							Reference: patcher.AddReference(
								createdObject.Contents.GetReference(),
								options.ObjectCapturer.CaptureCreatedObject(createdObject),
							),
						},
					},
				},
				patcher,
			), nil
		},
	)

	e := depsetChildrenEncoder[TReference, TMetadata]{
		path:        path,
		options:     options,
		treeBuilder: treeBuilder,
	}
	if err := e.encode(d.children); err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Depset, TMetadata]{}, false, err
	}

	elements, err := treeBuilder.FinalizeList()
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Depset, TMetadata]{}, false, err
	}

	return model_core.NewPatchedMessage(
		&model_starlark_pb.Depset{
			Elements: elements.Message,
			Order:    d.order,
		},
		elements.Patcher,
	), e.needsCode, nil
}

func (d *Depset[TReference, TMetadata]) EncodeValue(path map[starlark.Value]struct{}, currentIdentifier *pg_label.CanonicalStarlarkIdentifier, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata], bool, error) {
	encodedDepset, needsCode, err := d.Encode(path, options)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata]{}, false, err
	}
	return model_core.NewPatchedMessage(
		&model_starlark_pb.Value{
			Kind: &model_starlark_pb.Value_Depset{
				Depset: encodedDepset.Message,
			},
		},
		encodedDepset.Patcher,
	), needsCode, nil
}

func (d *Depset[TReference, TMetadata]) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	switch name {
	case "to_list":
		return starlark.NewBuiltin("depset.to_list", d.doToList), nil
	default:
		return nil, nil
	}
}

var depsetAttrNames = []string{
	"to_list",
}

func (d *Depset[TReference, TMetadata]) AttrNames() []string {
	return depsetAttrNames
}

type depsetToListConverter[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata] struct {
	thread *starlark.Thread

	valueDecodingOptions *ValueDecodingOptions[TReference]

	list             []starlark.Value
	valuesSeen       valueSet
	encodedListsSeen map[object.LocalReference]struct{}
	depsetsSeen      map[*any]struct{}
}

func (dlc *depsetToListConverter[TReference, TMetadata]) appendChildren(children any) error {
	switch v := children.(type) {
	case starlark.Value:
		if alreadySeen, err := dlc.valuesSeen.testAndAdd(dlc.thread, v); err != nil {
			return err
		} else if !alreadySeen {
			dlc.list = append(dlc.list, v)
		}
	case model_core.Message[*model_starlark_pb.List_Element, TReference]:
		if dlc.valueDecodingOptions == nil {
			valueDecodingOptionsValue := dlc.thread.Local(ValueDecodingOptionsKey)
			if valueDecodingOptionsValue == nil {
				return errors.New("depsets with encoded elements cannot be decoded from within this context")
			}
			dlc.valueDecodingOptions = valueDecodingOptionsValue.(*ValueDecodingOptions[TReference])
		}

		var errIter error
		for encodedElement := range AllListLeafElementsSkippingDuplicateParents(
			dlc.valueDecodingOptions.Context,
			dlc.valueDecodingOptions.Readers.List,
			model_core.Nested(v, []*model_starlark_pb.List_Element{v.Message}),
			dlc.encodedListsSeen,
			&errIter,
		) {
			decodedElement, err := DecodeValue[TReference, TMetadata](encodedElement, nil, dlc.valueDecodingOptions)
			if err != nil {
				return err
			}
			if alreadySeen, err := dlc.valuesSeen.testAndAdd(dlc.thread, decodedElement); err != nil {
				return err
			} else if !alreadySeen {
				dlc.list = append(dlc.list, decodedElement)
			}
		}
		if errIter != nil {
			return fmt.Errorf("failed to iterate depset elements: %w", errIter)
		}
	case []any:
		if _, ok := dlc.depsetsSeen[&v[0]]; !ok {
			for _, child := range v {
				if err := dlc.appendChildren(child); err != nil {
					return err
				}
			}
			dlc.depsetsSeen[&v[0]] = struct{}{}
		}
	default:
		panic("unexpected element type")
	}
	return nil
}

func (d *Depset[TReference, TMetadata]) ToList(thread *starlark.Thread) (*starlark.List, error) {
	dlc := depsetToListConverter[TReference, TMetadata]{
		thread:           thread,
		encodedListsSeen: map[object.LocalReference]struct{}{},
		depsetsSeen:      map[*any]struct{}{},
	}
	if d.children != nil {
		if err := dlc.appendChildren(d.children); err != nil {
			return nil, err
		}
		if d.order == model_starlark_pb.Depset_TOPOLOGICAL {
			// Undo reversal caused by insertion in opposite
			// direction.
			slices.Reverse(dlc.list)
		}
	}
	l := starlark.NewList(dlc.list)
	l.Freeze()
	return l, nil
}

func (d *Depset[TReference, TMetadata]) doToList(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if err := starlark.UnpackArgs(b.Name(), args, kwargs); err != nil {
		return nil, err
	}
	return d.ToList(thread)
}

// valueSet is a simple set type for starlark.Value. It's not possible
// to use map[starlark.Value]struct{} for this purpose, as this does not
// imply equality at the Starlark level. For this starlark.Equal() needs
// to be called.
type valueSet struct {
	hashes      []uint32
	values      []starlark.Value
	utilization int
}

func (vs *valueSet) testAndAdd(thread *starlark.Thread, v starlark.Value) (bool, error) {
	// Compute hash of the object. Ensure the resulting hash is
	// non-zero, as zero is used by the hash table to indicate an
	// entry is not used.
	hash, err := v.Hash(thread)
	if err != nil {
		return false, err
	}
	if hash == 0 {
		hash = 1
	}

	vs.maybeGrow()

	mask := uint(len(vs.hashes) - 1)
	for h, inc := uint(hash), uint(1); ; h, inc = h+inc, inc+1 {
		index := h & mask
		switch vs.hashes[index] {
		case 0:
			// Value is not yet present.
			vs.hashes[index] = hash
			vs.values[index] = v
			vs.utilization++
			return false, nil
		case hash:
			// Matching hash. Perform deep comparison.
			if equal, err := starlark.Equal(thread, v, vs.values[index]); err != nil {
				return false, err
			} else if equal {
				return true, nil
			}
		}
	}
}

func (vs *valueSet) maybeGrow() {
	if vs.utilization*2 >= len(vs.hashes) {
		// Utilization is 50% or more. Allocate a new hash table
		// that is twice as big.
		newLength := max(64, len(vs.hashes)*2)
		newHashes := make([]uint32, newLength)
		newValues := make([]starlark.Value, newLength)

		// Copy entries from the old hash table to the new one.
		newMask := uint(newLength - 1)
		for oldIndex, hash := range vs.hashes {
			if hash != 0 {
				value := vs.values[oldIndex]
				for h, inc := uint(hash), uint(1); ; h, inc = h+inc, inc+1 {
					newIndex := h & newMask
					if newHashes[newIndex] == 0 {
						newHashes[newIndex] = hash
						newValues[newIndex] = value
						break
					}
				}
			}
		}

		vs.hashes = newHashes
		vs.values = newValues
	}
}
