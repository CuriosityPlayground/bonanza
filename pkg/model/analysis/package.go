package analysis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/buildbarn/bonanza/pkg/evaluation"
	"github.com/buildbarn/bonanza/pkg/glob"
	"github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	"github.com/buildbarn/bonanza/pkg/model/core/btree"
	model_filesystem "github.com/buildbarn/bonanza/pkg/model/filesystem"
	model_starlark "github.com/buildbarn/bonanza/pkg/model/starlark"
	model_analysis_pb "github.com/buildbarn/bonanza/pkg/proto/model/analysis"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

var buildDotBazelTargetNames = []label.TargetName{
	label.MustNewTargetName("BUILD.bazel"),
	label.MustNewTargetName("BUILD"),
}

func (c *baseComputer[TReference, TMetadata]) ComputePackageValue(ctx context.Context, key *model_analysis_pb.Package_Key, e PackageEnvironment[TReference, TMetadata]) (PatchedPackageValue, error) {
	canonicalPackage, err := label.NewCanonicalPackage(key.Label)
	if err != nil {
		return PatchedPackageValue{}, fmt.Errorf("invalid package label: %w", err)
	}
	canonicalRepo := canonicalPackage.GetCanonicalRepo()

	allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
	repoDefaultAttrsValue := e.GetRepoDefaultAttrsValue(&model_analysis_pb.RepoDefaultAttrs_Key{
		CanonicalRepo: canonicalRepo.String(),
	})
	fileReader, gotFileReader := e.GetFileReaderValue(&model_analysis_pb.FileReader_Key{})
	if !allBuiltinsModulesNames.IsSet() || !repoDefaultAttrsValue.IsSet() || !gotFileReader {
		return PatchedPackageValue{}, evaluation.ErrMissingDependency
	}

	builtinsModuleNames := allBuiltinsModulesNames.Message.BuiltinsModuleNames
	thread := c.newStarlarkThread(ctx, e, builtinsModuleNames)
	buildFileBuiltins, err := c.getBuildFileBuiltins(thread, e, builtinsModuleNames)
	if err != nil {
		return PatchedPackageValue{}, err
	}

	listReader := c.valueReaders.List
	for _, buildFileName := range buildDotBazelTargetNames {
		buildFileProperties := e.GetFilePropertiesValue(&model_analysis_pb.FileProperties_Key{
			CanonicalRepo: canonicalRepo.String(),
			Path:          canonicalPackage.AppendTargetName(buildFileName).GetRepoRelativePath(),
		})
		if !buildFileProperties.IsSet() {
			return PatchedPackageValue{}, evaluation.ErrMissingDependency
		}
		if buildFileProperties.Message.Exists == nil {
			continue
		}

		buildFileLabel := canonicalPackage.AppendTargetName(buildFileName)
		buildFileContentsEntry, err := model_filesystem.NewFileContentsEntryFromProto(
			model_core.Nested(buildFileProperties, buildFileProperties.Message.Exists.Contents),
		)
		if err != nil {
			return PatchedPackageValue{}, fmt.Errorf("invalid contents for file %#v: %w", buildFileLabel.String(), err)
		}
		buildFileData, err := fileReader.FileReadAll(ctx, buildFileContentsEntry, 1<<20)
		if err != nil {
			return PatchedPackageValue{}, err
		}

		_, program, err := starlark.SourceProgramOptions(
			&syntax.FileOptions{
				Set: true,
			},
			buildFileLabel.String(),
			buildFileData,
			buildFileBuiltins.Has,
		)
		if err != nil {
			return PatchedPackageValue{}, fmt.Errorf("failed to load %#v: %w", buildFileLabel.String(), err)
		}

		if err := c.preloadBzlGlobals(e, canonicalPackage, program, builtinsModuleNames); err != nil {
			return PatchedPackageValue{}, err
		}

		thread.SetLocal(model_starlark.CanonicalPackageKey, canonicalPackage)
		thread.SetLocal(model_starlark.ValueEncodingOptionsKey, c.getValueEncodingOptions(e, nil))
		thread.SetLocal(model_starlark.GlobExpanderKey, func(includePatterns, excludePatterns []string, includeDirectories bool) ([]string, error) {
			nfa, err := glob.NewNFAFromPatterns(includePatterns, excludePatterns)
			if err != nil {
				return nil, err
			}
			globValue := e.GetGlobValue(&model_analysis_pb.Glob_Key{
				Package:            canonicalPackage.String(),
				Pattern:            nfa.Bytes(),
				IncludeDirectories: includeDirectories,
			})
			if !globValue.IsSet() {
				return nil, evaluation.ErrMissingDependency
			}
			return globValue.Message.MatchedPaths, nil
		})

		repoDefaultAttrs := model_core.NewMessageFromPatchedReferenced(
			model_core.CloningObjectManager[TMetadata]{},
			model_core.NewPatchedMessageFromExistingCaptured(
				e,
				model_core.Nested(repoDefaultAttrsValue, repoDefaultAttrsValue.Message.InheritableAttrs),
			),
		)

		targetRegistrar := model_starlark.NewTargetRegistrar[TMetadata](c.getInlinedTreeOptions(), e, repoDefaultAttrs)
		thread.SetLocal(model_starlark.TargetRegistrarKey, targetRegistrar)

		thread.SetLocal(model_starlark.GlobalResolverKey, func(identifier label.CanonicalStarlarkIdentifier) (model_core.Message[*model_starlark_pb.Value, TReference], error) {
			canonicalLabel := identifier.GetCanonicalLabel()
			compiledBzlFile := e.GetCompiledBzlFileValue(&model_analysis_pb.CompiledBzlFile_Key{
				Label:               canonicalLabel.String(),
				BuiltinsModuleNames: trimBuiltinModuleNames(builtinsModuleNames, canonicalLabel.GetCanonicalRepo().GetModuleInstance().GetModule()),
			})
			if !compiledBzlFile.IsSet() {
				return model_core.Message[*model_starlark_pb.Value, TReference]{}, evaluation.ErrMissingDependency
			}
			return model_starlark.GetStructFieldValue(
				ctx,
				listReader,
				model_core.Nested(compiledBzlFile, compiledBzlFile.Message.CompiledProgram.GetGlobals()),
				identifier.GetStarlarkIdentifier().String(),
			)
		})

		// Execute the BUILD.bazel file, so that all targets
		// contained within are instantiated.
		if _, err := program.Init(thread, buildFileBuiltins); err != nil {
			var evalErr *starlark.EvalError
			if !errors.Is(err, evaluation.ErrMissingDependency) && errors.As(err, &evalErr) {
				return PatchedPackageValue{}, errors.New(evalErr.Backtrace())
			}
			return PatchedPackageValue{}, err
		}

		// Store all targets in a B-tree.
		// TODO: Use a proper encoder!
		treeBuilder := btree.NewSplitProllyBuilder(
			/* minimumSizeBytes = */ 32*1024,
			/* maximumSizeBytes = */ 128*1024,
			btree.NewObjectCreatingNodeMerger(
				c.getValueObjectEncoder(),
				c.getReferenceFormat(),
				/* parentNodeComputer = */ func(createdObject model_core.CreatedObject[TMetadata], childNodes []*model_analysis_pb.Package_Value_Target) (model_core.PatchedMessage[*model_analysis_pb.Package_Value_Target, TMetadata], error) {
					var firstName string
					switch firstElement := childNodes[0].Level.(type) {
					case *model_analysis_pb.Package_Value_Target_Leaf:
						firstName = firstElement.Leaf.Name
					case *model_analysis_pb.Package_Value_Target_Parent_:
						firstName = firstElement.Parent.FirstName
					}
					patcher := model_core.NewReferenceMessagePatcher[TMetadata]()
					return model_core.NewPatchedMessage(
						&model_analysis_pb.Package_Value_Target{
							Level: &model_analysis_pb.Package_Value_Target_Parent_{
								Parent: &model_analysis_pb.Package_Value_Target_Parent{
									Reference: patcher.AddReference(
										createdObject.Contents.GetReference(),
										e.CaptureCreatedObject(createdObject),
									),
									FirstName: firstName,
								},
							},
						},
						patcher,
					), nil
				},
			),
		)

		targets := targetRegistrar.GetTargets()
		for _, name := range slices.Sorted(maps.Keys(targets)) {
			target := targets[name]
			if !target.IsSet() {
				// Target is referenced, but not
				// provided explicitly. Assume it refers
				// to a source file with private
				// visibility.
				target = model_core.NewSimplePatchedMessage[TMetadata](
					&model_starlark_pb.Target_Definition{
						Kind: &model_starlark_pb.Target_Definition_SourceFileTarget{
							SourceFileTarget: &model_starlark_pb.SourceFileTarget{
								Visibility: &model_starlark_pb.PackageGroup{
									Tree: &model_starlark_pb.PackageGroup_Subpackages{},
								},
							},
						},
					},
				)
			}
			if err := treeBuilder.PushChild(model_core.NewPatchedMessage(
				&model_analysis_pb.Package_Value_Target{
					Level: &model_analysis_pb.Package_Value_Target_Leaf{
						Leaf: &model_starlark_pb.Target{
							Name:       name,
							Definition: target.Message,
						},
					},
				},
				target.Patcher,
			)); err != nil {
				return PatchedPackageValue{}, err
			}
		}

		targetsList, err := treeBuilder.FinalizeList()
		if err != nil {
			return PatchedPackageValue{}, err
		}

		return model_core.NewPatchedMessage(
			&model_analysis_pb.Package_Value{
				Targets: targetsList.Message,
			},
			model_core.MapReferenceMetadataToWalkers(targetsList.Patcher),
		), nil
	}

	return PatchedPackageValue{}, errors.New("BUILD.bazel does not exist")
}
