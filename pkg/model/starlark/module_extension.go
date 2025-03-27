package starlark

import (
	"errors"
	"maps"
	"slices"
	"strings"

	pg_label "github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"go.starlark.net/starlark"
)

type ModuleExtensionDefinition[TReference any, TMetadata model_core.CloneableReferenceMetadata] interface {
	EncodableValue[TReference, TMetadata]
}

type moduleExtension[TReference any, TMetadata model_core.CloneableReferenceMetadata] struct {
	ModuleExtensionDefinition[TReference, TMetadata]
}

var (
	_ starlark.Value                                                               = (*moduleExtension[object.LocalReference, model_core.CloneableReferenceMetadata])(nil)
	_ EncodableValue[object.LocalReference, model_core.CloneableReferenceMetadata] = (*moduleExtension[object.LocalReference, model_core.CloneableReferenceMetadata])(nil)
)

func NewModuleExtension[TReference any, TMetadata model_core.CloneableReferenceMetadata](definition ModuleExtensionDefinition[TReference, TMetadata]) starlark.Value {
	return &moduleExtension[TReference, TMetadata]{
		ModuleExtensionDefinition: definition,
	}
}

func (me *moduleExtension[TReference, TMetadata]) String() string {
	return "<module_extension>"
}

func (me *moduleExtension[TReference, TMetadata]) Type() string {
	return "module_extension"
}

func (me *moduleExtension[TReference, TMetadata]) Freeze() {}

func (me *moduleExtension[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (me *moduleExtension[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("module_extension cannot be hashed")
}

type starlarkModuleExtensionDefinition[TReference any, TMetadata model_core.CloneableReferenceMetadata] struct {
	implementation NamedFunction[TReference, TMetadata]
	tagClasses     map[pg_label.StarlarkIdentifier]*TagClass[TReference, TMetadata]
}

func NewStarlarkModuleExtensionDefinition[TReference any, TMetadata model_core.CloneableReferenceMetadata](implementation NamedFunction[TReference, TMetadata], tagClasses map[pg_label.StarlarkIdentifier]*TagClass[TReference, TMetadata]) ModuleExtensionDefinition[TReference, TMetadata] {
	return &starlarkModuleExtensionDefinition[TReference, TMetadata]{
		implementation: implementation,
		tagClasses:     tagClasses,
	}
}

func (med *starlarkModuleExtensionDefinition[TReference, TMetadata]) EncodeValue(path map[starlark.Value]struct{}, currentIdentifier *pg_label.CanonicalStarlarkIdentifier, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata], bool, error) {
	implementation, needsCode, err := med.implementation.Encode(path, options)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata]{}, false, err
	}
	patcher := implementation.Patcher

	tagClasses := make([]*model_starlark_pb.ModuleExtension_NamedTagClass, 0, len(med.tagClasses))
	for _, name := range slices.SortedFunc(
		maps.Keys(med.tagClasses),
		func(a, b pg_label.StarlarkIdentifier) int { return strings.Compare(a.String(), b.String()) },
	) {
		encodedTagClass, tagClassNeedsCode, err := med.tagClasses[name].Encode(path, options)
		if err != nil {
			return model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata]{}, false, err
		}
		tagClasses = append(tagClasses, &model_starlark_pb.ModuleExtension_NamedTagClass{
			Name:     name.String(),
			TagClass: encodedTagClass.Message,
		})
		patcher.Merge(encodedTagClass.Patcher)
		needsCode = needsCode || tagClassNeedsCode
	}

	return model_core.NewPatchedMessage(
		&model_starlark_pb.Value{
			Kind: &model_starlark_pb.Value_ModuleExtension{
				ModuleExtension: &model_starlark_pb.ModuleExtension{
					Implementation: implementation.Message,
					TagClasses:     tagClasses,
				},
			},
		},
		patcher,
	), needsCode, nil
}

type protoModuleExtensionDefinition[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata] struct {
	message model_core.Message[*model_starlark_pb.ModuleExtension, TReference]
}

func NewProtoModuleExtensionDefinition[TReference object.BasicReference, TMetadata model_core.CloneableReferenceMetadata](message model_core.Message[*model_starlark_pb.ModuleExtension, TReference]) ModuleExtensionDefinition[TReference, TMetadata] {
	return &protoModuleExtensionDefinition[TReference, TMetadata]{
		message: message,
	}
}

func (med *protoModuleExtensionDefinition[TReference, TMetadata]) EncodeValue(path map[starlark.Value]struct{}, currentIdentifier *pg_label.CanonicalStarlarkIdentifier, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata], bool, error) {
	patchedMessage := model_core.Patch(options.ObjectCapturer, med.message)
	return model_core.NewPatchedMessage(
		&model_starlark_pb.Value{
			Kind: &model_starlark_pb.Value_ModuleExtension{
				ModuleExtension: patchedMessage.Message,
			},
		},
		patchedMessage.Patcher,
	), false, nil
}
