package analysis

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	model_starlark "github.com/buildbarn/bonanza/pkg/model/starlark"
	model_analysis_pb "github.com/buildbarn/bonanza/pkg/proto/model/analysis"
	model_core_pb "github.com/buildbarn/bonanza/pkg/proto/model/core"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
	pg_starlark "github.com/buildbarn/bonanza/pkg/starlark"
	"github.com/buildbarn/bonanza/pkg/storage/dag"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"go.starlark.net/starlark"
)

var platformInfoProviderIdentifier = label.MustNewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%PlatformInfo")

type registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	computer              *baseComputer[TReference, TMetadata]
	context               context.Context
	environment           RegisteredExecutionPlatformsEnvironment[TReference]
	moduleInstance        label.ModuleInstance
	ignoreDevDependencies bool
	executionPlatforms    *[]*model_analysis_pb.ExecutionPlatform
}

func (registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]) BazelDep(name label.Module, version *label.ModuleVersion, maxCompatibilityLevel int, repoName label.ApparentRepo, devDependency bool) error {
	return nil
}

func (registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]) Module(name label.Module, version *label.ModuleVersion, compatibilityLevel int, repoName label.ApparentRepo, bazelCompatibility []string) error {
	return nil
}

func (c *baseComputer[TReference, TMetadata]) extractFromPlatformInfoConstraints(ctx context.Context, value model_core.Message[*model_starlark_pb.Value, TReference]) ([]*model_analysis_pb.Constraint, error) {
	dict, ok := value.Message.Kind.(*model_starlark_pb.Value_Dict)
	if !ok {
		return nil, errors.New("constraints field of PlatformInfo")
	}
	var iterErr error
	constraints := map[string]string{}
	for entry := range model_starlark.AllDictLeafEntries(
		ctx,
		c.valueReaders.Dict,
		model_core.NewNestedMessage(value, dict.Dict),
		&iterErr,
	) {
		key, ok := entry.Message.Key.GetKind().(*model_starlark_pb.Value_Label)
		if !ok {
			return nil, errors.New("key of constraints field of PlatformInfo is not a label")
		}
		value, ok := entry.Message.Value.GetKind().(*model_starlark_pb.Value_Label)
		if !ok {
			return nil, errors.New("value of constraints field of PlatformInfo is not a label")
		}
		constraints[key.Label] = value.Label
	}
	if iterErr != nil {
		return nil, fmt.Errorf("failed to iterate dict of constraints field of PlatformInfo: %w", iterErr)
	}
	sortedConstraints := make([]*model_analysis_pb.Constraint, 0, len(constraints))
	for _, setting := range slices.Sorted(maps.Keys(constraints)) {
		sortedConstraints = append(sortedConstraints, &model_analysis_pb.Constraint{
			Setting: setting,
			Value:   constraints[setting],
		})
	}
	return sortedConstraints, nil
}

func (h *registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]) RegisterExecutionPlatforms(platformTargetPatterns []label.ApparentTargetPattern, devDependency bool) error {
	if !devDependency || !h.ignoreDevDependencies {
		listReader := h.computer.valueReaders.List
		for _, apparentPlatformTargetPattern := range platformTargetPatterns {
			canonicalPlatformTargetPattern, err := resolveApparent(h.environment, h.moduleInstance.GetBareCanonicalRepo(), apparentPlatformTargetPattern)
			if err != nil {
				return err
			}
			var iterErr error
			for canonicalPlatformLabel := range h.computer.expandCanonicalTargetPattern(h.context, h.environment, canonicalPlatformTargetPattern, &iterErr) {
				canonicalPlatformLabelStr := canonicalPlatformLabel.String()
				platformInfoProvider, err := getProviderFromConfiguredTarget(
					h.environment,
					canonicalPlatformLabelStr,
					model_core.NewSimplePatchedMessage[model_core.WalkableReferenceMetadata, *model_core_pb.Reference](nil),
					platformInfoProviderIdentifier,
				)
				if err != nil {
					return fmt.Errorf("failed to obtain PlatformInfo provider for target %#v: %w", canonicalPlatformLabelStr, err)
				}

				// Extract constraints and PKIX public key
				// fields from the PlatformInfo providers. These
				// are the only two fields that need to be
				// considered when using platforms for executing
				// actions.
				var constraints []*model_analysis_pb.Constraint
				var execPKIXPublicKey []byte
				var errIter error
				for key, value := range model_starlark.AllStructFields(
					h.context,
					listReader,
					platformInfoProvider,
					&errIter,
				) {
					switch key {
					case "constraints":
						constraints, err = h.computer.extractFromPlatformInfoConstraints(h.context, value)
						if err != nil {
							return fmt.Errorf("failed to extract constraints from execution platform %#v: %w", err)
						}
					case "exec_pkix_public_key":
						str, ok := value.Message.Kind.(*model_starlark_pb.Value_Str)
						if !ok {
							return fmt.Errorf("exec_pkix_public_key field of PlatformInfo of execution platform %#v is not a string", canonicalPlatformLabelStr)
						}
						execPKIXPublicKey, err = base64.StdEncoding.DecodeString(str.Str)
						if err != nil {
							return fmt.Errorf("exec_pkix_public_key field of PlatformInfo of execution platform %#v: %w", canonicalPlatformLabelStr, err)
						}
					}
				}
				if errIter != nil {
					return errIter
				}
				if constraints == nil {
					return fmt.Errorf("PlatformInfo of execution platform %#v does not contain field constraints", canonicalPlatformLabelStr)
				}
				if len(execPKIXPublicKey) == 0 {
					return fmt.Errorf("exec_pkix_public_key field of PlatformInfo of execution platform %#v is not set to a non-empty string", canonicalPlatformLabelStr)
				}

				*h.executionPlatforms = append(*h.executionPlatforms, &model_analysis_pb.ExecutionPlatform{
					Constraints:       constraints,
					ExecPkixPublicKey: execPKIXPublicKey,
				})
			}
			if iterErr != nil {
				return fmt.Errorf("failed to expand target pattern %#v: %w", canonicalPlatformTargetPattern.String(), iterErr)
			}
			return nil
		}
	}
	return nil
}

func (registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]) RegisterToolchains(toolchainTargetPatterns []label.ApparentTargetPattern, devDependency bool) error {
	return nil
}

func (registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]) UseExtension(extensionBzlFile label.ApparentLabel, extensionName label.StarlarkIdentifier, devDependency, isolate bool) (pg_starlark.ModuleExtensionProxy, error) {
	return pg_starlark.NullModuleExtensionProxy, nil
}

func (registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]) UseRepoRule(repoRuleBzlFile label.ApparentLabel, repoRuleName string) (pg_starlark.RepoRuleProxy, error) {
	return func(name label.ApparentRepo, devDependency bool, attrs map[string]starlark.Value) error {
		return nil
	}, nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeRegisteredExecutionPlatformsValue(ctx context.Context, key *model_analysis_pb.RegisteredExecutionPlatforms_Key, e RegisteredExecutionPlatformsEnvironment[TReference]) (PatchedRegisteredExecutionPlatformsValue, error) {
	var executionPlatforms []*model_analysis_pb.ExecutionPlatform
	if err := c.visitModuleDotBazelFilesBreadthFirst(ctx, e, func(moduleInstance label.ModuleInstance, ignoreDevDependencies bool) pg_starlark.ChildModuleDotBazelHandler {
		return &registeredExecutionPlatformExtractingModuleDotBazelHandler[TReference, TMetadata]{
			computer:              c,
			context:               ctx,
			environment:           e,
			moduleInstance:        moduleInstance,
			ignoreDevDependencies: ignoreDevDependencies,
			executionPlatforms:    &executionPlatforms,
		}
	}); err != nil {
		return PatchedRegisteredExecutionPlatformsValue{}, err
	}

	if len(executionPlatforms) == 0 {
		return PatchedRegisteredExecutionPlatformsValue{}, errors.New("failed to find registered execution platforms in any of the MODULE.bazel files")
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.RegisteredExecutionPlatforms_Value{
		ExecutionPlatforms: executionPlatforms,
	}), nil
}
