package label_test

import (
	"testing"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/label"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalTargetPattern(t *testing.T) {
	t.Run("AsCanonicalLabel", func(t *testing.T) {
		t.Run("Failure", func(t *testing.T) {
			for _, input := range []string{
				"@@com_github_buildbarn_bb_storage+//:all",
				"@@com_github_buildbarn_bb_storage+//:all-targets",
				"@@com_github_buildbarn_bb_storage+//:*",
				"@@com_github_buildbarn_bb_storage+//...",
				"@@com_github_buildbarn_bb_storage+//...:all",
				"@@com_github_buildbarn_bb_storage+//...:all-targets",
				"@@com_github_buildbarn_bb_storage+//...:*",
				"@@com_github_buildbarn_bb_storage+//all:all",
				"@@com_github_buildbarn_bb_storage+//all-targets:all-targets",
				"@@com_github_buildbarn_bb_storage+//*:*",
				"@@com_github_buildbarn_bb_storage+//hello/...",
				"@@com_github_buildbarn_bb_storage+//hello/...:all",
				"@@com_github_buildbarn_bb_storage+//hello/...:all-targets",
				"@@com_github_buildbarn_bb_storage+//hello/...:*",
			} {
				t.Run(input, func(t *testing.T) {
					_, ok := util.Must(label.NewCanonicalTargetPattern(input)).AsCanonicalLabel()
					require.False(t, ok)
				})
			}
		})

		t.Run("Success", func(t *testing.T) {
			for _, input := range []string{
				"@@com_github_buildbarn_bb_storage+",
				"@@com_github_buildbarn_bb_storage+//:foo",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
				"@@com_github_buildbarn_bb_storage+//all",
				"@@com_github_buildbarn_bb_storage+//all-targets",
				"@@com_github_buildbarn_bb_storage+//*",
			} {
				canonicalLabel, ok := util.Must(label.NewCanonicalTargetPattern(input)).AsCanonicalLabel()
				require.True(t, ok)
				assert.Equal(t, input, canonicalLabel.String())
			}
		})
	})

	t.Run("AsSinglePackageTargetPattern", func(t *testing.T) {
		t.Run("Failure", func(t *testing.T) {
			for _, input := range []string{
				"@@com_github_buildbarn_bb_storage+",
				"@@com_github_buildbarn_bb_storage+//...",
				"@@com_github_buildbarn_bb_storage+//...:all",
				"@@com_github_buildbarn_bb_storage+//...:all-targets",
				"@@com_github_buildbarn_bb_storage+//...:*",
				"@@com_github_buildbarn_bb_storage+//hello/...",
				"@@com_github_buildbarn_bb_storage+//hello/...:all",
				"@@com_github_buildbarn_bb_storage+//hello/...:all-targets",
				"@@com_github_buildbarn_bb_storage+//hello/...:*",
				"@@com_github_buildbarn_bb_storage+//:foo",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
				"@@com_github_buildbarn_bb_storage+//all",
				"@@com_github_buildbarn_bb_storage+//all-targets",
				"@@com_github_buildbarn_bb_storage+//*",
			} {
				t.Run(input, func(t *testing.T) {
					_, _, ok := util.Must(label.NewCanonicalTargetPattern(input)).AsSinglePackageTargetPattern()
					require.False(t, ok)
				})
			}
		})

		t.Run("Success", func(t *testing.T) {
			initialTarget, includeFileTargets, ok := util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//:all")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//:all", initialTarget.String())
			require.False(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//:all-targets")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//:all-targets", initialTarget.String())
			require.True(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//:*")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//:*", initialTarget.String())
			require.True(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello:all")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello:all", initialTarget.String())
			require.False(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello:all-targets")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello:all-targets", initialTarget.String())
			require.True(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello:*")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello:*", initialTarget.String())
			require.True(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//all:all")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//all", initialTarget.String())
			require.False(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//all-targets:all-targets")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//all-targets", initialTarget.String())
			require.True(t, includeFileTargets)

			initialTarget, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//*:*")).AsSinglePackageTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//*", initialTarget.String())
			require.True(t, includeFileTargets)
		})
	})

	t.Run("AsRecursiveTargetPattern", func(t *testing.T) {
		t.Run("Failure", func(t *testing.T) {
			for _, input := range []string{
				"@@com_github_buildbarn_bb_storage+//:all",
				"@@com_github_buildbarn_bb_storage+//:all-targets",
				"@@com_github_buildbarn_bb_storage+//:*",
				"@@com_github_buildbarn_bb_storage+//all:all",
				"@@com_github_buildbarn_bb_storage+//all-targets:all-targets",
				"@@com_github_buildbarn_bb_storage+//*:*",
				"@@com_github_buildbarn_bb_storage+",
				"@@com_github_buildbarn_bb_storage+//:foo",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
				"@@com_github_buildbarn_bb_storage+//all",
				"@@com_github_buildbarn_bb_storage+//all-targets",
				"@@com_github_buildbarn_bb_storage+//*",
			} {
				t.Run(input, func(t *testing.T) {
					_, _, ok := util.Must(label.NewCanonicalTargetPattern(input)).AsRecursiveTargetPattern()
					require.False(t, ok)
				})
			}
		})

		t.Run("Success", func(t *testing.T) {
			basePackage, includeFileTargets, ok := util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//...")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+", basePackage.String())
			require.False(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//...:all")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+", basePackage.String())
			require.False(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//...:all-targets")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+", basePackage.String())
			require.True(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//...:*")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+", basePackage.String())
			require.True(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello/...")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello", basePackage.String())
			require.False(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello/...:all")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello", basePackage.String())
			require.False(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello/...:all-targets")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello", basePackage.String())
			require.True(t, includeFileTargets)

			basePackage, includeFileTargets, ok = util.Must(label.NewCanonicalTargetPattern("@@com_github_buildbarn_bb_storage+//hello/...:*")).AsRecursiveTargetPattern()
			require.True(t, ok)
			require.Equal(t, "@@com_github_buildbarn_bb_storage+//hello", basePackage.String())
			require.True(t, includeFileTargets)
		})
	})
}
