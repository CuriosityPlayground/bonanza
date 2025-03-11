package starlark

import (
	"errors"
	"fmt"
	go_path "path"

	pg_label "github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
	"github.com/buildbarn/bonanza/pkg/starlark/unpack"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type label[TReference any, TMetadata model_core.CloneableReferenceMetadata] struct {
	value pg_label.ResolvedLabel
}

var (
	_ EncodableValue[object.LocalReference, model_core.CloneableReferenceMetadata] = label[object.LocalReference, model_core.CloneableReferenceMetadata]{}
	_ HasLabels                                                                    = label[object.LocalReference, model_core.CloneableReferenceMetadata]{}
	_ starlark.HasAttrs                                                            = label[object.LocalReference, model_core.CloneableReferenceMetadata]{}
	_ starlark.Value                                                               = label[object.LocalReference, model_core.CloneableReferenceMetadata]{}
)

func NewLabel[TReference any, TMetadata model_core.CloneableReferenceMetadata](value pg_label.ResolvedLabel) starlark.Value {
	return label[TReference, TMetadata]{
		value: value,
	}
}

func (l label[TReference, TMetadata]) String() string {
	return l.value.String()
}

func (l label[TReference, TMetadata]) Type() string {
	return "Label"
}

func (l label[TReference, TMetadata]) Freeze() {}

func (l label[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (l label[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return starlark.String(l.value.String()).Hash(thread)
}

func (l label[TReference, TMetadata]) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	switch name {
	case "name":
		return starlark.String(l.value.GetTargetName().String()), nil
	case "package":
		return starlark.String(l.value.GetPackagePath()), nil
	case "repo_name":
		canonicalLabel, err := l.value.AsCanonical()
		if err != nil {
			return nil, err
		}
		return starlark.String(canonicalLabel.GetCanonicalPackage().GetCanonicalRepo().String()), nil
	case "same_package_label":
		return starlark.NewBuiltin(
			"Label.same_package_label",
			func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var targetName pg_label.TargetName
				if err := starlark.UnpackArgs(
					b.Name(), args, kwargs,
					"target_name", unpack.Bind(thread, &targetName, unpack.TargetName),
				); err != nil {
					return nil, err
				}
				return NewLabel[TReference, TMetadata](l.value.AppendTargetName(targetName)), nil
			},
		), nil
	case "workspace_root":
		canonicalLabel, err := l.value.AsCanonical()
		if err != nil {
			return nil, err
		}
		return starlark.String(go_path.Join(
			externalDirectoryName,
			canonicalLabel.GetCanonicalPackage().GetCanonicalRepo().String(),
		)), nil

	default:
		return nil, nil
	}
}

var labelAttrNames = []string{
	"name",
	"package",
	"repo_name",
	"same_package_label",
	"workspace_root",
}

func (l label[TReference, TMetadata]) AttrNames() []string {
	return labelAttrNames
}

func (l label[TReference, TMetadata]) EncodeValue(path map[starlark.Value]struct{}, currentIdentifier *pg_label.CanonicalStarlarkIdentifier, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata], bool, error) {
	return model_core.NewSimplePatchedMessage[TMetadata](
		&model_starlark_pb.Value{
			Kind: &model_starlark_pb.Value_Label{
				Label: l.value.String(),
			},
		},
	), false, nil
}

func (l label[TReference, TMetadata]) VisitLabels(thread *starlark.Thread, path map[starlark.Value]struct{}, visitor func(pg_label.ResolvedLabel) error) error {
	if err := visitor(l.value); err != nil {
		return fmt.Errorf("label %#v: %w", l.value.String(), err)
	}
	return nil
}

type (
	CanonicalRepoResolver = func(fromCanonicalRepo pg_label.CanonicalRepo, toApparentRepo pg_label.ApparentRepo) (*pg_label.CanonicalRepo, error)
	RootModuleResolver    = func() (pg_label.Module, error)
)

const (
	CanonicalRepoResolverKey = "canonical_repo_resolver"
	RootModuleResolverKey    = "root_module_resolver"
)

type labelOrStringUnpackerInto[TReference any, TMetadata model_core.CloneableReferenceMetadata] struct {
	basePackage pg_label.CanonicalPackage
}

func NewLabelOrStringUnpackerInto[TReference any, TMetadata model_core.CloneableReferenceMetadata](basePackage pg_label.CanonicalPackage) unpack.UnpackerInto[pg_label.ResolvedLabel] {
	return &labelOrStringUnpackerInto[TReference, TMetadata]{
		basePackage: basePackage,
	}
}

func (ui *labelOrStringUnpackerInto[TReference, TMetadata]) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *pg_label.ResolvedLabel) error {
	switch typedV := v.(type) {
	case starlark.String:
		// Label value is a bare string. Parse it.
		apparentLabel, err := ui.basePackage.AppendLabel(string(typedV))
		if err != nil {
			return err
		}
		if canonicalLabel, ok := apparentLabel.AsCanonical(); ok {
			*dst = canonicalLabel.AsResolved()
			return nil
		}

		if apparentRepo, ok := apparentLabel.GetApparentRepo(); ok {
			// Label value has an apparent repo name. Resolve it.
			canonicalRepoResolver := thread.Local(CanonicalRepoResolverKey)
			if canonicalRepoResolver == nil {
				return fmt.Errorf("label has apparent repo %#v, which cannot be resolved to a canonical repo from within this context", apparentRepo.String())
			}
			canonicalRepo, err := canonicalRepoResolver.(CanonicalRepoResolver)(ui.basePackage.GetCanonicalRepo(), apparentRepo)
			if err != nil {
				return fmt.Errorf("failed to resolve apparent repo %#v: %w", apparentRepo.String(), err)
			}
			if canonicalRepo == nil {
				// Repo does not exist. Still permit the
				// label to be constructed, so that
				// analysis of other targets may pass.
				*dst = apparentLabel.AsResolvedWithError(
					fmt.Sprintf(
						"unknown repo '%s' requested from %s",
						apparentRepo.String(),
						ui.basePackage.GetCanonicalRepo().String(),
					),
				)
			} else {
				*dst = apparentLabel.WithCanonicalRepo(*canonicalRepo).AsResolved()
			}
			return nil
		}

		// Label is prefixed with "@@". Resolve to the root module.
		rootModuleResolver := thread.Local(RootModuleResolverKey)
		if rootModuleResolver == nil {
			return errors.New("label is prefixed with \"@@\", which cannot be resolved to the root module from within this context")
		}
		rootModule, err := rootModuleResolver.(RootModuleResolver)()
		if err != nil {
			return fmt.Errorf("failed to resolve root module: %w", err)
		}
		*dst = apparentLabel.
			WithCanonicalRepo(rootModule.ToModuleInstance(nil).GetBareCanonicalRepo()).
			AsResolved()
		return nil
	case label[TReference, TMetadata]:
		// Label value is already wrapped in Label().
		*dst = typedV.value
		return nil
	default:
		return fmt.Errorf("got %s, want Label or str", v.Type())
	}
}

func (ui *labelOrStringUnpackerInto[TReference, TMetadata]) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	var l pg_label.ResolvedLabel
	if err := ui.UnpackInto(thread, v, &l); err != nil {
		return nil, err
	}
	return label[TReference, TMetadata]{value: l}, nil
}

func (labelOrStringUnpackerInto[TReference, TMetadata]) GetConcatenationOperator() syntax.Token {
	return 0
}

type labelUnpackerInto[TReference any, TMetadata model_core.CloneableReferenceMetadata] struct{}

func NewLabelUnpackerInto[TReference any, TMetadata model_core.CloneableReferenceMetadata]() unpack.UnpackerInto[pg_label.ResolvedLabel] {
	return labelUnpackerInto[TReference, TMetadata]{}
}

func (labelUnpackerInto[TReference, TMetadata]) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *pg_label.ResolvedLabel) error {
	l, ok := v.(label[TReference, TMetadata])
	if !ok {
		return fmt.Errorf("got %s, want Label", v.Type())
	}
	*dst = l.value
	return nil
}

func (ui labelUnpackerInto[TReference, TMetadata]) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	var l pg_label.ResolvedLabel
	if err := ui.UnpackInto(thread, v, &l); err != nil {
		return nil, err
	}
	return label[TReference, TMetadata]{value: l}, nil
}

func (labelUnpackerInto[TReference, TMetadata]) GetConcatenationOperator() syntax.Token {
	return 0
}

func CurrentFilePackage(thread *starlark.Thread, depth int) pg_label.CanonicalPackage {
	return pg_label.MustNewCanonicalLabel(thread.CallFrame(depth).Pos.Filename()).GetCanonicalPackage()
}
