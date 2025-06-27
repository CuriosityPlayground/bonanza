package build

import (
	"errors"
	"fmt"
	"net/url"

	"bonanza.build/pkg/label"
	pg_starlark "bonanza.build/pkg/starlark"

	"github.com/buildbarn/bb-storage/pkg/filesystem/path"

	"go.starlark.net/starlark"
)

// LocalPathExtractingModuleDotBazelHandler is capable of capturing the
// paths contained in local_path_override() directives of a MODULE.bazel
// file. These paths are needed by the client to determine which
// directories to upload to the server to perform the build.
type LocalPathExtractingModuleDotBazelHandler struct {
	modulePaths    map[label.Module]path.Parser
	rootModulePath path.Parser
	rootModuleName *label.Module
}

func NewLocalPathExtractingModuleDotBazelHandler(modulePaths map[label.Module]path.Parser, rootModulePath path.Parser) *LocalPathExtractingModuleDotBazelHandler {
	return &LocalPathExtractingModuleDotBazelHandler{
		modulePaths:    modulePaths,
		rootModulePath: rootModulePath,
	}
}

func (h *LocalPathExtractingModuleDotBazelHandler) GetRootModuleName() (label.Module, error) {
	if h.rootModuleName == nil {
		var badModule label.Module
		return badModule, errors.New("MODULE.bazel of root module does not contain a module() declaration")
	}
	return *h.rootModuleName, nil
}

func (LocalPathExtractingModuleDotBazelHandler) BazelDep(name label.Module, version *label.ModuleVersion, maxCompatibilityLevel int, repoName label.ApparentRepo, devDependency bool) error {
	return nil
}

func (h *LocalPathExtractingModuleDotBazelHandler) LocalPathOverride(moduleName label.Module, path path.Parser) error {
	if _, ok := h.modulePaths[moduleName]; ok {
		return fmt.Errorf("multiple local_path_override() or module() declarations for module with name %#v", moduleName.String())
	}
	h.modulePaths[moduleName] = path
	return nil
}

func (h *LocalPathExtractingModuleDotBazelHandler) Module(name label.Module, version *label.ModuleVersion, compatibilityLevel int, repoName label.ApparentRepo, bazelCompatibility []string) error {
	if h.rootModuleName != nil {
		return errors.New("multiple module() declarations")
	}
	h.rootModuleName = &name
	return h.LocalPathOverride(name, h.rootModulePath)
}

func (LocalPathExtractingModuleDotBazelHandler) MultipleVersionOverride(moduleName label.Module, versions []label.ModuleVersion, registry *url.URL) error {
	return nil
}

func (LocalPathExtractingModuleDotBazelHandler) RegisterExecutionPlatforms(platformTargetPatterns []label.ApparentTargetPattern, devDependency bool) error {
	return nil
}

func (LocalPathExtractingModuleDotBazelHandler) RegisterToolchains(toolchainTargetPatterns []label.ApparentTargetPattern, devDependency bool) error {
	return nil
}

func (LocalPathExtractingModuleDotBazelHandler) RepositoryRuleOverride(moduleName label.Module, repositoryRuleIdentifier label.CanonicalStarlarkIdentifier, attrs map[string]starlark.Value) error {
	return nil
}

func (LocalPathExtractingModuleDotBazelHandler) SingleVersionOverride(moduleName label.Module, version *label.ModuleVersion, registry *url.URL, patchOptions *pg_starlark.PatchOptions) error {
	return nil
}

func (LocalPathExtractingModuleDotBazelHandler) UseExtension(extensionBzlFile label.ApparentLabel, extensionName label.StarlarkIdentifier, devDependency, isolate bool) (pg_starlark.ModuleExtensionProxy, error) {
	return pg_starlark.NullModuleExtensionProxy, nil
}

func (LocalPathExtractingModuleDotBazelHandler) UseRepoRule(repoRuleBzlFile label.ApparentLabel, repoRuleName label.StarlarkIdentifier) (pg_starlark.RepoRuleProxy, error) {
	return func(name label.ApparentRepo, devDependency bool, attrs map[string]starlark.Value) error {
		return nil
	}, nil
}
