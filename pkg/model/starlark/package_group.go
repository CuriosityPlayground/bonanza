package starlark

import (
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"

	pg_label "github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	"github.com/buildbarn/bonanza/pkg/model/core/inlinedtree"
	model_encoding "github.com/buildbarn/bonanza/pkg/model/encoding"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
)

// packageGroupNode is a simplified representation of the
// PackageGroup.Package and PackageGroup.Subpackages messages. It is
// used to construct new trees contained in PackageGroup.
type packageGroupNode struct {
	includePackage     bool
	includeSubpackages bool
	subpackages        map[string]*packageGroupNode
}

func (n *packageGroupNode) getOrCreate(name string) *packageGroupNode {
	if n.includeSubpackages {
		panic("attempted to look up node under node that already includes all children")
	}
	nSub, ok := n.subpackages[name]
	if !ok {
		nSub = &packageGroupNode{
			subpackages: map[string]*packageGroupNode{},
		}
		n.subpackages[name] = nSub
	}
	return nSub
}

// lookupPackage looks up the node that corresponds to a given canonical
// package name.
func (n *packageGroupNode) lookupPackage(canonicalPackage pg_label.CanonicalPackage) *packageGroupNode {
	nWalk := n.getOrCreate(canonicalPackage.GetCanonicalRepo().String())
	packagePath := canonicalPackage.GetPackagePath()
	for {
		if nWalk.includeSubpackages {
			return nil
		}
		if packagePath == "" {
			return nWalk
		}
		if split := strings.IndexByte(packagePath, '/'); split < 0 {
			nWalk = nWalk.getOrCreate(packagePath)
			packagePath = ""
		} else {
			nWalk = nWalk.getOrCreate(packagePath[:split])
			packagePath = packagePath[split+1:]
		}
	}
}

// toProto converts the data contained in a tree of packageGroupNode to
// its Protobuf message counterpart.
func packageGroupNodeToProto[TMetadata model_core.ReferenceMetadata](n *packageGroupNode, encoder model_encoding.BinaryEncoder, inlinedTreeOptions *inlinedtree.Options, objectCapturer model_core.CreatedObjectCapturer[TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.PackageGroup_Subpackages, TMetadata], error) {
	inlineCandidates := make(inlinedtree.CandidateList[*model_starlark_pb.PackageGroup_Subpackages, TMetadata], 0, 2)
	defer inlineCandidates.Discard()

	// Set the IncludeSubpackages field.
	inlineCandidates = append(inlineCandidates, inlinedtree.Candidate[*model_starlark_pb.PackageGroup_Subpackages, TMetadata]{
		ExternalMessage: model_core.NewSimplePatchedMessage[TMetadata](model_core.Marshalable(nil)),
		ParentAppender: func(
			subpackages model_core.PatchedMessage[*model_starlark_pb.PackageGroup_Subpackages, TMetadata],
			externalObject *model_core.Decodable[model_core.CreatedObject[TMetadata]],
		) {
			subpackages.Message.IncludeSubpackages = n.includeSubpackages
		},
	})

	// If one or more subpackages are present, set the overrides field.
	if len(n.subpackages) > 0 {
		overrides := model_starlark_pb.PackageGroup_Subpackages_Overrides{
			Packages: make([]*model_starlark_pb.PackageGroup_Package, 0, len(n.subpackages)),
		}
		patcher := model_core.NewReferenceMessagePatcher[TMetadata]()
		for _, component := range slices.Sorted(maps.Keys(n.subpackages)) {
			nChild := n.subpackages[component]
			subpackages, err := packageGroupNodeToProto[TMetadata](nChild, encoder, inlinedTreeOptions, objectCapturer)
			if err != nil {
				return model_core.PatchedMessage[*model_starlark_pb.PackageGroup_Subpackages, TMetadata]{}, err
			}
			overrides.Packages = append(overrides.Packages, &model_starlark_pb.PackageGroup_Package{
				Component:      component,
				IncludePackage: nChild.includePackage,
				Subpackages:    subpackages.Message,
			})
			patcher.Merge(subpackages.Patcher)
		}

		inlineCandidates = append(inlineCandidates, inlinedtree.Candidate[*model_starlark_pb.PackageGroup_Subpackages, TMetadata]{
			ExternalMessage: model_core.NewPatchedMessage(model_core.NewProtoMarshalable(&overrides), patcher),
			Encoder:         encoder,
			ParentAppender: func(
				subpackages model_core.PatchedMessage[*model_starlark_pb.PackageGroup_Subpackages, TMetadata],
				externalObject *model_core.Decodable[model_core.CreatedObject[TMetadata]],
			) {
				if externalObject == nil {
					subpackages.Message.Overrides = &model_starlark_pb.PackageGroup_Subpackages_OverridesInline{
						OverridesInline: &overrides,
					}
				} else {
					subpackages.Message.Overrides = &model_starlark_pb.PackageGroup_Subpackages_OverridesExternal{
						OverridesExternal: subpackages.Patcher.CaptureAndAddDecodableReference(
							*externalObject,
							objectCapturer,
						),
					}
				}
			},
		})
	}

	return inlinedtree.Build(inlineCandidates, inlinedTreeOptions)
}

// NewPackageGroupFromVisibility generates a PackageGroup message based
// on a sequence of "visibility" labels provided to repo(), package(),
// or rule targets.
func NewPackageGroupFromVisibility[TMetadata model_core.ReferenceMetadata](visibility []pg_label.ResolvedLabel, encoder model_encoding.BinaryEncoder, inlinedTreeOptions *inlinedtree.Options, objectCapturer model_core.CreatedObjectCapturer[TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.PackageGroup, TMetadata], error) {
	tree := packageGroupNode{
		subpackages: map[string]*packageGroupNode{},
	}
	var includePackageGroups []string

	for _, resolvedLabel := range visibility {
		canonicalLabel, err := resolvedLabel.AsCanonical()
		if err != nil {
			// Label points to an invalid repo. For
			// consistency with Bazel, discard the entry.
			//
			// TODO: Maybe we should record that this
			// happened as part of the resulting
			// PackageGroup? That way we can improve
			// visibility related error messages to state
			// that this may be caused by invalid labels.
			continue
		}

		canonicalPackage := canonicalLabel.GetCanonicalPackage()
		packagePath := canonicalPackage.GetPackagePath()
		targetName := canonicalLabel.GetTargetName().String()
		if packagePath == "visibility" {
			// Special labels under //visibility:*.
			switch targetName {
			case "private":
				if len(visibility) > 1 {
					return model_core.PatchedMessage[*model_starlark_pb.PackageGroup, TMetadata]{}, errors.New("//visibility:private may not be combined with other labels")
				}
				return model_core.NewSimplePatchedMessage[TMetadata](&model_starlark_pb.PackageGroup{
					Tree: &model_starlark_pb.PackageGroup_Subpackages{},
				}), nil
			case "public":
				if len(visibility) > 1 {
					return model_core.PatchedMessage[*model_starlark_pb.PackageGroup, TMetadata]{}, errors.New("//visibility:public may not be combined with other labels")
				}
				return model_core.NewSimplePatchedMessage[TMetadata](&model_starlark_pb.PackageGroup{
					Tree: &model_starlark_pb.PackageGroup_Subpackages{
						IncludeSubpackages: true,
					},
				}), nil
			}
		}

		switch targetName {
		case "__pkg__":
			// Include a single package.
			if n := tree.lookupPackage(canonicalPackage); n != nil {
				n.includePackage = true
			}
		case "__subpackages__":
			// Include a package and all of its children.
			if n := tree.lookupPackage(canonicalPackage); n != nil {
				*n = packageGroupNode{
					includePackage:     true,
					includeSubpackages: true,
				}
			}
		default:
			// Reference to another package group that
			// should be merged into this set of packages.
			includePackageGroups = append(includePackageGroups, canonicalLabel.String())
		}
	}

	treeProto, err := packageGroupNodeToProto[TMetadata](&tree, encoder, inlinedTreeOptions, objectCapturer)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.PackageGroup, TMetadata]{}, err
	}

	sort.Strings(includePackageGroups)
	return model_core.NewPatchedMessage(
		&model_starlark_pb.PackageGroup{
			Tree:                 treeProto.Message,
			IncludePackageGroups: slices.Compact(includePackageGroups),
		},
		treeProto.Patcher,
	), nil
}

type CreatedObjectCapturerForTesting = model_core.CreatedObjectCapturer[model_core.CreatedObjectTree]
