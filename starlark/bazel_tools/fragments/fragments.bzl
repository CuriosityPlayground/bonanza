load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")

FragmentInfo = provider()

def _apple_fragment_impl(ctx):
    return [FragmentInfo(
        include_xcode_exec_requirements = ctx.attr._include_xcode_exec_requirements[BuildSettingInfo].value,
        ios_minimum_os_flag = ctx.attr._ios_minimum_os[BuildSettingInfo].value,
        ios_sdk_version_flag = ctx.attr._ios_sdk_version[BuildSettingInfo].value,
        macos_minimum_os_flag = ctx.attr._macos_minimum_os[BuildSettingInfo].value,
        macos_sdk_version_flag = ctx.attr._macos_sdk_version[BuildSettingInfo].value,
        prefer_mutual_xcode = ctx.attr._prefer_mutual_xcode[BuildSettingInfo].value,
        single_arch_platform = struct(
            platform_type = ctx.attr._apple_platform_type[BuildSettingInfo].value,
        ),
        tvos_minimum_os_flag = ctx.attr._tvos_minimum_os[BuildSettingInfo].value,
        tvos_sdk_version_flag = ctx.attr._tvos_sdk_version[BuildSettingInfo].value,
        watchos_minimum_os_flag = ctx.attr._watchos_minimum_os[BuildSettingInfo].value,
        watchos_sdk_version_flag = ctx.attr._watchos_sdk_version[BuildSettingInfo].value,
        xcode_version_flag = ctx.attr._xcode_version[BuildSettingInfo].value,
    )]

apple_fragment = rule(
    _apple_fragment_impl,
    attrs = {
        "_apple_platform_type": attr.label(default = "//command_line_option:apple_platform_type"),
        "_include_xcode_exec_requirements": attr.label(default = "//command_line_option:experimental_include_xcode_exec_requirements"),
        "_ios_minimum_os": attr.label(default = "//command_line_option:ios_minimum_os"),
        "_ios_sdk_version": attr.label(default = "//command_line_option:ios_sdk_version"),
        "_macos_minimum_os": attr.label(default = "//command_line_option:macos_minimum_os"),
        "_macos_sdk_version": attr.label(default = "//command_line_option:macos_sdk_version"),
        "_prefer_mutual_xcode": attr.label(default = "//command_line_option:experimental_prefer_mutual_xcode"),
        "_tvos_minimum_os": attr.label(default = "//command_line_option:tvos_minimum_os"),
        "_tvos_sdk_version": attr.label(default = "//command_line_option:tvos_sdk_version"),
        "_watchos_minimum_os": attr.label(default = "//command_line_option:watchos_minimum_os"),
        "_watchos_sdk_version": attr.label(default = "//command_line_option:watchos_sdk_version"),
        "_xcode_version": attr.label(default = "//command_line_option:xcode_version"),
    },
)

def _cpp_fragment_impl(ctx):
    compilation_mode = ctx.attr._compilation_mode[BuildSettingInfo].value
    dynamic_mode = ctx.attr._dynamic_mode[BuildSettingInfo].value.upper()
    experimental_cc_implementation_deps = ctx.attr._cc_implementation_deps[BuildSettingInfo].value
    fission = ctx.attr._fission[BuildSettingInfo].value
    fission_active_for_current_compilation_mode = (
        True if fission == "yes" else False if fission == "no" else compilation_mode in fission.split(",")
    )
    force_pic = ctx.attr._force_pic[BuildSettingInfo].value
    generate_llvm_lcov = ctx.attr._generate_llvm_lcov[BuildSettingInfo].value
    grte_top = ctx.attr._grte_top.label if ctx.attr._grte_top else None
    minimum_os_version = ctx.attr._minimum_os_version[BuildSettingInfo].value
    process_headers_in_dependencies = ctx.attr._process_headers_in_dependencies[BuildSettingInfo].value
    save_feature_state = ctx.attr._save_feature_state[BuildSettingInfo].value
    strip = ctx.attr._strip[BuildSettingInfo].value
    stripopt = ctx.attr._strip[BuildSettingInfo].value
    should_strip_binaries = strip == "always" or (strip == "sometimes" and compilation_mode == "fastbuild")
    use_specific_tool_files = ctx.attr._use_specific_tool_files[BuildSettingInfo].value
    return [FragmentInfo(
        compilation_mode = lambda: compilation_mode,
        conlyopts = ctx.attr._conlyopt[BuildSettingInfo].value,
        copts = ctx.attr._copt[BuildSettingInfo].value,
        custom_malloc = ctx.attr._custom_malloc[BuildSettingInfo].value if ctx.attr._custom_malloc else None,
        cxxopts = ctx.attr._cxxopt[BuildSettingInfo].value,
        do_not_use_macos_set_install_name = ctx.attr._macos_set_install_name[BuildSettingInfo].value,
        dynamic_mode = lambda: dynamic_mode,
        experimental_cc_implementation_deps = lambda: experimental_cc_implementation_deps,
        experimental_starlark_linking = lambda: True,
        fission_active_for_current_compilation_mode = lambda: fission_active_for_current_compilation_mode,
        force_pic = lambda: force_pic,
        generate_llvm_lcov = lambda: generate_llvm_lcov,
        grte_top = lambda: grte_top,
        linkopts = ctx.attr._linkopt[BuildSettingInfo].value,
        minimum_os_version = lambda: minimum_os_version,
        process_headers_in_dependencies = lambda: process_headers_in_dependencies,
        save_feature_state = lambda: save_feature_state,
        should_strip_binaries = lambda: should_strip_binaries,
        strip_opts = lambda: stripopt,
        incompatible_use_specific_tool_files = lambda: use_specific_tool_files,
    )]

cpp_fragment = rule(
    _cpp_fragment_impl,
    attrs = {
        "_cc_implementation_deps": attr.label(default = "//command_line_option:experimental_cc_implementation_deps"),
        "_compilation_mode": attr.label(default = "//command_line_option:compilation_mode"),
        "_conlyopt": attr.label(default = "//command_line_option:conlyopt"),
        "_copt": attr.label(default = "//command_line_option:copt"),
        "_custom_malloc": attr.label(default = "//command_line_option:custom_malloc"),
        "_cxxopt": attr.label(default = "//command_line_option:cxxopt"),
        "_dynamic_mode": attr.label(default = "//command_line_option:dynamic_mode"),
        "_fission": attr.label(default = "//command_line_option:fission"),
        "_force_pic": attr.label(default = "//command_line_option:force_pic"),
        "_generate_llvm_lcov": attr.label(default = "//command_line_option:experimental_generate_llvm_lcov"),
        "_grte_top": attr.label(default = "//command_line_option:grte_top"),
        "_linkopt": attr.label(default = "//command_line_option:linkopt"),
        "_process_headers_in_dependencies": attr.label(default = "//command_line_option:process_headers_in_dependencies"),
        "_macos_set_install_name": attr.label(default = "//command_line_option:incompatible_macos_set_install_name"),
        "_minimum_os_version": attr.label(default = "//command_line_option:minimum_os_version"),
        "_save_feature_state": attr.label(default = "//command_line_option:experimental_save_feature_state"),
        "_strip": attr.label(default = "//command_line_option:strip"),
        "_stripopt": attr.label(default = "//command_line_option:stripopt"),
        "_use_specific_tool_files": attr.label(default = "//command_line_option:incompatible_use_specific_tool_files"),
    },
)

def _java_fragment_impl(ctx):
    disallow_java_import_empty_jars = ctx.attr._disallow_java_import_empty_jars[BuildSettingInfo].value
    disallow_java_import_exports = ctx.attr._disallow_java_import_exports[BuildSettingInfo].value
    use_ijars = ctx.attr._use_ijars[BuildSettingInfo].value
    return [FragmentInfo(
        disallow_java_import_empty_jars = lambda: disallow_java_import_empty_jars,
        disallow_java_import_exports = lambda: disallow_java_import_exports,
        use_ijars = lambda: use_ijars,
    )]

java_fragment = rule(
    _java_fragment_impl,
    attrs = {
        "_disallow_java_import_empty_jars": attr.label(default = "//command_line_option:incompatible_disallow_java_import_empty_jars"),
        "_disallow_java_import_exports": attr.label(default = "//command_line_option:incompatible_disallow_java_import_exports"),
        "_use_ijars": attr.label(default = "//command_line_option:use_ijars"),
    },
)

def _platform_fragment_impl(ctx):
    return [FragmentInfo(
        host_platform = ctx.attr._host_platform.label,
        platform = ctx.attr._platform.label,
    )]

platform_fragment = rule(
    _platform_fragment_impl,
    attrs = {
        "_host_platform": attr.label(
            cfg = "exec",
            default = "//command_line_option:platforms",
        ),
        "_platform": attr.label(
            cfg = "target",
            default = "//command_line_option:platforms",
        ),
    },
)

def _proto_fragment_impl(ctx):
    return [FragmentInfo(
        experimental_protoc_opts = ctx.attr._protocopt[BuildSettingInfo].value,
    )]

proto_fragment = rule(
    _proto_fragment_impl,
    attrs = {
        "_protocopt": attr.label(default = "//command_line_option:protocopt"),
    },
)
