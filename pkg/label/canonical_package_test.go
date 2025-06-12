package label_test

import (
	"testing"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/label"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalPackage(t *testing.T) {
	t.Run("Valid", func(t *testing.T) {
		for _, input := range []string{
			"@@com_github_buildbarn_bb_storage+",
			"@@com_github_buildbarn_bb_storage+//cmd",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
			`@@com_github_buildbarn_bb_storage+//cmd/! "#$%&'()*+,-.;<=>?@[]^_{|}` + "`",
		} {
			canonicalPackage := util.Must(label.NewCanonicalPackage(input))
			assert.Equal(t, input, canonicalPackage.String())
		}
	})

	t.Run("Invalid", func(t *testing.T) {
		for _, input := range []string{
			"",
			"hello",
			"//cmd/hello_world",
			"@repo//cmd/hello_world",
			"@//cmd/hello_world",
			"@@//cmd/hello_world",
			"@@com_github_buildbarn_bb_storage+//",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world:target",
			"@@com_github_buildbarn_bb_storage+:target",
			"@@com_github_buildbarn_bb_storage+//cmd//hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/./hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/../hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/.../hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/..../hello_world",
			"@@com_github_buildbarn_bb_storage+///cmd/hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world/",
		} {
			_, err := label.NewCanonicalPackage(input)
			assert.ErrorContains(t, err, "canonical package name must match ", input)
		}
	})

	t.Run("GetCanonicalRepo", func(t *testing.T) {
		for _, input := range []string{
			"@@com_github_buildbarn_bb_storage+",
			"@@com_github_buildbarn_bb_storage+//cmd",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
		} {
			canonicalPackage := util.Must(label.NewCanonicalPackage(input))
			assert.Equal(t, "com_github_buildbarn_bb_storage+", canonicalPackage.GetCanonicalRepo().String())
		}
	})

	t.Run("GetPackagePath", func(t *testing.T) {
		for input, output := range map[string]string{
			"@@com_github_buildbarn_bb_storage+":                  "",
			"@@com_github_buildbarn_bb_storage+//cmd":             "cmd",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world": "cmd/hello_world",
		} {
			canonicalPackage := util.Must(label.NewCanonicalPackage(input))
			assert.Equal(t, output, canonicalPackage.GetPackagePath())
		}
	})

	t.Run("AppendLabel", func(t *testing.T) {
		t.Run("AtRoot", func(t *testing.T) {
			base := util.Must(label.NewCanonicalPackage("@@example+"))
			for input, output := range map[string]string{
				":foo":         "@@example+//:foo",
				"bar:wiz":      "@@example+//bar:wiz",
				"bar/wiz":      "@@example+//:bar/wiz",
				"bar:all":      "@@example+//bar:all",
				":all":         "@@example+//:all",
				"//baz":        "@@example+//baz",
				"//:example+":  "@@example+",
				"@foo/bar/baz": "@@example+//:@foo/bar/baz",
				"@foo:@bar":    "@@example+//@foo:@bar",
			} {
				newLabel, err := base.AppendLabel(input)
				require.NoError(t, err)
				assert.Equal(t, output, newLabel.String())
			}
		})
		t.Run("InsidePackage", func(t *testing.T) {
			base := util.Must(label.NewCanonicalPackage("@@example+//foo"))
			for input, output := range map[string]string{
				":foo":                "@@example+//foo",
				"bar:wiz":             "@@example+//foo/bar:wiz",
				"bar/wiz":             "@@example+//foo:bar/wiz",
				"bar:all":             "@@example+//foo/bar:all",
				":all":                "@@example+//foo:all",
				"//baz":               "@@example+//baz",
				"//:example+":         "@@example+",
				"@@other1+":           "@@other1+",
				"@@other1+//:other1+": "@@other1+",
				"@@other1+//:foo":     "@@other1+//:foo",
				"@other2":             "@other2",
				"@other2//:other2":    "@other2",
				"@other2//:foo":       "@other2//:foo",
			} {
				newLabel, err := base.AppendLabel(input)
				require.NoError(t, err)
				assert.Equal(t, output, newLabel.String())
			}
		})
	})

	t.Run("AppendTargetName", func(t *testing.T) {
		require.Equal(
			t,
			"@@example+//:foo",
			util.Must(label.NewCanonicalPackage("@@example+")).
				AppendTargetName(util.Must(label.NewTargetName("foo"))).
				String(),
		)
		require.Equal(
			t,
			"@@example+",
			util.Must(label.NewCanonicalPackage("@@example+")).
				AppendTargetName(util.Must(label.NewTargetName("example+"))).
				String(),
		)
		require.Equal(
			t,
			"@@example+//hello_world:foo",
			util.Must(label.NewCanonicalPackage("@@example+//hello_world")).
				AppendTargetName(util.Must(label.NewTargetName("foo"))).
				String(),
		)
		require.Equal(
			t,
			"@@example+//hello_world",
			util.Must(label.NewCanonicalPackage("@@example+//hello_world")).
				AppendTargetName(util.Must(label.NewTargetName("hello_world"))).
				String(),
		)
	})

	t.Run("AppendTargetPattern", func(t *testing.T) {
		base := util.Must(label.NewCanonicalPackage("@@example+//foo"))
		for input, output := range map[string]string{
			"//foo/bar:wiz":         "@@example+//foo/bar:wiz",
			"//foo/bar":             "@@example+//foo/bar",
			"//foo/bar:bar":         "@@example+//foo/bar",
			"//foo/bar:all":         "@@example+//foo/bar:all",
			"//foo/...":             "@@example+//foo/...",
			"//foo/...:all":         "@@example+//foo/...",
			"//foo/...:*":           "@@example+//foo/...:*",
			"//foo/...:all-targets": "@@example+//foo/...:*",
			"//...":                 "@@example+//...",
			"//:all":                "@@example+//:all",
			":foo":                  "@@example+//foo",
			"bar:wiz":               "@@example+//foo/bar:wiz",
			// According to the Bazel documentation, "bar/wiz"
			// may also expand to @example+//foo/bar/wiz or
			// @example+//foo/bar:wiz, depending on whether
			// those packages exist.
			//
			// This is something we likely do not want to
			// support, as such relative patterns do not have
			// an absolute representation.
			"bar/wiz":     "@@example+//foo:bar/wiz",
			"bar:all":     "@@example+//foo/bar:all",
			":all":        "@@example+//foo:all",
			"...:all":     "@@example+//foo/...",
			"...":         "@@example+//foo/...",
			"bar/...":     "@@example+//foo/bar/...",
			"bar/...:all": "@@example+//foo/bar/...",

			"//all:all":     "@@example+//all:all",
			"//all":         "@@example+//all",
			"@@foo+":        "@@foo+",
			"@@foo+//:foo+": "@@foo+",
			"@@foo+//:bar":  "@@foo+//:bar",
			"@foo":          "@foo",
			"@foo//:foo":    "@foo",
			"@foo//:bar":    "@foo//:bar",
			"@all//:all":    "@all//:all",
			"@@//...":       "@@//...",
		} {
			newLabel, err := base.AppendTargetPattern(input)
			require.NoError(t, err)
			assert.Equal(t, output, newLabel.String())
		}
	})

	t.Run("ToRecursiveTargetPatternBelow", func(t *testing.T) {
		t.Run("Invalid", func(t *testing.T) {
			base := util.Must(label.NewCanonicalPackage("@@repo+"))
			for _, input := range []string{
				"",
				"foo//bar",
				"./foo",
				"foo/",
			} {
				_, err := base.ToRecursiveTargetPatternBelow(input, false)
				assert.ErrorContains(t, err, "non-empty package name must match ", input)
			}
		})

		t.Run("Valid", func(t *testing.T) {
			canonicalTargetPattern, err := util.Must(label.NewCanonicalPackage("@@repo+")).
				ToRecursiveTargetPatternBelow("foo/bar", false)
			require.NoError(t, err)
			assert.Equal(t, "@@repo+//foo/bar/...", canonicalTargetPattern.String())

			canonicalTargetPattern, err = util.Must(label.NewCanonicalPackage("@@repo+//foo")).
				ToRecursiveTargetPatternBelow("bar", true)
			require.NoError(t, err)
			assert.Equal(t, "@@repo+//foo/bar/...:*", canonicalTargetPattern.String())
		})
	})
}
