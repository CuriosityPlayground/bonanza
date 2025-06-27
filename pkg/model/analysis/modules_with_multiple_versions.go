package analysis

import (
	"context"

	"bonanza.build/pkg/evaluation"
	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	"bonanza.build/pkg/storage/dag"
)

func (c *baseComputer[TReference, TMetadata]) ComputeModulesWithMultipleVersionsValue(ctx context.Context, key *model_analysis_pb.ModulesWithMultipleVersions_Key, e ModulesWithMultipleVersionsEnvironment[TReference, TMetadata]) (PatchedModulesWithMultipleVersionsValue, error) {
	modulesWithOverridesValue := e.GetModulesWithOverridesValue(&model_analysis_pb.ModulesWithOverrides_Key{})
	if !modulesWithOverridesValue.IsSet() {
		return PatchedModulesWithMultipleVersionsValue{}, evaluation.ErrMissingDependency
	}
	var modulesWithMultipleVersions []*model_analysis_pb.OverridesListModule
	for _, module := range modulesWithOverridesValue.Message.OverridesList {
		if len(module.Versions) > 0 {
			modulesWithMultipleVersions = append(modulesWithMultipleVersions, module)
		}
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.ModulesWithMultipleVersions_Value{
		OverridesList: modulesWithMultipleVersions,
	}), nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeModulesWithMultipleVersionsObjectValue(ctx context.Context, key *model_analysis_pb.ModulesWithMultipleVersionsObject_Key, e ModulesWithMultipleVersionsObjectEnvironment[TReference, TMetadata]) (map[label.Module]OverrideVersions, error) {
	modulesWithMultipleVersionsValue := e.GetModulesWithMultipleVersionsValue(&model_analysis_pb.ModulesWithMultipleVersions_Key{})
	if !modulesWithMultipleVersionsValue.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	return parseOverridesList(modulesWithMultipleVersionsValue.Message.OverridesList)
}
