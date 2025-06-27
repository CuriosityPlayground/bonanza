package analysis

import (
	"context"

	"bonanza.build/pkg/evaluation"
	model_core "bonanza.build/pkg/model/core"
	model_filesystem "bonanza.build/pkg/model/filesystem"
	model_analysis_pb "bonanza.build/pkg/proto/model/analysis"
	"bonanza.build/pkg/storage/dag"
)

func (c *baseComputer[TReference, TMetadata]) ComputeDirectoryCreationParametersValue(ctx context.Context, key *model_analysis_pb.DirectoryCreationParameters_Key, e DirectoryCreationParametersEnvironment[TReference, TMetadata]) (PatchedDirectoryCreationParametersValue, error) {
	buildSpecification := e.GetBuildSpecificationValue(&model_analysis_pb.BuildSpecification_Key{})
	if !buildSpecification.IsSet() {
		return PatchedDirectoryCreationParametersValue{}, evaluation.ErrMissingDependency
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](&model_analysis_pb.DirectoryCreationParameters_Value{
		DirectoryCreationParameters: buildSpecification.Message.BuildSpecification.GetDirectoryCreationParameters(),
	}), nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeDirectoryCreationParametersObjectValue(ctx context.Context, key *model_analysis_pb.DirectoryCreationParametersObject_Key, e DirectoryCreationParametersObjectEnvironment[TReference, TMetadata]) (*model_filesystem.DirectoryCreationParameters, error) {
	directoryCreationParameters := e.GetDirectoryCreationParametersValue(&model_analysis_pb.DirectoryCreationParameters_Key{})
	if !directoryCreationParameters.IsSet() {
		return nil, evaluation.ErrMissingDependency
	}
	return model_filesystem.NewDirectoryCreationParametersFromProto(
		directoryCreationParameters.Message.DirectoryCreationParameters,
		c.getReferenceFormat(),
	)
}
