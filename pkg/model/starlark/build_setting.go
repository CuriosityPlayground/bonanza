package starlark

import (
	"fmt"

	pg_label "bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/starlark/unpack"

	"google.golang.org/protobuf/types/known/emptypb"

	"go.starlark.net/starlark"
)

type BuildSetting struct {
	buildSettingType BuildSettingType
	flag             bool
}

var _ starlark.Value = (*BuildSetting)(nil)

func NewBuildSetting(buildSettingType BuildSettingType, flag bool) *BuildSetting {
	return &BuildSetting{
		buildSettingType: buildSettingType,
		flag:             flag,
	}
}

func (bs *BuildSetting) String() string {
	return fmt.Sprintf("<config.%s>", bs.buildSettingType.Type())
}

func (bs *BuildSetting) Type() string {
	return "config." + bs.buildSettingType.Type()
}

func (bs *BuildSetting) Freeze() {}

func (bs *BuildSetting) Truth() starlark.Bool {
	return starlark.True
}

func (bs *BuildSetting) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, fmt.Errorf("config.%s cannot be hashed", bs.buildSettingType.Type())
}

func (bs *BuildSetting) Encode() *model_starlark_pb.BuildSetting {
	buildSetting := model_starlark_pb.BuildSetting{
		Flag: bs.flag,
	}
	bs.buildSettingType.Encode(&buildSetting)
	return &buildSetting
}

type BuildSettingType interface {
	Type() string
	Encode(out *model_starlark_pb.BuildSetting)
	GetCanonicalizer(currentPackage pg_label.CanonicalPackage) unpack.Canonicalizer
}

type boolBuildSettingType struct{}

var BoolBuildSettingType BuildSettingType = boolBuildSettingType{}

func (boolBuildSettingType) Type() string {
	return "bool"
}

func (boolBuildSettingType) Encode(out *model_starlark_pb.BuildSetting) {
	out.Type = &model_starlark_pb.BuildSetting_Bool{
		Bool: &emptypb.Empty{},
	}
}

func (boolBuildSettingType) GetCanonicalizer(currentPackage pg_label.CanonicalPackage) unpack.Canonicalizer {
	return unpack.Bool
}

type intBuildSettingType struct{}

var IntBuildSettingType BuildSettingType = intBuildSettingType{}

func (intBuildSettingType) Type() string {
	return "int"
}

func (intBuildSettingType) Encode(out *model_starlark_pb.BuildSetting) {
	out.Type = &model_starlark_pb.BuildSetting_Int{
		Int: &emptypb.Empty{},
	}
}

func (intBuildSettingType) GetCanonicalizer(currentPackage pg_label.CanonicalPackage) unpack.Canonicalizer {
	return unpack.Int[int32]()
}

type labelListBuildSettingType[TReference any, TMetadata model_core.CloneableReferenceMetadata] struct {
	repeatable bool
}

func NewLabelListBuildSettingType[TReference any, TMetadata model_core.CloneableReferenceMetadata](repeatable bool) BuildSettingType {
	return labelListBuildSettingType[TReference, TMetadata]{
		repeatable: repeatable,
	}
}

func (labelListBuildSettingType[TReference, TMetadata]) Type() string {
	return "label_list"
}

func (bst labelListBuildSettingType[TReference, TMetadata]) Encode(out *model_starlark_pb.BuildSetting) {
	out.Type = &model_starlark_pb.BuildSetting_LabelList{
		LabelList: &model_starlark_pb.BuildSetting_ListType{
			Repeatable: bst.repeatable,
		},
	}
}

func (labelListBuildSettingType[TReference, TMetadata]) GetCanonicalizer(currentPackage pg_label.CanonicalPackage) unpack.Canonicalizer {
	return unpack.List(NewLabelOrStringUnpackerInto[TReference, TMetadata](currentPackage))
}

type stringBuildSettingType struct{}

var StringBuildSettingType BuildSettingType = stringBuildSettingType{}

func (stringBuildSettingType) Type() string {
	return "string"
}

func (stringBuildSettingType) Encode(out *model_starlark_pb.BuildSetting) {
	out.Type = &model_starlark_pb.BuildSetting_String_{
		String_: &emptypb.Empty{},
	}
}

func (stringBuildSettingType) GetCanonicalizer(currentPackage pg_label.CanonicalPackage) unpack.Canonicalizer {
	return unpack.String
}

type stringListBuildSettingType struct {
	repeatable bool
}

func NewStringListBuildSettingType(repeatable bool) BuildSettingType {
	return stringListBuildSettingType{
		repeatable: repeatable,
	}
}

func (stringListBuildSettingType) Type() string {
	return "string_list"
}

func (bst stringListBuildSettingType) Encode(out *model_starlark_pb.BuildSetting) {
	out.Type = &model_starlark_pb.BuildSetting_StringList{
		StringList: &model_starlark_pb.BuildSetting_ListType{
			Repeatable: bst.repeatable,
		},
	}
}

func (stringListBuildSettingType) GetCanonicalizer(currentPackage pg_label.CanonicalPackage) unpack.Canonicalizer {
	return unpack.List(unpack.String)
}
