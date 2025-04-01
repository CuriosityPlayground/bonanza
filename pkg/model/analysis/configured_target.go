package analysis

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/buildbarn/bonanza/pkg/evaluation"
	"github.com/buildbarn/bonanza/pkg/label"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	"github.com/buildbarn/bonanza/pkg/model/core/btree"
	model_parser "github.com/buildbarn/bonanza/pkg/model/parser"
	model_starlark "github.com/buildbarn/bonanza/pkg/model/starlark"
	model_analysis_pb "github.com/buildbarn/bonanza/pkg/proto/model/analysis"
	model_core_pb "github.com/buildbarn/bonanza/pkg/proto/model/core"
	model_starlark_pb "github.com/buildbarn/bonanza/pkg/proto/model/starlark"
	"github.com/buildbarn/bonanza/pkg/starlark/unpack"
	"github.com/buildbarn/bonanza/pkg/storage/dag"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"google.golang.org/protobuf/types/known/emptypb"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

var (
	constraintValueInfoProviderIdentifier      = label.MustNewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%ConstraintValueInfo")
	defaultInfoProviderIdentifier              = label.MustNewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%DefaultInfo")
	fragmentInfoProviderIdentifier             = label.MustNewCanonicalStarlarkIdentifier("@@bazel_tools+//fragments:fragments.bzl%FragmentInfo")
	fragmentsPackage                           = label.MustNewCanonicalPackage("@@bazel_tools+//fragments")
	packageSpecificationInfoProviderIdentifier = label.MustNewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%PackageSpecificationInfo")
	toolchainInfoProviderIdentifier            = label.MustNewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%ToolchainInfo")
	filesToRunProviderIdentifier               = label.MustNewCanonicalStarlarkIdentifier("@@builtins_core+//:exports.bzl%FilesToRun")
)

// constraintValuesToConstraints converts a list of labels of constraint
// values to a list of Constraint messages that include both the
// constraint setting and constraint value labels. These can be used to
// perform matching of constraints.
func (c *baseComputer[TReference, TMetadata]) constraintValuesToConstraints(ctx context.Context, e getProviderFromVisibleConfiguredTargetEnvironment[TReference], fromPackage label.CanonicalPackage, constraintValues []string) ([]*model_analysis_pb.Constraint, error) {
	constraints := make(map[string]string, len(constraintValues))
	missingDependencies := false
	for _, constraintValue := range constraintValues {
		constrainValueInfoProvider, _, err := getProviderFromVisibleConfiguredTarget(
			e,
			fromPackage.String(),
			constraintValue,
			model_core.NewSimpleMessage[model_core.CloneableReference[TMetadata]]((*model_core_pb.Reference)(nil)),
			model_core.CloningObjectManager[TMetadata]{},
			constraintValueInfoProviderIdentifier,
		)
		if err != nil {
			if errors.Is(err, evaluation.ErrMissingDependency) {
				missingDependencies = true
				continue
			}
			return nil, err
		}

		var actualConstraintSetting, actualConstraintValue, defaultConstraintValue *string
		var errIter error
		listReader := c.valueReaders.List
		for key, value := range model_starlark.AllStructFields(ctx, listReader, constrainValueInfoProvider, &errIter) {
			switch key {
			case "constraint":
				constraintSettingInfoProvider, ok := value.Message.Kind.(*model_starlark_pb.Value_Struct)
				if !ok {
					return nil, fmt.Errorf("field \"constraint\" of ConstraintValueInfo provider of target %#v is not a struct")
				}
				var errIter error
				for key, value := range model_starlark.AllStructFields(
					ctx,
					listReader,
					model_core.Nested(value, constraintSettingInfoProvider.Struct.Fields),
					&errIter,
				) {
					switch key {
					case "default_constraint_value":
						switch v := value.Message.Kind.(type) {
						case *model_starlark_pb.Value_Label:
							defaultConstraintValue = &v.Label
						case *model_starlark_pb.Value_None:
						default:
							return nil, fmt.Errorf("field \"constraint.default_constraint_value\" of ConstraintValueInfo provider of target %#v is not a Label or None")
						}
					case "label":
						v, ok := value.Message.Kind.(*model_starlark_pb.Value_Label)
						if !ok {
							return nil, fmt.Errorf("field \"constraint.label\" of ConstraintValueInfo provider of target %#v is not a Label")
						}
						actualConstraintSetting = &v.Label
					}
				}
				if errIter != nil {
					return nil, err
				}
			case "label":
				v, ok := value.Message.Kind.(*model_starlark_pb.Value_Label)
				if !ok {
					return nil, fmt.Errorf("field \"label\" of ConstraintValueInfo provider of target %#v is not a Label")
				}
				actualConstraintValue = &v.Label
			}
		}
		if errIter != nil {
			return nil, errIter
		}
		if actualConstraintSetting == nil {
			return nil, fmt.Errorf("ConstraintValueInfo provider of target %#v does not contain field \"constraint.label\"")
		}
		if actualConstraintValue == nil {
			return nil, fmt.Errorf("ConstraintValueInfo provider of target %#v does not contain field \"label\"")
		}
		effectiveConstraintValue := *actualConstraintValue
		if defaultConstraintValue != nil && effectiveConstraintValue == *defaultConstraintValue {
			effectiveConstraintValue = ""
		}

		if _, ok := constraints[*actualConstraintSetting]; ok {
			return nil, fmt.Errorf("got multiple constraint values for constraint setting %#v", *actualConstraintSetting)
		}
		constraints[*actualConstraintSetting] = effectiveConstraintValue

	}
	if missingDependencies {
		return nil, evaluation.ErrMissingDependency
	}

	sortedConstraints := make([]*model_analysis_pb.Constraint, 0, len(constraints))
	for _, constraintSetting := range slices.Sorted(maps.Keys(constraints)) {
		sortedConstraints = append(
			sortedConstraints,
			&model_analysis_pb.Constraint{
				Setting: constraintSetting,
				Value:   constraints[constraintSetting],
			},
		)
	}
	return sortedConstraints, nil
}

func getDefaultInfoSimpleFilesToRun(executable *model_starlark_pb.Value) *model_starlark_pb.List_Element {
	return &model_starlark_pb.List_Element{
		Level: &model_starlark_pb.List_Element_Leaf{
			Leaf: &model_starlark_pb.Value{
				Kind: &model_starlark_pb.Value_Struct{
					Struct: &model_starlark_pb.Struct{
						ProviderInstanceProperties: &model_starlark_pb.Provider_InstanceProperties{
							ProviderIdentifier: filesToRunProviderIdentifier.String(),
						},
						Fields: &model_starlark_pb.Struct_Fields{
							Keys: []string{
								"executable",
								"repo_mapping_manifest",
								"runfiles_manifest",
							},
							Values: []*model_starlark_pb.List_Element{
								{
									Level: &model_starlark_pb.List_Element_Leaf{
										Leaf: executable,
									},
								},
								{
									Level: &model_starlark_pb.List_Element_Leaf{
										Leaf: &model_starlark_pb.Value{
											Kind: &model_starlark_pb.Value_None{
												None: &emptypb.Empty{},
											},
										},
									},
								},
								{
									Level: &model_starlark_pb.List_Element_Leaf{
										Leaf: &model_starlark_pb.Value{
											Kind: &model_starlark_pb.Value_None{
												None: &emptypb.Empty{},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

var emptyRunfilesValue = &model_starlark_pb.Value{
	Kind: &model_starlark_pb.Value_Runfiles{
		Runfiles: &model_starlark_pb.Runfiles{
			Files:        &model_starlark_pb.Depset{},
			RootSymlinks: &model_starlark_pb.Depset{},
			Symlinks:     &model_starlark_pb.Depset{},
		},
	},
}

var defaultInfoProviderInstanceProperties = model_starlark.NewProviderInstanceProperties(&defaultInfoProviderIdentifier, false)

func getSingleFileConfiguredTargetValue(file *model_starlark_pb.File) PatchedConfiguredTargetValue {
	fileValue := &model_starlark_pb.Value{
		Kind: &model_starlark_pb.Value_File{
			File: file,
		},
	}
	return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](
		&model_analysis_pb.ConfiguredTarget_Value{
			ProviderInstances: []*model_starlark_pb.Struct{{
				ProviderInstanceProperties: &model_starlark_pb.Provider_InstanceProperties{
					ProviderIdentifier: defaultInfoProviderIdentifier.String(),
				},
				Fields: &model_starlark_pb.Struct_Fields{
					Keys: []string{
						"data_runfiles",
						"default_runfiles",
						"files",
						"files_to_run",
					},
					Values: []*model_starlark_pb.List_Element{
						{
							Level: &model_starlark_pb.List_Element_Leaf{
								Leaf: emptyRunfilesValue,
							},
						},
						{
							Level: &model_starlark_pb.List_Element_Leaf{
								Leaf: emptyRunfilesValue,
							},
						},
						{
							Level: &model_starlark_pb.List_Element_Leaf{
								Leaf: &model_starlark_pb.Value{
									Kind: &model_starlark_pb.Value_Depset{
										Depset: &model_starlark_pb.Depset{
											Elements: []*model_starlark_pb.List_Element{{
												Level: &model_starlark_pb.List_Element_Leaf{
													Leaf: fileValue,
												},
											}},
										},
									},
								},
							},
						},
						getDefaultInfoSimpleFilesToRun(fileValue),
					},
				},
			}},
		},
	)
}

func getAttrValueParts[TReference object.BasicReference, TMetadata model_core.WalkableReferenceMetadata](
	e getValueFromSelectGroupEnvironment[TReference, TMetadata],
	configurationReference model_core.Message[*model_core_pb.Reference, TReference],
	ruleTargetPackage label.CanonicalPackage,
	namedAttr model_core.Message[*model_starlark_pb.NamedAttr, TReference],
	publicAttrValue model_core.Message[*model_starlark_pb.RuleTarget_PublicAttrValue, TReference],
) (valueParts model_core.Message[[]*model_starlark_pb.Value, TReference], usedDefaultValue bool, err error) {
	if !strings.HasPrefix(namedAttr.Message.Name, "_") {
		// Attr is public. Extract the value from the rule target.
		selectGroups := publicAttrValue.Message.ValueParts
		if len(selectGroups) == 0 {
			return model_core.Message[[]*model_starlark_pb.Value, TReference]{}, false, fmt.Errorf("attr %#v has no select groups", namedAttr.Message.Name)
		}

		valueParts := make([]*model_starlark_pb.Value, 0, len(selectGroups))
		missingDependencies := false
		for _, selectGroup := range selectGroups {
			valuePart, err := getValueFromSelectGroup(
				e,
				configurationReference,
				ruleTargetPackage,
				selectGroup,
				false,
			)
			if err == nil {
				valueParts = append(valueParts, valuePart)
			} else if errors.Is(err, evaluation.ErrMissingDependency) {
				missingDependencies = true
			} else {
				return model_core.Message[[]*model_starlark_pb.Value, TReference]{}, false, err
			}
		}
		if missingDependencies {
			return model_core.Message[[]*model_starlark_pb.Value, TReference]{}, false, evaluation.ErrMissingDependency
		}

		// Use the value from the rule target if it's not None.
		if len(valueParts) > 1 {
			return model_core.Nested(publicAttrValue, valueParts), false, nil
		}
		if _, ok := valueParts[0].Kind.(*model_starlark_pb.Value_None); !ok {
			return model_core.Nested(publicAttrValue, valueParts), false, nil
		}
	}

	// No value provided. Use the default value from the rule definition.
	defaultValue := namedAttr.Message.Attr.GetDefault()
	if defaultValue == nil {
		return model_core.Message[[]*model_starlark_pb.Value, TReference]{}, false, fmt.Errorf("missing value for mandatory attr %#v", namedAttr.Message.Name)
	}
	return model_core.Nested(namedAttr, []*model_starlark_pb.Value{defaultValue}), true, nil
}

func (c *baseComputer[TReference, TMetadata]) configureAttrValueParts(
	ctx context.Context,
	e ConfiguredTargetEnvironment[TReference, TMetadata],
	thread *starlark.Thread,
	namedAttr *model_starlark_pb.NamedAttr,
	valueParts model_core.Message[[]*model_starlark_pb.Value, TReference],
	configurationReference model_core.Message[*model_core_pb.Reference, TReference],
	visibilityFromPackage label.CanonicalPackage,
	execGroupPlatformLabels map[string]string,
) (starlark.Value, error) {
	// See if any transitions need to be applied.
	var cfg *model_starlark_pb.Transition_Reference
	isScalar := false
	switch attrType := namedAttr.Attr.GetType().(type) {
	case *model_starlark_pb.Attr_Label:
		cfg = attrType.Label.ValueOptions.GetCfg()
		isScalar = true
	case *model_starlark_pb.Attr_LabelKeyedStringDict:
		cfg = attrType.LabelKeyedStringDict.DictKeyOptions.GetCfg()
	case *model_starlark_pb.Attr_LabelList:
		cfg = attrType.LabelList.ListValueOptions.GetCfg()
	}

	var configurationReferences []model_core.Message[*model_core_pb.Reference, TReference]
	mayHaveMultipleConfigurations := false
	if cfg != nil {
		var patchedResult model_core.PatchedMessage[*model_analysis_pb.UserDefinedTransition_Value_Success, TMetadata]
		var err error
		patchedResult, mayHaveMultipleConfigurations, err = c.performTransition(
			ctx,
			e,
			cfg,
			configurationReference,
			model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
				// TODO!
			}),
			execGroupPlatformLabels,
		)
		if err != nil {
			return nil, err
		}
		result := model_core.Unpatch(e, patchedResult)
		for _, entry := range result.Message.Entries {
			configurationReferences = append(configurationReferences, model_core.Nested(result, entry.OutputConfigurationReference))
		}
	}

	var attr starlark.Value
	if len(configurationReferences) == 0 {
		for _, valuePart := range valueParts.Message {
			decodedPart, err := model_starlark.DecodeValue[TReference, TMetadata](
				model_core.Nested(valueParts, valuePart),
				/* currentIdentifier = */ nil,
				c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
					// We should leave the target
					// unconfigured. Provide a
					// target reference that does
					// not contain any providers.
					return model_starlark.NewTargetReference[TReference, TMetadata](
						resolvedLabel,
						model_core.NewSimpleMessage[TReference]([]*model_starlark_pb.Struct(nil)),
					), nil
				}),
			)
			if err != nil {
				return nil, err
			}
			if err := concatenateAttrValueParts(thread, &attr, decodedPart); err != nil {
				return nil, err
			}
		}
	} else {
		missingDependencies := false
		for _, configurationReference := range configurationReferences {
			valueDecodingOptions := c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
				// Resolve the label.
				canonicalLabel, err := resolvedLabel.AsCanonical()
				if err != nil {
					return nil, err
				}
				patchedConfigurationReference1 := model_core.Patch(e, configurationReference)
				resolvedLabelValue := e.GetVisibleTargetValue(
					model_core.NewPatchedMessage(
						&model_analysis_pb.VisibleTarget_Key{
							FromPackage:            visibilityFromPackage.String(),
							ToLabel:                canonicalLabel.String(),
							ConfigurationReference: patchedConfigurationReference1.Message,
						},
						model_core.MapReferenceMetadataToWalkers(patchedConfigurationReference1.Patcher),
					),
				)
				if !resolvedLabelValue.IsSet() {
					missingDependencies = true
					return starlark.None, nil
				}
				if resolvedLabelStr := resolvedLabelValue.Message.Label; resolvedLabelStr != "" {
					canonicalLabel, err := label.NewCanonicalLabel(resolvedLabelStr)
					if err != nil {
						return nil, fmt.Errorf("invalid label %#v: %w", resolvedLabelStr, err)
					}

					// Obtain the providers of the target.
					patchedConfigurationReference2 := model_core.Patch(e, configurationReference)
					configuredTarget := e.GetConfiguredTargetValue(
						model_core.NewPatchedMessage(
							&model_analysis_pb.ConfiguredTarget_Key{
								Label:                  resolvedLabelStr,
								ConfigurationReference: patchedConfigurationReference2.Message,
							},
							model_core.MapReferenceMetadataToWalkers(patchedConfigurationReference2.Patcher),
						),
					)
					if !configuredTarget.IsSet() {
						missingDependencies = true
						return starlark.None, nil
					}

					return model_starlark.NewTargetReference[TReference, TMetadata](
						canonicalLabel.AsResolved(),
						model_core.Nested(configuredTarget, configuredTarget.Message.ProviderInstances),
					), nil
				} else {
					return starlark.None, nil
				}
			})
			for _, valuePart := range valueParts.Message {
				decodedPart, err := model_starlark.DecodeValue[TReference, TMetadata](
					model_core.Nested(valueParts, valuePart),
					/* currentIdentifier = */ nil,
					valueDecodingOptions,
				)
				if err != nil {
					return nil, err
				}
				if isScalar && mayHaveMultipleConfigurations {
					decodedPart = starlark.NewList([]starlark.Value{decodedPart})
				}
				if err := concatenateAttrValueParts(thread, &attr, decodedPart); err != nil {
					return nil, err
				}
			}
		}
		if missingDependencies {
			return nil, evaluation.ErrMissingDependency
		}
	}

	if attr == nil {
		return nil, errors.New("attr value does not have any parts")
	}
	attr.Freeze()
	return attr, nil
}

func concatenateAttrValueParts(thread *starlark.Thread, left *starlark.Value, right starlark.Value) error {
	if *left == nil {
		// Initial round.
		*left = right
		return nil
	}

	concatenationOperator := syntax.PLUS
	if _, ok := (*left).(*starlark.Dict); ok {
		concatenationOperator = syntax.PIPE
	}
	v, err := starlark.Binary(thread, concatenationOperator, *left, right)
	if err != nil {
		return err
	}
	*left = v
	return nil
}

func (c *baseComputer[TReference, TMetadata]) ComputeConfiguredTargetValue(ctx context.Context, key model_core.Message[*model_analysis_pb.ConfiguredTarget_Key, TReference], e ConfiguredTargetEnvironment[TReference, TMetadata]) (PatchedConfiguredTargetValue, error) {
	targetLabel, err := label.NewCanonicalLabel(key.Message.Label)
	if err != nil {
		return PatchedConfiguredTargetValue{}, fmt.Errorf("invalid target label: %w", err)
	}
	targetValue := e.GetTargetValue(&model_analysis_pb.Target_Key{
		Label: targetLabel.String(),
	})
	if !targetValue.IsSet() {
		return PatchedConfiguredTargetValue{}, evaluation.ErrMissingDependency
	}

	switch targetKind := targetValue.Message.Definition.GetKind().(type) {
	case *model_starlark_pb.Target_Definition_PackageGroup:
		return model_core.NewSimplePatchedMessage[dag.ObjectContentsWalker](
			&model_analysis_pb.ConfiguredTarget_Value{
				ProviderInstances: []*model_starlark_pb.Struct{
					{
						ProviderInstanceProperties: &model_starlark_pb.Provider_InstanceProperties{
							ProviderIdentifier: defaultInfoProviderIdentifier.String(),
						},
						Fields: &model_starlark_pb.Struct_Fields{
							Keys: []string{
								"data_runfiles",
								"default_runfiles",
								"files",
								"files_to_run",
							},
							Values: []*model_starlark_pb.List_Element{
								{
									Level: &model_starlark_pb.List_Element_Leaf{
										Leaf: emptyRunfilesValue,
									},
								},
								{
									Level: &model_starlark_pb.List_Element_Leaf{
										Leaf: emptyRunfilesValue,
									},
								},
								{
									Level: &model_starlark_pb.List_Element_Leaf{
										Leaf: &model_starlark_pb.Value{
											Kind: &model_starlark_pb.Value_Depset{
												Depset: &model_starlark_pb.Depset{},
											},
										},
									},
								},
								getDefaultInfoSimpleFilesToRun(&model_starlark_pb.Value{
									Kind: &model_starlark_pb.Value_None{
										None: &emptypb.Empty{},
									},
								}),
							},
						},
					},
					{
						ProviderInstanceProperties: &model_starlark_pb.Provider_InstanceProperties{
							ProviderIdentifier: packageSpecificationInfoProviderIdentifier.String(),
						},
						Fields: &model_starlark_pb.Struct_Fields{},
					},
				},
			},
		), nil
	case *model_starlark_pb.Target_Definition_PredeclaredOutputFileTarget:
		// Handcraft a DefaultInfo provider for this source file.
		return getSingleFileConfiguredTargetValue(&model_starlark_pb.File{
			Owner: &model_starlark_pb.File_Owner{
				Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
				TargetName: targetKind.PredeclaredOutputFileTarget.OwnerTargetName,
			},
			Package:             targetLabel.GetCanonicalPackage().String(),
			PackageRelativePath: targetLabel.GetTargetName().String(),
			Type:                model_starlark_pb.File_FILE,
		}), nil
	case *model_starlark_pb.Target_Definition_RuleTarget:
		ruleTarget := targetKind.RuleTarget
		ruleIdentifier, err := label.NewCanonicalStarlarkIdentifier(ruleTarget.RuleIdentifier)
		if err != nil {
			return PatchedConfiguredTargetValue{}, err
		}

		allBuiltinsModulesNames := e.GetBuiltinsModuleNamesValue(&model_analysis_pb.BuiltinsModuleNames_Key{})
		ruleValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
			Identifier: ruleIdentifier.String(),
		})
		if !allBuiltinsModulesNames.IsSet() || !ruleValue.IsSet() {
			return PatchedConfiguredTargetValue{}, evaluation.ErrMissingDependency
		}
		v, ok := ruleValue.Message.Global.GetKind().(*model_starlark_pb.Value_Rule)
		if !ok {
			return PatchedConfiguredTargetValue{}, fmt.Errorf("%#v is not a rule", ruleIdentifier.String())
		}
		d, ok := v.Rule.Kind.(*model_starlark_pb.Rule_Definition_)
		if !ok {
			return PatchedConfiguredTargetValue{}, fmt.Errorf("%#v is not a rule definition", ruleIdentifier.String())
		}
		ruleDefinition := model_core.Nested(ruleValue, d.Definition)

		thread := c.newStarlarkThread(ctx, e, allBuiltinsModulesNames.Message.BuiltinsModuleNames)

		// Set all common attrs.
		attrValues := make(map[string]any, len(ruleDefinition.Message.Attrs)+2)
		name := starlark.String(targetLabel.GetTargetName().String())
		attrValues["name"] = name

		tags := make([]starlark.Value, 0, len(ruleTarget.Tags))
		for _, tag := range ruleTarget.Tags {
			tags = append(tags, starlark.String(tag))
		}
		tagsList := starlark.NewList(tags)
		attrValues["tags"] = tagsList

		attrValues["testonly"] = starlark.Bool(ruleTarget.InheritableAttrs.GetTestonly())

		edgeTransitionAttrValues := make(map[string]any, len(ruleDefinition.Message.Attrs)+2)
		for k, v := range attrValues {
			edgeTransitionAttrValues[k] = v
		}

		// Obtain all attr values that don't depend on any
		// configuration, as these need to be provided to any
		// incoming edge transitions.
		ruleTargetPublicAttrValues := ruleTarget.PublicAttrValues
	GetConfigurationFreeAttrValues:
		for _, namedAttr := range ruleDefinition.Message.Attrs {
			var publicAttrValue *model_starlark_pb.RuleTarget_PublicAttrValue
			if !strings.HasPrefix(namedAttr.Name, "_") {
				if len(ruleTargetPublicAttrValues) == 0 {
					return PatchedConfiguredTargetValue{}, errors.New("rule target has fewer public attr values than the rule definition has public attrs")
				}
				publicAttrValue = ruleTargetPublicAttrValues[0]
				ruleTargetPublicAttrValues = ruleTargetPublicAttrValues[1:]
			}

			var valueParts []model_core.Message[*model_starlark_pb.Value, TReference]
			if !strings.HasPrefix(namedAttr.Name, "_") {
				// Attr is public. Extract the value
				// from the rule target.
				selectGroups := publicAttrValue.ValueParts
				if len(selectGroups) == 0 {
					return PatchedConfiguredTargetValue{}, fmt.Errorf("attr %#v has no select groups", namedAttr.Name)
				}
				for _, selectGroup := range selectGroups {
					if len(selectGroup.Conditions) > 0 {
						// Conditions are present, meaning the value
						// depends on a configuration.
						continue GetConfigurationFreeAttrValues
					}
					noMatch, ok := selectGroup.NoMatch.(*model_starlark_pb.Select_Group_NoMatchValue)
					if !ok {
						// No default value provided.
						continue GetConfigurationFreeAttrValues
					}
					valueParts = append(valueParts, model_core.Nested(targetValue, noMatch.NoMatchValue))
				}

				// If the value is None, fall back to the
				// default value from the rule definition.
				if len(valueParts) == 1 {
					if _, ok := valueParts[0].Message.Kind.(*model_starlark_pb.Value_None); ok {
						valueParts = valueParts[:0]
					}
				}
			}

			// No value provided. Use the default value from the
			// rule definition.
			if len(valueParts) == 0 {
				defaultValue := namedAttr.Attr.GetDefault()
				if defaultValue == nil {
					return PatchedConfiguredTargetValue{}, fmt.Errorf("missing value for mandatory attr %#v", namedAttr.Name)
				}
				valueParts = append(valueParts, model_core.Nested(ruleDefinition, defaultValue))
			}

			var attrValue starlark.Value
			for _, valuePart := range valueParts {
				decodedPart, err := model_starlark.DecodeValue[TReference, TMetadata](
					valuePart,
					/* currentIdentifier = */ nil,
					c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
						return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
					}),
				)
				if err != nil {
					return PatchedConfiguredTargetValue{}, err
				}
				if err := concatenateAttrValueParts(thread, &attrValue, decodedPart); err != nil {
					return PatchedConfiguredTargetValue{}, err
				}
			}
			attrValue.Freeze()

			switch namedAttr.Attr.GetType().(type) {
			case *model_starlark_pb.Attr_Label, *model_starlark_pb.Attr_LabelList, *model_starlark_pb.Attr_LabelKeyedStringDict,
				*model_starlark_pb.Attr_Output, *model_starlark_pb.Attr_OutputList:
				// Don't set these, as they depend on
				// the configuration.
			default:
				attrValues[namedAttr.Name] = attrValue
			}

			edgeTransitionAttrValues[namedAttr.Name] = attrValue
		}
		if l := len(ruleTargetPublicAttrValues); l != 0 {
			return PatchedConfiguredTargetValue{}, fmt.Errorf("rule target has %d more public attr values than the rule definition has public attrs", l)
		}

		// If provided, apply a user defined incoming edge transition.
		configurationReference := model_core.Nested(key, key.Message.ConfigurationReference)
		if cfgTransitionIdentifier := ruleDefinition.Message.CfgTransitionIdentifier; cfgTransitionIdentifier != "" {
			patchedConfigurationReferences, err := c.performUserDefinedTransitionCached(
				ctx,
				e,
				cfgTransitionIdentifier,
				configurationReference,
				model_starlark.NewStructFromDict[TReference, TMetadata](nil, edgeTransitionAttrValues),
			)
			if err != nil {
				return PatchedConfiguredTargetValue{}, err
			}

			entries := patchedConfigurationReferences.Message.Entries
			if l := len(entries); l != 1 {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("incoming edge transition %#v used by rule %#v is a 1:%d transition, while a 1:1 transition was expected", cfgTransitionIdentifier, ruleIdentifier.String(), l)
			}

			configurationReferences := model_core.Unpatch(e, patchedConfigurationReferences)
			configurationReference = model_core.Nested(configurationReferences, entries[0].OutputConfigurationReference)
		}

		// Compute non-label attrs that depend on a
		// configuration, due to them using select().
		missingDependencies := false
		outputsValues := map[string]any{}
		ruleTargetPublicAttrValues = ruleTarget.PublicAttrValues
		targetPackage := targetLabel.GetCanonicalPackage()
	GetNonLabelAttrValues:
		for _, namedAttr := range ruleDefinition.Message.Attrs {
			var publicAttrValue *model_starlark_pb.RuleTarget_PublicAttrValue
			if !strings.HasPrefix(namedAttr.Name, "_") {
				publicAttrValue = ruleTargetPublicAttrValues[0]
				ruleTargetPublicAttrValues = ruleTargetPublicAttrValues[1:]
			}
			if _, ok := attrValues[namedAttr.Name]; ok {
				// Attr was already computed previously.
				continue
			}

			switch namedAttr.Attr.GetType().(type) {
			case *model_starlark_pb.Attr_Label, *model_starlark_pb.Attr_LabelList, *model_starlark_pb.Attr_LabelKeyedStringDict:
				continue GetNonLabelAttrValues
			}

			valueParts, _, err := getAttrValueParts(
				e,
				configurationReference,
				targetPackage,
				model_core.Nested(ruleDefinition, namedAttr),
				model_core.Nested(targetValue, publicAttrValue),
			)
			if err != nil {
				if errors.Is(err, evaluation.ErrMissingDependency) {
					missingDependencies = true
					continue GetNonLabelAttrValues
				}
				return PatchedConfiguredTargetValue{}, err
			}

			var attrValue starlark.Value
			var attrOutputs []starlark.Value
			for _, valuePart := range valueParts.Message {
				decodedPart, err := model_starlark.DecodeValue[TReference, TMetadata](
					model_core.Nested(valueParts, valuePart),
					/* currentIdentifier = */ nil,
					c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
						switch namedAttr.Attr.GetType().(type) {
						case *model_starlark_pb.Attr_Output, *model_starlark_pb.Attr_OutputList:
							canonicalLabel, err := resolvedLabel.AsCanonical()
							if err != nil {
								return nil, err
							}
							canonicalPackage := canonicalLabel.GetCanonicalPackage()
							if canonicalPackage != targetPackage {
								return nil, fmt.Errorf("output attr %#v contains to label %#v, which refers to a different package", namedAttr.Name, canonicalLabel.String())
							}
							attrOutputs = append(attrOutputs, model_starlark.NewFile[TReference, TMetadata](&model_starlark_pb.File{
								Owner: &model_starlark_pb.File_Owner{
									// TODO: Fill in a proper hash.
									Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
									TargetName: targetLabel.GetTargetName().String(),
								},
								Package:             canonicalPackage.String(),
								PackageRelativePath: canonicalLabel.GetTargetName().String(),
								Type:                model_starlark_pb.File_FILE,
							}))
							return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
						default:
							return nil, fmt.Errorf("value of attr %#v contains labels, which is not expected for this type", namedAttr.Name)
						}
					}),
				)
				if err != nil {
					return PatchedConfiguredTargetValue{}, err
				}
				if err := concatenateAttrValueParts(thread, &attrValue, decodedPart); err != nil {
					return PatchedConfiguredTargetValue{}, err
				}
			}
			attrValue.Freeze()
			attrValues[namedAttr.Name] = attrValue
			edgeTransitionAttrValues[namedAttr.Name] = attrValue

			switch namedAttr.Attr.GetType().(type) {
			case *model_starlark_pb.Attr_Output:
				if len(attrOutputs) == 0 {
					outputsValues[namedAttr.Name] = starlark.None
				} else if len(attrOutputs) == 1 {
					outputsValues[namedAttr.Name] = attrOutputs[0]
				} else {
					return PatchedConfiguredTargetValue{}, fmt.Errorf("value of attr %#v contains multiple labels, which is not expected for attrs of type output", namedAttr.Name)
				}
			case *model_starlark_pb.Attr_OutputList:
				outputsValues[namedAttr.Name] = starlark.NewList(attrOutputs)
			}
		}

		// Resolve all toolchains and execution platforms.
		namedExecGroups := ruleDefinition.Message.ExecGroups
		execGroups := make([]ruleContextExecGroupState, 0, len(namedExecGroups))
		execGroupPlatformLabels := map[string]string{}
		for _, namedExecGroup := range namedExecGroups {
			execGroupDefinition := namedExecGroup.ExecGroup
			if execGroupDefinition == nil {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("missing definition of exec group %#v", namedExecGroup.Name)
			}
			execCompatibleWith, err := c.constraintValuesToConstraints(
				ctx,
				e,
				targetPackage,
				execGroupDefinition.ExecCompatibleWith,
			)
			if err != nil {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("invalid constraint values for exec group %#v: %w", namedExecGroup.Name)
			}
			patchedConfigurationReference := model_core.Patch(e, configurationReference)
			resolvedToolchains := e.GetResolvedToolchainsValue(
				model_core.NewPatchedMessage(
					&model_analysis_pb.ResolvedToolchains_Key{
						ExecCompatibleWith:     execCompatibleWith,
						ConfigurationReference: patchedConfigurationReference.Message,
						Toolchains:             execGroupDefinition.Toolchains,
					},
					model_core.MapReferenceMetadataToWalkers(patchedConfigurationReference.Patcher),
				),
			)
			if !resolvedToolchains.IsSet() {
				missingDependencies = true
				continue
			}
			toolchainIdentifiers := resolvedToolchains.Message.ToolchainIdentifiers
			if actual, expected := len(toolchainIdentifiers), len(execGroupDefinition.Toolchains); actual != expected {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("obtained %d resolved toolchains, while exec group %#v depends on %d toolchains", actual, namedExecGroup.Name, expected)
			}

			execGroups = append(execGroups, ruleContextExecGroupState{
				toolchainIdentifiers: toolchainIdentifiers,
				toolchainInfos:       make([]starlark.Value, len(toolchainIdentifiers)),
			})
			execGroupPlatformLabels[namedExecGroup.Name] = resolvedToolchains.Message.PlatformLabel
		}

		if missingDependencies {
			return PatchedConfiguredTargetValue{}, evaluation.ErrMissingDependency
		}

		// Last but not least, get the values of label attr.
		executableValues := map[string]any{}
		fileValues := map[string]any{}
		filesValues := map[string]any{}
		splitAttrValues := map[string]any{}
		ruleTargetPublicAttrValues = ruleTarget.PublicAttrValues
		edgeTransitionAttrValuesStruct := model_starlark.NewStructFromDict[TReference, TMetadata](nil, edgeTransitionAttrValues)
	GetLabelAttrValues:
		for _, namedAttr := range ruleDefinition.Message.Attrs {
			var publicAttrValue *model_starlark_pb.RuleTarget_PublicAttrValue
			if !strings.HasPrefix(namedAttr.Name, "_") {
				publicAttrValue = ruleTargetPublicAttrValues[0]
				ruleTargetPublicAttrValues = ruleTargetPublicAttrValues[1:]
			}
			if _, ok := attrValues[namedAttr.Name]; ok {
				// Attr was already computed previously.
				continue
			}

			isScalar := false
			var labelOptions *model_starlark_pb.Attr_LabelOptions
			allowSingleFile := false
			executable := false
			switch attrType := namedAttr.Attr.GetType().(type) {
			case *model_starlark_pb.Attr_Label:
				labelOptions = attrType.Label.ValueOptions
				isScalar = true
				allowSingleFile = attrType.Label.AllowSingleFile
				executable = attrType.Label.Executable
			case *model_starlark_pb.Attr_LabelKeyedStringDict:
				labelOptions = attrType.LabelKeyedStringDict.DictKeyOptions
			case *model_starlark_pb.Attr_LabelList:
				labelOptions = attrType.LabelList.ListValueOptions
			default:
				panic("only label attr types should be processed at this point")
			}
			if labelOptions == nil {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("attr %#v does not have label options", namedAttr.Name)
			}

			// Perform outgoing edge transition. User
			// defined transitions get access to all
			// non-label attr values.
			patchedTransition, mayHaveMultipleConfigurations, err := c.performTransition(
				ctx,
				e,
				labelOptions.Cfg,
				configurationReference,
				edgeTransitionAttrValuesStruct,
				execGroupPlatformLabels,
			)
			if err != nil {
				if errors.Is(err, evaluation.ErrMissingDependency) {
					missingDependencies = true
					continue GetLabelAttrValues
				}
				return PatchedConfiguredTargetValue{}, err
			}
			transition := model_core.Unpatch(e, patchedTransition)

			var attrValue starlark.Value
			var splitAttrValue *starlark.Dict
			if mayHaveMultipleConfigurations {
				splitAttrValue = starlark.NewDict(len(transition.Message.Entries))
			}

			var filesDepsetElements []any

			if len(transition.Message.Entries) == 0 {
				// We should leave targets unconfigured.
				// Perform select() without a configuration.
				valueParts, _, err := getAttrValueParts(
					e,
					model_core.NewSimpleMessage[TReference]((*model_core_pb.Reference)(nil)),
					targetPackage,
					model_core.Nested(ruleDefinition, namedAttr),
					model_core.Nested(targetValue, publicAttrValue),
				)
				if err != nil {
					if errors.Is(err, evaluation.ErrMissingDependency) {
						missingDependencies = true
						continue GetLabelAttrValues
					}
					return PatchedConfiguredTargetValue{}, err
				}

				// Provide a target reference that does
				// not contain any providers.
				for _, valuePart := range valueParts.Message {
					decodedPart, err := model_starlark.DecodeValue[TReference, TMetadata](
						model_core.Nested(valueParts, valuePart),
						/* currentIdentifier = */ nil,
						c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
							return model_starlark.NewTargetReference[TReference, TMetadata](
								resolvedLabel,
								model_core.NewSimpleMessage[TReference]([]*model_starlark_pb.Struct(nil)),
							), nil
						}),
					)
					if err != nil {
						return PatchedConfiguredTargetValue{}, err
					}
					if err := concatenateAttrValueParts(thread, &attrValue, decodedPart); err != nil {
						return PatchedConfiguredTargetValue{}, err
					}
				}
			} else {
				for _, transitionEntry := range transition.Message.Entries {
					outputConfigurationReference := model_core.Nested(transition, transitionEntry.OutputConfigurationReference)
					valueParts, usedDefaultValue, err := getAttrValueParts(
						e,
						outputConfigurationReference,
						targetPackage,
						model_core.Nested(ruleDefinition, namedAttr),
						model_core.Nested(targetValue, publicAttrValue),
					)
					if err != nil {
						if errors.Is(err, evaluation.ErrMissingDependency) {
							missingDependencies = true
							continue GetLabelAttrValues
						}
						return PatchedConfiguredTargetValue{}, err
					}

					// Whether an explicit value or a default attr
					// value is used determines how visibility is
					// computed. For explicit values, visibility is
					// computed relative to the package declaring
					// the target. For default values, the package
					// declaring the rule is used.
					var visibilityFromPackage label.CanonicalPackage
					if usedDefaultValue {
						visibilityFromPackage = ruleIdentifier.GetCanonicalLabel().GetCanonicalPackage()
					} else {
						visibilityFromPackage = targetPackage
					}

					var splitAttrEntry starlark.Value
					valueDecodingOptions := c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
						// Resolve the label.
						canonicalLabel, err := resolvedLabel.AsCanonical()
						if err != nil {
							return nil, err
						}
						patchedConfigurationReference1 := model_core.Patch(e, outputConfigurationReference)
						resolvedLabelValue := e.GetVisibleTargetValue(
							model_core.NewPatchedMessage(
								&model_analysis_pb.VisibleTarget_Key{
									FromPackage:            visibilityFromPackage.String(),
									ToLabel:                canonicalLabel.String(),
									ConfigurationReference: patchedConfigurationReference1.Message,
								},
								model_core.MapReferenceMetadataToWalkers(patchedConfigurationReference1.Patcher),
							),
						)
						if !resolvedLabelValue.IsSet() {
							missingDependencies = true
							return starlark.None, nil
						}
						if resolvedLabelStr := resolvedLabelValue.Message.Label; resolvedLabelStr != "" {
							canonicalLabel, err := label.NewCanonicalLabel(resolvedLabelStr)
							if err != nil {
								return nil, fmt.Errorf("invalid label %#v: %w", resolvedLabelStr, err)
							}

							// Obtain the providers of the target.
							patchedConfigurationReference2 := model_core.Patch(e, outputConfigurationReference)
							configuredTarget := e.GetConfiguredTargetValue(
								model_core.NewPatchedMessage(
									&model_analysis_pb.ConfiguredTarget_Key{
										Label:                  resolvedLabelStr,
										ConfigurationReference: patchedConfigurationReference2.Message,
									},
									model_core.MapReferenceMetadataToWalkers(patchedConfigurationReference2.Patcher),
								),
							)
							if !configuredTarget.IsSet() {
								missingDependencies = true
								return starlark.None, nil
							}
							providerInstances := model_core.Nested(configuredTarget, configuredTarget.Message.ProviderInstances)

							defaultInfoProviderIdentifierStr := defaultInfoProviderIdentifier.String()
							defaultInfoIndex, ok := sort.Find(
								len(providerInstances.Message),
								func(i int) int {
									return strings.Compare(defaultInfoProviderIdentifierStr, providerInstances.Message[i].ProviderInstanceProperties.GetProviderIdentifier())
								},
							)
							if !ok {
								return nil, fmt.Errorf("target with label %#v did not yield provider %#v", resolvedLabelStr, defaultInfoProviderIdentifierStr)
							}

							files, err := model_starlark.GetStructFieldValue(
								ctx,
								c.valueReaders.List,
								model_core.Nested(providerInstances, providerInstances.Message[defaultInfoIndex].Fields),
								"files",
							)
							if err != nil {
								return nil, fmt.Errorf("failed to obtain field \"files\" of DefaultInfo provider of target with label %#v: %w", resolvedLabelStr, err)
							}
							valueDepset, ok := files.Message.Kind.(*model_starlark_pb.Value_Depset)
							if !ok {
								return nil, fmt.Errorf("field \"files\" of DefaultInfo provider of target with label %#v is not a depset", resolvedLabelStr)
							}
							for _, element := range valueDepset.Depset.Elements {
								// TODO: Validate extensions.
								filesDepsetElements = append(filesDepsetElements, model_core.Nested(files, element))
							}

							if executable {
								filesToRun, err := model_starlark.GetStructFieldValue(
									ctx,
									c.valueReaders.List,
									model_core.Nested(providerInstances, providerInstances.Message[defaultInfoIndex].Fields),
									"files_to_run",
								)
								if err != nil {
									return nil, fmt.Errorf("failed to obtain field \"files\" of DefaultInfo provider of target with label %#v: %w", resolvedLabelStr, err)
								}
								filesToRunStruct, ok := filesToRun.Message.Kind.(*model_starlark_pb.Value_Struct)
								if !ok {
									return nil, fmt.Errorf("field \"files_to_run\" of DefaultInfo provider of target with label %#v is not a struct", resolvedLabelStr)
								}
								executableField, err := model_starlark.GetStructFieldValue(
									ctx,
									c.valueReaders.List,
									model_core.Nested(filesToRun, filesToRunStruct.Struct.Fields),
									"executable",
								)
								if err != nil {
									return nil, fmt.Errorf("failed to obtain field \"files_to_run.executable\" of DefaultInfo provider of target with label %#v: %w", resolvedLabelStr, err)
								}
								decodedExecutable, err := model_starlark.DecodeValue[TReference, TMetadata](
									executableField,
									/* currentIdentifier = */ nil,
									c.getValueDecodingOptions(ctx, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
										return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
									}),
								)
								if err != nil {
									return nil, fmt.Errorf("decode field \"files_to_run.executable\" of DefaultInfo provider of target with label %#v: %w", resolvedLabelStr, err)
								}
								executableValues[namedAttr.Name] = decodedExecutable
							}

							return model_starlark.NewTargetReference[TReference, TMetadata](canonicalLabel.AsResolved(), providerInstances), nil
						} else {
							return starlark.None, nil
						}
					})
					for _, valuePart := range valueParts.Message {
						decodedPart, err := model_starlark.DecodeValue[TReference, TMetadata](
							model_core.Nested(valueParts, valuePart),
							/* currentIdentifier = */ nil,
							valueDecodingOptions,
						)
						if err != nil {
							return PatchedConfiguredTargetValue{}, fmt.Errorf("decode value part: %w", err)
						}
						if isScalar && mayHaveMultipleConfigurations {
							decodedPart = starlark.NewList([]starlark.Value{decodedPart})
						}
						if err := concatenateAttrValueParts(thread, &attrValue, decodedPart); err != nil {
							return PatchedConfiguredTargetValue{}, fmt.Errorf("concatenate attr value parts: %w", err)
						}
						if mayHaveMultipleConfigurations {
							if err := concatenateAttrValueParts(thread, &splitAttrEntry, decodedPart); err != nil {
								return PatchedConfiguredTargetValue{}, fmt.Errorf("concatenate split attr value parts: %w", err)
							}
						}
					}

					if mayHaveMultipleConfigurations {
						if err := splitAttrValue.SetKey(thread, starlark.String(transitionEntry.Key), splitAttrEntry); err != nil {
							return PatchedConfiguredTargetValue{}, err
						}
					}
				}
			}
			if !missingDependencies {
				attrValue.Freeze()
				attrValues[namedAttr.Name] = attrValue

				if mayHaveMultipleConfigurations {
					splitAttrValue.Freeze()
					splitAttrValues[namedAttr.Name] = splitAttrValue
				}

				filesDepset := model_starlark.NewDepsetFromList[TReference, TMetadata](filesDepsetElements, model_starlark_pb.Depset_DEFAULT)
				files, err := filesDepset.ToList(thread)
				if err != nil {
					return PatchedConfiguredTargetValue{}, fmt.Errorf("converting files depset to list: %w", err)
				}
				files.Freeze()
				if allowSingleFile {
					switch l := files.Len(); l {
					case 0:
						fileValues[namedAttr.Name] = starlark.None
					case 1:
						fileValues[namedAttr.Name] = files.Index(0)
					default:
						return PatchedConfiguredTargetValue{}, fmt.Errorf("attr %#v has allow_single_file=True, but its value expands to %d targets", namedAttr.Name, l)
					}
				} else {
					filesValues[namedAttr.Name] = files
				}
			}
		}
		if missingDependencies {
			return PatchedConfiguredTargetValue{}, evaluation.ErrMissingDependency
		}

		rc := &ruleContext[TReference, TMetadata]{
			computer:               c,
			context:                ctx,
			environment:            e,
			ruleIdentifier:         ruleIdentifier,
			targetLabel:            targetLabel,
			configurationReference: configurationReference,
			ruleDefinition:         ruleDefinition,
			ruleTarget:             model_core.Nested(targetValue, ruleTarget),
			attr:                   model_starlark.NewStructFromDict[TReference, TMetadata](nil, attrValues),
			splitAttr:              model_starlark.NewStructFromDict[TReference, TMetadata](nil, splitAttrValues),
			executable:             model_starlark.NewStructFromDict[TReference, TMetadata](nil, executableValues),
			file:                   model_starlark.NewStructFromDict[TReference, TMetadata](nil, fileValues),
			files:                  model_starlark.NewStructFromDict[TReference, TMetadata](nil, filesValues),
			outputs:                model_starlark.NewStructFromDict[TReference, TMetadata](nil, outputsValues),
			execGroups:             execGroups,
			fragments:              map[string]*model_starlark.Struct[TReference, TMetadata]{},
		}
		thread.SetLocal(model_starlark.CurrentCtxKey, rc)

		thread.SetLocal(model_starlark.SubruleInvokerKey, func(subruleIdentifier label.CanonicalStarlarkIdentifier, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			// TODO: Subrules are allowed to be nested. Keep a stack!
			permittedSubruleIdentifiers := ruleDefinition.Message.SubruleIdentifiers

			subruleIdentifierStr := subruleIdentifier.String()
			if _, ok := sort.Find(
				len(permittedSubruleIdentifiers),
				func(i int) int { return strings.Compare(subruleIdentifierStr, permittedSubruleIdentifiers[i]) },
			); !ok {
				return nil, fmt.Errorf("subrule %#v cannot be invoked from within the current (sub)rule", subruleIdentifierStr)
			}
			subruleValue := e.GetCompiledBzlFileGlobalValue(&model_analysis_pb.CompiledBzlFileGlobal_Key{
				Identifier: subruleIdentifierStr,
			})
			if !subruleValue.IsSet() {
				return nil, evaluation.ErrMissingDependency
			}
			v, ok := subruleValue.Message.Global.GetKind().(*model_starlark_pb.Value_Subrule)
			if !ok {
				return nil, fmt.Errorf("%#v is not a subrule", subruleIdentifierStr)
			}
			d, ok := v.Subrule.Kind.(*model_starlark_pb.Subrule_Definition_)
			if !ok {
				return nil, fmt.Errorf("%#v is not a subrule definition", subruleIdentifierStr)
			}
			subruleDefinition := model_core.Nested(subruleValue, d.Definition)

			missingDependencies := false

			implementationArgs := append(
				starlark.Tuple{
					&subruleContext[TReference, TMetadata]{ruleContext: rc},
				},
				args...,
			)
			implementationKwargs := append(
				make([]starlark.Tuple, 0, len(kwargs)+len(subruleDefinition.Message.Attrs)),
				kwargs...,
			)
			for _, namedAttr := range subruleDefinition.Message.Attrs {
				defaultValue := namedAttr.Attr.GetDefault()
				if defaultValue == nil {
					return nil, fmt.Errorf("missing value for mandatory attr %#v", namedAttr.Name)
				}
				// TODO: Is this using the correct configuration?
				value, err := rc.computer.configureAttrValueParts(
					rc.context,
					rc.environment,
					thread,
					namedAttr,
					model_core.Nested(rc.ruleDefinition, []*model_starlark_pb.Value{defaultValue}),
					rc.configurationReference,
					rc.ruleIdentifier.GetCanonicalLabel().GetCanonicalPackage(),
					execGroupPlatformLabels,
				)
				if err != nil {
					if errors.Is(err, evaluation.ErrMissingDependency) {
						missingDependencies = true
						continue
					}
					return nil, err
				}
				implementationKwargs = append(
					implementationKwargs,
					starlark.Tuple{
						starlark.String(namedAttr.Name),
						value,
					},
				)
			}

			if missingDependencies {
				return nil, evaluation.ErrMissingDependency
			}

			return starlark.Call(
				thread,
				model_starlark.NewNamedFunction(
					model_starlark.NewProtoNamedFunctionDefinition[TReference, TMetadata](
						model_core.Nested(subruleDefinition, subruleDefinition.Message.Implementation),
					),
				),
				implementationArgs,
				implementationKwargs,
			)
		})

		returnValue, err := starlark.Call(
			thread,
			model_starlark.NewNamedFunction(
				model_starlark.NewProtoNamedFunctionDefinition[TReference, TMetadata](
					model_core.Nested(ruleDefinition, ruleDefinition.Message.Implementation),
				),
			),
			/* args = */ starlark.Tuple{rc},
			/* kwargs = */ nil,
		)
		if err != nil {
			if !errors.Is(err, evaluation.ErrMissingDependency) {
				var evalErr *starlark.EvalError
				if errors.As(err, &evalErr) {
					return PatchedConfiguredTargetValue{}, errors.New(evalErr.Backtrace())
				}
			}
			return PatchedConfiguredTargetValue{}, err
		}

		// Bazel permits returning either a single provider, or
		// a list of providers.
		var providerInstances []*model_starlark.Struct[TReference, TMetadata]
		structUnpackerInto := unpack.Type[*model_starlark.Struct[TReference, TMetadata]]("struct")
		if err := unpack.IfNotNone(
			unpack.Or([]unpack.UnpackerInto[[]*model_starlark.Struct[TReference, TMetadata]]{
				unpack.Singleton(structUnpackerInto),
				unpack.List(structUnpackerInto),
			}),
		).UnpackInto(thread, returnValue, &providerInstances); err != nil {
			return PatchedConfiguredTargetValue{}, fmt.Errorf("failed to unpack implementation function return value: %w", err)
		}

		// Convert list of providers to a map where the provider
		// identifier is the key.
		providerInstancesByIdentifier := make(map[label.CanonicalStarlarkIdentifier]*model_starlark.Struct[TReference, TMetadata], len(providerInstances))
		for i, providerInstance := range providerInstances {
			providerIdentifier, err := providerInstance.GetProviderIdentifier()
			if err != nil {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("struct returned at index %d: %w", i, err)
			}
			if _, ok := providerInstancesByIdentifier[providerIdentifier]; ok {
				return PatchedConfiguredTargetValue{}, fmt.Errorf("implementation function returned multiple structs for provider %#v", providerIdentifier.String())
			}
			providerInstancesByIdentifier[providerIdentifier] = providerInstance
		}

		if defaultInfo, ok := providerInstancesByIdentifier[defaultInfoProviderIdentifier]; ok {
			// Rule returned DefaultInfo. Make sure that
			// "data_runfiles", "default_runfiles" and
			// "files" are not set to None.
			//
			// Ideally we'd do this as part of DefaultInfo's
			// init function, but runfiles objects can only
			// be constructed via ctx.
			attrNames := defaultInfo.AttrNames()
			newAttrs := make(map[string]any, len(attrNames))
			for _, attrName := range attrNames {
				attrValue, err := defaultInfo.Attr(thread, attrName)
				if err != nil {
					return PatchedConfiguredTargetValue{}, err
				}
				switch attrName {
				case "data_runfiles", "default_runfiles":
					if attrValue == starlark.None {
						attrValue = model_starlark.NewRunfiles(
							model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
							model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
							model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
						)
					}
				case "files":
					if attrValue == starlark.None {
						attrValue = model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT)
					}
				}
				newAttrs[attrName] = attrValue
			}
			providerInstancesByIdentifier[defaultInfoProviderIdentifier] = model_starlark.NewStructFromDict[TReference, TMetadata](defaultInfoProviderInstanceProperties, newAttrs)
		} else {
			// Rule did not return DefaultInfo. Return an
			// empty one.
			providerInstancesByIdentifier[defaultInfoProviderIdentifier] = model_starlark.NewStructFromDict[TReference, TMetadata](
				defaultInfoProviderInstanceProperties,
				map[string]any{
					"data_runfiles": model_starlark.NewRunfiles[TReference](
						model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
						model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
						model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
					),
					"default_runfiles": model_starlark.NewRunfiles[TReference](
						model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
						model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
						model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
					),
					"files": model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT),
					"files_to_run": model_starlark.NewStructFromDict[TReference, TMetadata](
						model_starlark.NewProviderInstanceProperties(&filesToRunProviderIdentifier, false),
						map[string]any{
							"executable":            starlark.None,
							"repo_mapping_manifest": starlark.None,
							"runfiles_manifest":     starlark.None,
						},
					),
				},
			)
		}

		encodedProviderInstances := make([]*model_starlark_pb.Struct, 0, len(providerInstancesByIdentifier))
		patcher := model_core.NewReferenceMessagePatcher[TMetadata]()
		for _, providerIdentifier := range slices.SortedFunc(
			maps.Keys(providerInstancesByIdentifier),
			func(a, b label.CanonicalStarlarkIdentifier) int {
				return strings.Compare(a.String(), b.String())
			},
		) {
			v, _, err := providerInstancesByIdentifier[providerIdentifier].
				Encode(map[starlark.Value]struct{}{}, c.getValueEncodingOptions(e, nil))
			if err != nil {
				return PatchedConfiguredTargetValue{}, err
			}
			encodedProviderInstances = append(encodedProviderInstances, v.Message)
			patcher.Merge(v.Patcher)
		}

		return model_core.NewPatchedMessage(
			&model_analysis_pb.ConfiguredTarget_Value{
				ProviderInstances: encodedProviderInstances,
			},
			model_core.MapReferenceMetadataToWalkers(patcher),
		), nil
	case *model_starlark_pb.Target_Definition_SourceFileTarget:
		// Handcraft a DefaultInfo provider for this source file.
		return getSingleFileConfiguredTargetValue(&model_starlark_pb.File{
			Package:             targetLabel.GetCanonicalPackage().String(),
			PackageRelativePath: targetLabel.GetTargetName().String(),
			Type:                model_starlark_pb.File_FILE,
		}), nil
	default:
		return PatchedConfiguredTargetValue{}, errors.New("only source file targets and rule targets can be configured")
	}
}

type ruleContext[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	computer               *baseComputer[TReference, TMetadata]
	context                context.Context
	environment            ConfiguredTargetEnvironment[TReference, TMetadata]
	ruleIdentifier         label.CanonicalStarlarkIdentifier
	targetLabel            label.CanonicalLabel
	configurationReference model_core.Message[*model_core_pb.Reference, TReference]
	ruleDefinition         model_core.Message[*model_starlark_pb.Rule_Definition, TReference]
	ruleTarget             model_core.Message[*model_starlark_pb.RuleTarget, TReference]
	attr                   starlark.Value
	splitAttr              starlark.Value
	buildSettingValue      starlark.Value
	executable             starlark.Value
	file                   starlark.Value
	files                  starlark.Value
	outputs                starlark.Value
	execGroups             []ruleContextExecGroupState
	tags                   *starlark.List
	varDict                *starlark.Dict
	fragments              map[string]*model_starlark.Struct[TReference, TMetadata]
}

var _ starlark.HasAttrs = (*ruleContext[object.GlobalReference, BaseComputerReferenceMetadata])(nil)

func (rc *ruleContext[TReference, TMetadata]) String() string {
	return fmt.Sprintf("<ctx for %s>", rc.targetLabel.String())
}

func (ruleContext[TReference, TMetadata]) Type() string {
	return "ctx"
}

func (ruleContext[TReference, TMetadata]) Freeze() {
}

func (ruleContext[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (ruleContext[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("ctx cannot be hashed")
}

func (rc *ruleContext[TReference, TMetadata]) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	switch name {
	case "actions":
		return &ruleContextActions[TReference, TMetadata]{
			ruleContext: rc,
		}, nil
	case "attr":
		return rc.attr, nil
	case "build_setting_value":
		if rc.buildSettingValue == nil {
			buildSettingDefault := rc.ruleTarget.Message.BuildSettingDefault
			if buildSettingDefault == nil {
				return nil, errors.New("rule is not a build setting")
			}

			configuration, err := model_parser.MaybeDereference(rc.context, rc.computer.configurationReader, rc.configurationReference)
			if err != nil {
				return nil, err
			}

			targetLabelStr := rc.targetLabel.String()
			override, err := btree.Find(
				rc.context,
				rc.computer.configurationBuildSettingOverrideReader,
				model_core.Nested(configuration, configuration.Message.GetBuildSettingOverrides()),
				func(entry *model_analysis_pb.Configuration_BuildSettingOverride) (int, *model_core_pb.Reference) {
					switch level := entry.Level.(type) {
					case *model_analysis_pb.Configuration_BuildSettingOverride_Leaf_:
						return strings.Compare(targetLabelStr, level.Leaf.Label), nil
					case *model_analysis_pb.Configuration_BuildSettingOverride_Parent_:
						return strings.Compare(targetLabelStr, level.Parent.FirstLabel), level.Parent.Reference
					default:
						return 0, nil
					}
				},
			)
			if err != nil {
				return nil, err
			}

			var encodedValue model_core.Message[*model_starlark_pb.Value, TReference]
			if override.IsSet() {
				overrideLeaf, ok := override.Message.Level.(*model_analysis_pb.Configuration_BuildSettingOverride_Leaf_)
				if !ok {
					return nil, errors.New("build setting override is not a valid leaf")
				}
				encodedValue = model_core.Nested(override, overrideLeaf.Leaf.Value)
			} else {
				encodedValue = model_core.Nested(rc.ruleTarget, rc.ruleTarget.Message.BuildSettingDefault)
			}

			value, err := model_starlark.DecodeValue[TReference, TMetadata](
				encodedValue,
				/* currentIdentifier = */ nil,
				rc.computer.getValueDecodingOptions(rc.context, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
					return nil, errors.New("did not expect label values")
				}),
			)
			if err != nil {
				return nil, err
			}
			rc.buildSettingValue = value
		}
		return rc.buildSettingValue, nil
	case "configuration":
		// Implement ctx.configuration as if it is a specially
		// named fragment.
		return rc.getFragment("configuration")
	case "coverage_instrumented":
		return starlark.NewBuiltin("ctx.coverage_instrumented", rc.doCoverageInstrumented), nil
	case "bin_dir":
		return model_starlark.NewStructFromDict[TReference, TMetadata](nil, map[string]any{
			// TODO: Fill in the right configuration in the path.
			"path": starlark.String("bazel-bin/TODO-CONFIGURATION/bin"),
		}), nil
	case "disabled_features":
		return starlark.NewList(nil), nil
	case "exec_groups":
		return &ruleContextExecGroups[TReference, TMetadata]{
			ruleContext: rc,
		}, nil
	case "executable":
		return rc.executable, nil
	case "expand_location":
		return starlark.NewBuiltin("ctx.expand_location", rc.doExpandLocation), nil
	case "features":
		// TODO: Do we want to support ctx.features in a meaningful way?
		return starlark.NewList(nil), nil
	case "file":
		return rc.file, nil
	case "files":
		return rc.files, nil
	case "fragments":
		return &ruleContextFragments[TReference, TMetadata]{
			ruleContext: rc,
		}, nil
	case "info_file":
		// Fill all of this in properly.
		return model_starlark.NewFile[TReference, TMetadata](&model_starlark_pb.File{
			Owner: &model_starlark_pb.File_Owner{
				Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
				TargetName: "stamp",
			},
			Package:             "@@builtins_core+",
			PackageRelativePath: "stable-status.txt",
			Type:                model_starlark_pb.File_FILE,
		}), nil
	case "label":
		return model_starlark.NewLabel[TReference, TMetadata](rc.targetLabel.AsResolved()), nil
	case "outputs":
		return rc.outputs, nil
	case "runfiles":
		return starlark.NewBuiltin("ctx.runfiles", rc.doRunfiles), nil
	case "split_attr":
		return rc.splitAttr, nil
	case "target_platform_has_constraint":
		return starlark.NewBuiltin("ctx.target_platform_has_constraint", rc.doTargetPlatformHasConstraint), nil
	case "toolchains":
		execGroups := rc.ruleDefinition.Message.ExecGroups
		execGroupIndex, ok := sort.Find(
			len(execGroups),
			func(i int) int { return strings.Compare("", execGroups[i].Name) },
		)
		if !ok {
			return nil, errors.New("rule does not have a default exec group")
		}
		return &toolchainContext[TReference, TMetadata]{
			ruleContext:    rc,
			execGroupIndex: execGroupIndex,
		}, nil
	case "var":
		if rc.varDict == nil {
			configurationReference := model_core.Patch(
				rc.environment,
				rc.configurationReference,
			)
			makeVariables := rc.environment.GetMakeVariablesValue(
				model_core.NewPatchedMessage(
					&model_analysis_pb.MakeVariables_Key{
						FromPackage:            rc.targetLabel.GetCanonicalPackage().String(),
						ConfigurationReference: configurationReference.Message,
						Toolchains:             rc.ruleTarget.Message.Toolchains,
					},
					model_core.MapReferenceMetadataToWalkers(configurationReference.Patcher),
				),
			)
			if !makeVariables.IsSet() {
				return nil, evaluation.ErrMissingDependency
			}

			d := starlark.NewDict(len(makeVariables.Message.Variables))
			for _, variable := range makeVariables.Message.Variables {
				if err := d.SetKey(thread, starlark.String(variable.Key), starlark.String(variable.Value)); err != nil {
					return nil, err
				}
			}
			d.Freeze()
			rc.varDict = d
		}
		return rc.varDict, nil
	case "version_file":
		// Fill all of this in properly.
		return model_starlark.NewFile[TReference, TMetadata](&model_starlark_pb.File{
			Owner: &model_starlark_pb.File_Owner{
				Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
				TargetName: "stamp",
			},
			Package:             "@@builtins_core+",
			PackageRelativePath: "volatile-status.txt",
			Type:                model_starlark_pb.File_FILE,
		}), nil
	case "workspace_name":
		return starlark.String("_main"), nil
	default:
		return nil, nil
	}
}

func (rc *ruleContext[TReference, TMetadata]) getFragment(name string) (starlark.Value, error) {
	fragmentInfo, ok := rc.fragments[name]
	if !ok {
		targetName, err := label.NewTargetName(name)
		if err != nil {
			return nil, fmt.Errorf("invalid target name %#v: %w", name, err)
		}
		encodedFragmentInfo, err := getProviderFromConfiguredTarget(
			rc.environment,
			fragmentsPackage.AppendTargetName(targetName).String(),
			model_core.Patch(
				rc.environment,
				rc.configurationReference,
			),
			fragmentInfoProviderIdentifier,
		)
		if err != nil {
			return nil, err
		}
		fragmentInfo, err = model_starlark.DecodeStruct[TReference, TMetadata](
			model_core.Nested(encodedFragmentInfo, &model_starlark_pb.Struct{
				ProviderInstanceProperties: &model_starlark_pb.Provider_InstanceProperties{
					ProviderIdentifier: fragmentInfoProviderIdentifier.String(),
				},
				Fields: encodedFragmentInfo.Message,
			}),
			rc.computer.getValueDecodingOptions(rc.context, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
				return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
			}),
		)
		if err != nil {
			return nil, err
		}
		rc.fragments[name] = fragmentInfo
	}
	return fragmentInfo, nil
}

var ruleContextAttrNames = []string{
	"actions",
	"attr",
	"build_setting_value",
	"exec_groups",
	"executable",
	"features",
	"file",
	"files",
	"fragments",
	"info_file",
	"label",
	"runfiles",
	"split_attr",
	"toolchains",
	"var",
	"version_file",
	"workspace_name",
}

func (ruleContext[TReference, TMetadata]) AttrNames() []string {
	return ruleContextAttrNames
}

func (ruleContext[TReference, TMetadata]) doCoverageInstrumented(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return starlark.False, nil
}

func (ruleContext[TReference, TMetadata]) doExpandLocation(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var input string
	var targets []*model_starlark.TargetReference[TReference, TMetadata]
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"input", unpack.Bind(thread, &input, unpack.String),
		"targets?", unpack.Bind(thread, &targets, unpack.List(unpack.Type[*model_starlark.TargetReference[TReference, TMetadata]]("Target"))),
	); err != nil {
		return nil, err
	}

	// TODO: Actually expand $(location) tags.
	return starlark.String(input), nil
}

func toSymlinkEntryDepset[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata](v any) *model_starlark.Depset[TReference, TMetadata] {
	switch typedV := v.(type) {
	case *model_starlark.Depset[TReference, TMetadata]:
		return typedV
	case map[string]string:
		panic("TODO: convert dict of strings to SymlinkEntry list")
	case nil:
		return model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT)
	default:
		panic("unknown type")
	}
}

func (ruleContext[TReference, TMetadata]) doRunfiles(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var files []starlark.Value
	var transitiveFiles *model_starlark.Depset[TReference, TMetadata]
	var symlinks any
	var rootSymlinks any
	symlinksUnpackerInto := unpack.Or([]unpack.UnpackerInto[any]{
		unpack.Decay(unpack.Dict(unpack.String, unpack.String)),
		unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset")),
	})
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"files?", unpack.Bind(thread, &files, unpack.List(unpack.Canonicalize(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")))),
		"transitive_files?", unpack.Bind(thread, &transitiveFiles, unpack.IfNotNone(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset"))),
		"symlinks?", unpack.Bind(thread, &symlinks, symlinksUnpackerInto),
		"root_symlinks?", unpack.Bind(thread, &rootSymlinks, symlinksUnpackerInto),
	); err != nil {
		return nil, err
	}

	if transitiveFiles == nil {
		transitiveFiles = model_starlark.NewDepsetFromList[TReference, TMetadata](nil, model_starlark_pb.Depset_DEFAULT)
	}
	filesDepset, err := model_starlark.NewDepset(
		thread,
		files,
		[]*model_starlark.Depset[TReference, TMetadata]{transitiveFiles},
		model_starlark_pb.Depset_DEFAULT,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create files depset: %w", err)
	}

	return model_starlark.NewRunfiles(
		filesDepset,
		toSymlinkEntryDepset[TReference, TMetadata](rootSymlinks),
		toSymlinkEntryDepset[TReference, TMetadata](symlinks),
	), nil
}

func (rc *ruleContext[TReference, TMetadata]) doTargetPlatformHasConstraint(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want 1", b.Name(), len(args))
	}
	var constraintValue *model_starlark.Struct[TReference, TMetadata]
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"constraintValue", unpack.Bind(thread, &constraintValue, unpack.Type[*model_starlark.Struct[TReference, TMetadata]]("struct")),
	); err != nil {
		return nil, err
	}

	labelUnpackerInto := model_starlark.NewLabelOrStringUnpackerInto[TReference, TMetadata](model_starlark.CurrentFilePackage(thread, 1))

	// Obtain the label of the provided constraint value.
	constraintValueLabelValue, err := constraintValue.Attr(thread, "label")
	if err != nil {
		return nil, errors.New("\"label\" attribute of constraint value")
	}
	var constraintValueLabel label.ResolvedLabel
	if err := labelUnpackerInto.UnpackInto(thread, constraintValueLabelValue, &constraintValueLabel); err != nil {
		return nil, errors.New("\"label\" attribute of constraint value")
	}

	// Obtain the label of the constraint setting.
	constraintSetting, err := constraintValue.Attr(thread, "constraint")
	if err != nil {
		return nil, err
	}
	constraintSettingAttrs, ok := constraintSetting.(starlark.HasAttrs)
	if !ok {
		return nil, errors.New("\"constraint\" attribute of constraint value is not a struct")
	}
	constraintSettingLabelValue, err := constraintSettingAttrs.Attr(thread, "label")
	if err != nil {
		return nil, fmt.Errorf("\"constraint.label\" attribute of constraint value: %w", err)
	}
	var constraintSettingLabel label.ResolvedLabel
	if err := labelUnpackerInto.UnpackInto(thread, constraintSettingLabelValue, &constraintSettingLabel); err != nil {
		return nil, fmt.Errorf("\"constraint.label\" attribute of constraint value; %w", err)
	}

	// Obtain constraints of the target platform.
	platformInfoProvider, err := getTargetPlatformInfoProvider(rc.environment, rc.configurationReference)
	if err != nil {
		return nil, err
	}
	platformConstraints, err := model_starlark.GetStructFieldValue(rc.context, rc.computer.valueReaders.List, platformInfoProvider, "constraints")
	if err != nil {
		return nil, err
	}
	platformConstraintsDict, ok := platformConstraints.Message.GetKind().(*model_starlark_pb.Value_Dict)
	if !ok {
		return nil, errors.New("\"constraints\" attribute of target platform's PlatformInfo is not a dict")
	}

	// Check whether the provided constraint setting is present on
	// the target platform. If so, check whether the provided
	// constraint value matches.
	constraintSettingLabelStr := constraintSettingLabel.String()
	var errIter error
	for platformConstraintSetting, platformConstraintValue := range model_starlark.AllDictLeafEntries(
		rc.context,
		rc.computer.valueReaders.Dict,
		model_core.Nested(platformConstraints, platformConstraintsDict.Dict),
		&errIter,
	) {
		platformConstraintSettingLabel, ok := platformConstraintSetting.Message.GetKind().(*model_starlark_pb.Value_Label)
		if !ok {
			return nil, errors.New("key in \"constraints\" attribute of target platform's PlatformInfo dict is not a label")
		}
		if platformConstraintSettingLabel.Label == constraintSettingLabelStr {
			platformConstraintValueLabel, ok := platformConstraintValue.Message.GetKind().(*model_starlark_pb.Value_Label)
			if !ok {
				return nil, fmt.Errorf("value of \"constraints\" attribute %#v of target platform's PlatformInfo dict is not a label", platformConstraintValueLabel.Label)
			}
			return starlark.Bool(platformConstraintValueLabel.Label == constraintValueLabel.String()), nil
		}
	}
	if errIter != nil {
		return nil, fmt.Errorf("failed to iterate platform constraints: %w", errIter)
	}

	// Target platform does not contain the constraint setting.
	// Check whether the constraint value is the constraint
	// setting's default value.
	defaultConstraintValue, err := constraintSettingAttrs.Attr(thread, "default_constraint_value")
	if err != nil {
		return nil, fmt.Errorf("\"constraint.default_constraint_value\" attribute of constraint value: %w", err)
	}
	var defaultConstraintValueLabel *label.ResolvedLabel
	if err := unpack.IfNotNone(unpack.Pointer(labelUnpackerInto)).UnpackInto(thread, defaultConstraintValue, &defaultConstraintValueLabel); err != nil {
		return nil, fmt.Errorf("\"constraint.default_constraint_value\" attribute of constraint value; %w", err)
	}
	return starlark.Bool(defaultConstraintValueLabel != nil && *defaultConstraintValueLabel == constraintValueLabel), nil
}

type ruleContextActions[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	ruleContext *ruleContext[TReference, TMetadata]
}

var _ starlark.HasAttrs = (*ruleContextActions[object.GlobalReference, BaseComputerReferenceMetadata])(nil)

func (ruleContextActions[TReference, TMetadata]) String() string {
	return "<ctx.actions>"
}

func (ruleContextActions[TReference, TMetadata]) Type() string {
	return "ctx.actions"
}

func (ruleContextActions[TReference, TMetadata]) Freeze() {
}

func (ruleContextActions[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (ruleContextActions[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("ctx.actions cannot be hashed")
}

func (rca *ruleContextActions[TReference, TMetadata]) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	switch name {
	case "args":
		return starlark.NewBuiltin("ctx.actions.args", rca.doArgs), nil
	case "declare_directory":
		return starlark.NewBuiltin("ctx.actions.declare_directory", rca.doDeclareDirectory), nil
	case "declare_file":
		return starlark.NewBuiltin("ctx.actions.declare_file", rca.doDeclareFile), nil
	case "declare_symlink":
		return starlark.NewBuiltin("ctx.actions.declare_symlink", rca.doDeclareSymlink), nil
	case "expand_template":
		return starlark.NewBuiltin("ctx.actions.expand_template", rca.doExpandTemplate), nil
	case "run":
		return starlark.NewBuiltin("ctx.actions.run", rca.doRun), nil
	case "run_shell":
		return starlark.NewBuiltin("ctx.actions.run_shell", rca.doRunShell), nil
	case "symlink":
		return starlark.NewBuiltin("ctx.actions.symlink", rca.doSymlink), nil
	case "transform_info_file":
		return starlark.NewBuiltin("ctx.actions.transform_info_file", rca.doTransformInfoFile), nil
	case "transform_version_file":
		return starlark.NewBuiltin("ctx.actions.transform_version_file", rca.doTransformVersionFile), nil
	case "write":
		return starlark.NewBuiltin("ctx.actions.write", rca.doWrite), nil
	default:
		return nil, nil
	}
}

var ruleContextActionsAttrNames = []string{
	"args",
	"declare_directory",
	"declare_file",
	"run",
	"run_shell",
	"symlink",
	"transform_info_file",
	"transform_version_file",
	"write",
}

func (rca *ruleContextActions[TReference, TMetadata]) AttrNames() []string {
	return ruleContextActionsAttrNames
}

func (rca *ruleContextActions[TReference, TMetadata]) doArgs(thread *starlark.Thread, b *starlark.Builtin, arguments starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return &args{}, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doDeclareDirectory(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 1 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want at most 1", b.Name(), len(args))
	}
	var filename label.TargetName
	var sibling *model_starlark.File[TReference, TMetadata]
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"filename", unpack.Bind(thread, &filename, unpack.TargetName),
		"sibling?", unpack.Bind(thread, &sibling, unpack.IfNotNone(unpack.Pointer(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")))),
	); err != nil {
		return nil, err
	}

	rc := rca.ruleContext
	return model_starlark.NewFile[TReference, TMetadata](&model_starlark_pb.File{
		Owner: &model_starlark_pb.File_Owner{
			// TODO: Fill in a proper hash.
			Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
			TargetName: rc.targetLabel.GetTargetName().String(),
		},
		Package:             rc.targetLabel.GetCanonicalPackage().String(),
		PackageRelativePath: filename.String(),
		Type:                model_starlark_pb.File_DIRECTORY,
	}), nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doDeclareFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 1 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want at most 1", b.Name(), len(args))
	}
	var filename label.TargetName
	var sibling *model_starlark.File[TReference, TMetadata]
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"filename", unpack.Bind(thread, &filename, unpack.TargetName),
		"sibling?", unpack.Bind(thread, &sibling, unpack.IfNotNone(unpack.Pointer(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")))),
	); err != nil {
		return nil, err
	}

	rc := rca.ruleContext
	return model_starlark.NewFile[TReference, TMetadata](&model_starlark_pb.File{
		Owner: &model_starlark_pb.File_Owner{
			// TODO: Fill in a proper hash.
			Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
			TargetName: rc.targetLabel.GetTargetName().String(),
		},
		Package:             rc.targetLabel.GetCanonicalPackage().String(),
		PackageRelativePath: filename.String(),
		Type:                model_starlark_pb.File_FILE,
	}), nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doDeclareSymlink(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) > 1 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want at most 1", b.Name(), len(args))
	}
	var filename label.TargetName
	var sibling *model_starlark.File[TReference, TMetadata]
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"filename", unpack.Bind(thread, &filename, unpack.TargetName),
		"sibling?", unpack.Bind(thread, &sibling, unpack.IfNotNone(unpack.Pointer(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")))),
	); err != nil {
		return nil, err
	}

	rc := rca.ruleContext
	return model_starlark.NewFile[TReference, TMetadata](&model_starlark_pb.File{
		Owner: &model_starlark_pb.File_Owner{
			// TODO: Fill in a proper hash.
			Cfg:        []byte{0xbe, 0x8a, 0x60, 0x1c, 0xe3, 0x03, 0x44, 0xf0},
			TargetName: rc.targetLabel.GetTargetName().String(),
		},
		Package:             rc.targetLabel.GetCanonicalPackage().String(),
		PackageRelativePath: filename.String(),
		Type:                model_starlark_pb.File_SYMLINK,
	}), nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doExpandTemplate(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 0 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want 0", b.Name(), len(args))
	}
	var output model_starlark.File[TReference, TMetadata]
	var template model_starlark.File[TReference, TMetadata]
	isExecutable := false
	var substitutions map[string]string
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		// Required arguments.
		"output", unpack.Bind(thread, &output, unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
		"template", unpack.Bind(thread, &template, unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
		// Optional arguments.
		// TODO: Add TemplateDict and computed_substitutions.
		"is_executable?", unpack.Bind(thread, &isExecutable, unpack.Bool),
		"substitutions?", unpack.Bind(thread, &substitutions, unpack.Dict(unpack.String, unpack.String)),
	); err != nil {
		return nil, err
	}

	return starlark.None, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doRun(thread *starlark.Thread, b *starlark.Builtin, fnArgs starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(fnArgs) != 0 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want 0", b.Name(), len(fnArgs))
	}
	var executable any
	var outputs []model_starlark.File[TReference, TMetadata]
	var arguments []any
	var env map[string]string
	execGroup := ""
	var executionRequirements map[string]string
	var inputs any
	mnemonic := ""
	var progressMessage string
	var resourceSet *model_starlark.NamedFunction[TReference, TMetadata]
	var toolchain *label.ResolvedLabel
	var tools []any
	useDefaultShellEnv := false
	if err := starlark.UnpackArgs(
		b.Name(), fnArgs, kwargs,
		// Required arguments.
		"executable", unpack.Bind(thread, &executable, unpack.Or([]unpack.UnpackerInto[any]{
			unpack.Decay(unpack.String),
			unpack.Decay(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
			unpack.Decay(unpack.Type[*model_starlark.Struct[TReference, TMetadata]]("struct")),
		})),
		"outputs", unpack.Bind(thread, &outputs, unpack.List(unpack.Type[model_starlark.File[TReference, TMetadata]]("File"))),
		// Optional arguments.
		"arguments?", unpack.Bind(thread, &arguments, unpack.List(unpack.Or([]unpack.UnpackerInto[any]{
			unpack.Decay(unpack.Type[*args]("Args")),
			unpack.Decay(unpack.String),
		}))),
		"env?", unpack.Bind(thread, &env, unpack.Dict(unpack.String, unpack.String)),
		"exec_group?", unpack.Bind(thread, &execGroup, unpack.IfNotNone(unpack.String)),
		"execution_requirements?", unpack.Bind(thread, &executionRequirements, unpack.Dict(unpack.String, unpack.String)),
		"inputs?", unpack.Bind(thread, &inputs, unpack.Or([]unpack.UnpackerInto[any]{
			unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset")),
			unpack.Decay(unpack.List(unpack.Type[model_starlark.File[TReference, TMetadata]]("File"))),
		})),
		"mnemonic?", unpack.Bind(thread, &mnemonic, unpack.IfNotNone(unpack.String)),
		"progress_message?", unpack.Bind(thread, &progressMessage, unpack.IfNotNone(unpack.String)),
		"resource_set?", unpack.Bind(thread, &resourceSet, unpack.IfNotNone(unpack.Pointer(model_starlark.NewNamedFunctionUnpackerInto[TReference, TMetadata]()))),
		"toolchain?", unpack.Bind(thread, &toolchain, unpack.IfNotNone(unpack.Pointer(model_starlark.NewLabelOrStringUnpackerInto[TReference, TMetadata](model_starlark.CurrentFilePackage(thread, 1))))),
		"tools?", unpack.Bind(thread, &tools, unpack.Or([]unpack.UnpackerInto[[]any]{
			unpack.Singleton(unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset"))),
			unpack.List(unpack.Or([]unpack.UnpackerInto[any]{
				unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset")),
				unpack.Decay(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
				unpack.Decay(unpack.Type[*model_starlark.Struct[TReference, TMetadata]]("struct")),
			})),
		})),
		"use_default_shell_env?", unpack.Bind(thread, &useDefaultShellEnv, unpack.Bool),
	); err != nil {
		return nil, err
	}

	return starlark.None, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doRunShell(thread *starlark.Thread, b *starlark.Builtin, fnArgs starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(fnArgs) != 0 {
		return nil, fmt.Errorf("%s: got %d positional arguments, want 0", b.Name(), len(fnArgs))
	}
	var command string
	var outputs []model_starlark.File[TReference, TMetadata]
	var arguments []any
	var env map[string]string
	execGroup := ""
	var executionRequirements map[string]string
	var inputs any
	mnemonic := ""
	progressMessage := ""
	var resourceSet *model_starlark.NamedFunction[TReference, TMetadata]
	var toolchain *label.ResolvedLabel
	var tools []any
	useDefaultShellEnv := false
	if err := starlark.UnpackArgs(
		b.Name(), fnArgs, kwargs,
		// Required arguments.
		"outputs", unpack.Bind(thread, &outputs, unpack.List(unpack.Type[model_starlark.File[TReference, TMetadata]]("File"))),
		"command", unpack.Bind(thread, &command, unpack.String),
		// Optional arguments.
		"arguments?", unpack.Bind(thread, &arguments, unpack.List(unpack.Or([]unpack.UnpackerInto[any]{
			unpack.Decay(unpack.Type[*args]("Args")),
			unpack.Decay(unpack.String),
		}))),
		"env?", unpack.Bind(thread, &env, unpack.Dict(unpack.String, unpack.String)),
		"exec_group?", unpack.Bind(thread, &execGroup, unpack.IfNotNone(unpack.String)),
		"execution_requirements?", unpack.Bind(thread, &executionRequirements, unpack.Dict(unpack.String, unpack.String)),
		"inputs?", unpack.Bind(thread, &inputs, unpack.Or([]unpack.UnpackerInto[any]{
			unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset")),
			unpack.Decay(unpack.List(unpack.Type[model_starlark.File[TReference, TMetadata]]("File"))),
		})),
		"mnemonic?", unpack.Bind(thread, &mnemonic, unpack.IfNotNone(unpack.String)),
		"progress_message?", unpack.Bind(thread, &progressMessage, unpack.IfNotNone(unpack.String)),
		"resource_set?", unpack.Bind(thread, &resourceSet, unpack.IfNotNone(unpack.Pointer(model_starlark.NewNamedFunctionUnpackerInto[TReference, TMetadata]()))),
		"toolchain?", unpack.Bind(thread, &toolchain, unpack.IfNotNone(unpack.Pointer(model_starlark.NewLabelOrStringUnpackerInto[TReference, TMetadata](model_starlark.CurrentFilePackage(thread, 1))))),
		"tools?", unpack.Bind(thread, &tools, unpack.Or([]unpack.UnpackerInto[[]any]{
			unpack.Singleton(unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset"))),
			unpack.List(unpack.Or([]unpack.UnpackerInto[any]{
				unpack.Decay(unpack.Type[*model_starlark.Depset[TReference, TMetadata]]("depset")),
				unpack.Decay(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
				unpack.Decay(unpack.Type[*model_starlark.Struct[TReference, TMetadata]]("struct")),
			})),
		})),
		"use_default_shell_env?", unpack.Bind(thread, &useDefaultShellEnv, unpack.Bool),
	); err != nil {
		return nil, err
	}

	// TODO: Actually register the action.
	return starlark.None, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doSymlink(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var output model_starlark.File[TReference, TMetadata]
	var targetFile *model_starlark.File[TReference, TMetadata]
	targetPath := ""
	isExecutable := false
	progressMessage := ""
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"output", unpack.Bind(thread, &output, unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
		"target_file?", unpack.Bind(thread, &targetFile, unpack.IfNotNone(unpack.Pointer(unpack.Type[model_starlark.File[TReference, TMetadata]]("File")))),
		"target_path?", unpack.Bind(thread, &targetPath, unpack.IfNotNone(unpack.String)),
		"is_executable?", unpack.Bind(thread, &isExecutable, unpack.Bool),
		"progress_message?", unpack.Bind(thread, &progressMessage, unpack.IfNotNone(unpack.String)),
	); err != nil {
		return nil, err
	}

	// TODO: Actually register the action.
	return starlark.None, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doTransformInfoFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return starlark.None, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doTransformVersionFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return starlark.None, nil
}

func (rca *ruleContextActions[TReference, TMetadata]) doWrite(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var output model_starlark.File[TReference, TMetadata]
	var content string
	isExecutable := false
	if err := starlark.UnpackArgs(
		b.Name(), args, kwargs,
		"output", unpack.Bind(thread, &output, unpack.Type[model_starlark.File[TReference, TMetadata]]("File")),
		// TODO: Accept Args.
		"content", unpack.Bind(thread, &content, unpack.String),
		"is_executable?", unpack.Bind(thread, &isExecutable, unpack.Bool),
	); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

type ruleContextExecGroups[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	ruleContext *ruleContext[TReference, TMetadata]
}

var _ starlark.Mapping = (*ruleContextExecGroups[object.GlobalReference, BaseComputerReferenceMetadata])(nil)

func (ruleContextExecGroups[TReference, TMetadata]) String() string {
	return "<ctx.exec_groups>"
}

func (ruleContextExecGroups[TReference, TMetadata]) Type() string {
	return "ctx.exec_groups"
}

func (ruleContextExecGroups[TReference, TMetadata]) Freeze() {
}

func (ruleContextExecGroups[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (ruleContextExecGroups[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("ctx.exec_groups cannot be hashed")
}

func (rca *ruleContextExecGroups[TReference, TMetadata]) Get(thread *starlark.Thread, key starlark.Value) (starlark.Value, bool, error) {
	var execGroupName string
	if err := unpack.String.UnpackInto(thread, key, &execGroupName); err != nil {
		return nil, false, err
	}

	rc := rca.ruleContext
	execGroups := rc.ruleDefinition.Message.ExecGroups
	execGroupIndex, ok := sort.Find(
		len(execGroups),
		func(i int) int { return strings.Compare(execGroupName, execGroups[i].Name) },
	)
	if !ok {
		return nil, false, fmt.Errorf("rule does not have an exec group with name %#v", execGroupName)
	}

	return nil, true, fmt.Errorf("TODO: use exec group with index %d", execGroupIndex)
}

type ruleContextFragments[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	ruleContext *ruleContext[TReference, TMetadata]
}

var _ starlark.HasAttrs = (*ruleContextFragments[object.GlobalReference, BaseComputerReferenceMetadata])(nil)

func (ruleContextFragments[TReference, TMetadata]) String() string {
	return "<ctx.fragments>"
}

func (ruleContextFragments[TReference, TMetadata]) Type() string {
	return "ctx.fragments"
}

func (ruleContextFragments[TReference, TMetadata]) Freeze() {
}

func (ruleContextFragments[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (ruleContextFragments[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("ctx.fragments cannot be hashed")
}

func (rcf *ruleContextFragments[TReference, TMetadata]) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	return rcf.ruleContext.getFragment(name)
}

func (ruleContextFragments[TReference, TMetadata]) AttrNames() []string {
	// TODO: implement.
	return nil
}

type toolchainContext[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	ruleContext    *ruleContext[TReference, TMetadata]
	execGroupIndex int
}

var _ starlark.Mapping = (*toolchainContext[object.GlobalReference, BaseComputerReferenceMetadata])(nil)

func (toolchainContext[TReference, TMetadata]) String() string {
	return "<toolchain context>"
}

func (toolchainContext[TReference, TMetadata]) Type() string {
	return "ToolchainContext"
}

func (toolchainContext[TReference, TMetadata]) Freeze() {
}

func (toolchainContext[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (toolchainContext[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("ToolchainContext cannot be hashed")
}

func (tc *toolchainContext[TReference, TMetadata]) Get(thread *starlark.Thread, v starlark.Value) (starlark.Value, bool, error) {
	rc := tc.ruleContext
	labelUnpackerInto := unpack.Stringer(model_starlark.NewLabelOrStringUnpackerInto[TReference, TMetadata](model_starlark.CurrentFilePackage(thread, 0)))
	var toolchainType string
	if err := labelUnpackerInto.UnpackInto(thread, v, &toolchainType); err != nil {
		return nil, false, err
	}

	namedExecGroup := rc.ruleDefinition.Message.ExecGroups[tc.execGroupIndex]
	execGroupDefinition := namedExecGroup.ExecGroup
	if execGroupDefinition == nil {
		return nil, false, errors.New("rule definition lacks exec group definition")
	}

	toolchains := execGroupDefinition.Toolchains
	toolchainIndex, ok := sort.Find(
		len(toolchains),
		func(i int) int { return strings.Compare(toolchainType, toolchains[i].ToolchainType) },
	)
	if !ok {
		return nil, false, fmt.Errorf("exec group %#v does not depend on toolchain type %#v", namedExecGroup.Name, toolchainType)
	}

	execGroup := &rc.execGroups[tc.execGroupIndex]
	toolchainInfo := execGroup.toolchainInfos[toolchainIndex]
	if toolchainInfo == nil {
		toolchainIdentifier := execGroup.toolchainIdentifiers[toolchainIndex]
		if toolchainIdentifier == "" {
			// Toolchain was optional, and no matching
			// toolchain was found.
			toolchainInfo = starlark.None
		} else {
			encodedToolchainInfo, err := getProviderFromConfiguredTarget(
				rc.environment,
				toolchainIdentifier,
				model_core.Patch(
					rc.environment,
					rc.configurationReference,
				),
				toolchainInfoProviderIdentifier,
			)
			if err != nil {
				return nil, true, err
			}

			toolchainInfo, err = model_starlark.DecodeStruct[TReference, TMetadata](
				model_core.Nested(encodedToolchainInfo, &model_starlark_pb.Struct{
					ProviderInstanceProperties: &model_starlark_pb.Provider_InstanceProperties{
						ProviderIdentifier: toolchainInfoProviderIdentifier.String(),
					},
					Fields: encodedToolchainInfo.Message,
				}),
				rc.computer.getValueDecodingOptions(rc.context, func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
					return model_starlark.NewLabel[TReference, TMetadata](resolvedLabel), nil
				}),
			)
			if err != nil {
				return nil, true, err
			}
		}
		execGroup.toolchainInfos[toolchainIndex] = toolchainInfo
	}
	return toolchainInfo, true, nil
}

type ruleContextExecGroupState struct {
	toolchainIdentifiers []string
	toolchainInfos       []starlark.Value
}

type getProviderFromConfiguredTargetEnvironment[TReference any] interface {
	GetConfiguredTargetValue(key model_core.PatchedMessage[*model_analysis_pb.ConfiguredTarget_Key, dag.ObjectContentsWalker]) model_core.Message[*model_analysis_pb.ConfiguredTarget_Value, TReference]
}

// getProviderFromConfiguredTarget looks up a single provider that is
// provided by a configured target.
func getProviderFromConfiguredTarget[TReference any, TMetadata model_core.WalkableReferenceMetadata](e getProviderFromConfiguredTargetEnvironment[TReference], targetLabel string, configurationReference model_core.PatchedMessage[*model_core_pb.Reference, TMetadata], providerIdentifier label.CanonicalStarlarkIdentifier) (model_core.Message[*model_starlark_pb.Struct_Fields, TReference], error) {
	configuredTargetValue := e.GetConfiguredTargetValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.ConfiguredTarget_Key{
				Label:                  targetLabel,
				ConfigurationReference: configurationReference.Message,
			},
			model_core.MapReferenceMetadataToWalkers(configurationReference.Patcher),
		),
	)
	if !configuredTargetValue.IsSet() {
		return model_core.Message[*model_starlark_pb.Struct_Fields, TReference]{}, evaluation.ErrMissingDependency
	}

	providerIdentifierStr := providerIdentifier.String()
	providerInstances := configuredTargetValue.Message.ProviderInstances
	if providerIndex, ok := sort.Find(
		len(providerInstances),
		func(i int) int {
			return strings.Compare(providerIdentifierStr, providerInstances[i].ProviderInstanceProperties.GetProviderIdentifier())
		},
	); ok {
		return model_core.Nested(configuredTargetValue, providerInstances[providerIndex].Fields), nil
	}
	return model_core.Message[*model_starlark_pb.Struct_Fields, TReference]{}, fmt.Errorf("target did not yield provider %#v", providerIdentifierStr)
}

type getProviderFromVisibleConfiguredTargetEnvironment[TReference any] interface {
	GetConfiguredTargetValue(model_core.PatchedMessage[*model_analysis_pb.ConfiguredTarget_Key, dag.ObjectContentsWalker]) model_core.Message[*model_analysis_pb.ConfiguredTarget_Value, TReference]
	GetVisibleTargetValue(model_core.PatchedMessage[*model_analysis_pb.VisibleTarget_Key, dag.ObjectContentsWalker]) model_core.Message[*model_analysis_pb.VisibleTarget_Value, TReference]
}

func getProviderFromVisibleConfiguredTarget[TReference any, TConfigurationReference object.BasicReference, TMetadata model_core.WalkableReferenceMetadata](
	e getProviderFromVisibleConfiguredTargetEnvironment[TReference],
	fromPackage string,
	targetLabel string,
	configurationReference model_core.Message[*model_core_pb.Reference, TConfigurationReference],
	configurationObjectCapturer model_core.ExistingObjectCapturer[TConfigurationReference, TMetadata],
	providerIdentifier label.CanonicalStarlarkIdentifier,
) (model_core.Message[*model_starlark_pb.Struct_Fields, TReference], string, error) {
	patchedConfigurationReference := model_core.Patch(
		configurationObjectCapturer,
		configurationReference,
	)
	visibleTarget := e.GetVisibleTargetValue(
		model_core.NewPatchedMessage(
			&model_analysis_pb.VisibleTarget_Key{
				FromPackage:            fromPackage,
				ToLabel:                targetLabel,
				ConfigurationReference: patchedConfigurationReference.Message,
			},
			model_core.MapReferenceMetadataToWalkers(patchedConfigurationReference.Patcher),
		),
	)
	if !visibleTarget.IsSet() {
		return model_core.Message[*model_starlark_pb.Struct_Fields, TReference]{}, "", evaluation.ErrMissingDependency
	}
	p, err := getProviderFromConfiguredTarget(
		e,
		visibleTarget.Message.Label,
		model_core.Patch(
			configurationObjectCapturer,
			configurationReference,
		),
		providerIdentifier,
	)
	return p, visibleTarget.Message.Label, err
}

type args struct{}

var _ starlark.HasAttrs = (*args)(nil)

func (args) String() string {
	return "<Args>"
}

func (args) Type() string {
	return "Args"
}

func (args) Freeze() {}

func (args) Truth() starlark.Bool {
	return starlark.True
}

func (args) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("Args cannot be hashed")
}

func (a *args) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	switch name {
	case "add":
		return starlark.NewBuiltin("Args.add", a.doAdd), nil
	case "add_all":
		return starlark.NewBuiltin("Args.add_all", a.doAddAll), nil
	case "add_joined":
		return starlark.NewBuiltin("Args.add_joined", a.doAddJoined), nil
	case "set_param_file_format":
		return starlark.NewBuiltin("Args.set_param_file_format", a.doSetParamFileFormat), nil
	case "use_param_file":
		return starlark.NewBuiltin("Args.use_param_file", a.doUseParamFile), nil
	default:
		return nil, nil
	}
}

var argsAttrNames = []string{
	"add",
	"add_all",
	"add_joined",
	"set_param_file_format",
	"use_param_file",
}

func (args) AttrNames() []string {
	return argsAttrNames
}

func (a *args) doAdd(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return a, nil
}

func (a *args) doAddAll(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return a, nil
}

func (a *args) doAddJoined(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return a, nil
}

func (a *args) doSetParamFileFormat(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return a, nil
}

func (a *args) doUseParamFile(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	return a, nil
}

type subruleContext[TReference object.BasicReference, TMetadata BaseComputerReferenceMetadata] struct {
	ruleContext *ruleContext[TReference, TMetadata]
}

var _ starlark.HasAttrs = (*subruleContext[object.GlobalReference, BaseComputerReferenceMetadata])(nil)

func (sc *subruleContext[TReference, TMetadata]) String() string {
	rc := sc.ruleContext
	return fmt.Sprintf("<subrule_ctx for %s>", rc.targetLabel.String())
}

func (subruleContext[TReference, TMetadata]) Type() string {
	return "subrule_ctx"
}

func (subruleContext[TReference, TMetadata]) Freeze() {
}

func (subruleContext[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

func (subruleContext[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("subrule_ctx cannot be hashed")
}

func (sc *subruleContext[TReference, TMetadata]) Attr(thread *starlark.Thread, name string) (starlark.Value, error) {
	rc := sc.ruleContext
	switch name {
	case "fragments":
		return &ruleContextFragments[TReference, TMetadata]{
			ruleContext: rc,
		}, nil
	default:
		return nil, nil
	}
}

var subruleContextAttrNames = []string{
	"fragments",
}

func (subruleContext[TReference, TMetadata]) AttrNames() []string {
	return subruleContextAttrNames
}
