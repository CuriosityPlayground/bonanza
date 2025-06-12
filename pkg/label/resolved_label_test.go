package label_test

import (
	"errors"
	"testing"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/label"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvedLabel(t *testing.T) {
	t.Run("ValidNormalized", func(t *testing.T) {
		for _, input := range []string{
			"@@com_github_buildbarn_bb_storage+",
			"@@com_github_buildbarn_bb_storage+//:foo",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world:go_default_library",
			`@@com_github_buildbarn_bb_storage+//cmd/! "#$%&'()*+,-.;<=>?@[]^_{|}` + "`",
			`@@com_github_buildbarn_bb_storage+//cmd/ℕ ⊆ ℕ₀ ⊂ ℤ ⊂ ℚ ⊂ ℝ ⊂ ℂ`,
			`@@com_github_buildbarn_bb_storage+//cmd/hello_world:ℕ ⊆ ℕ₀ ⊂ ℤ ⊂ ℚ ⊂ ℝ ⊂ ℂ`,
			"@@[this is an error message]//:foo",
			"@@[this is an error message]//pkg",
			"@@[this is an error message]//pkg:foo",
			"@@[this is an error message]//:[this is an error message]",
		} {
			resolvedLabel := util.Must(label.NewResolvedLabel(input))
			assert.Equal(t, input, resolvedLabel.String())
		}
	})

	t.Run("ValidDenormalized", func(t *testing.T) {
		for input, output := range map[string]string{
			"@@com_github_buildbarn_bb_storage+//:com_github_buildbarn_bb_storage+": "@@com_github_buildbarn_bb_storage+",
			"@@com_github_buildbarn_bb_storage+//cmd:cmd":                           "@@com_github_buildbarn_bb_storage+//cmd",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world:hello_world":       "@@com_github_buildbarn_bb_storage+//cmd/hello_world",
			"@@[this is an error message]//pkg:pkg":                                 "@@[this is an error message]//pkg",
		} {
			resolvedLabel := util.Must(label.NewResolvedLabel(input))
			assert.Equal(t, output, resolvedLabel.String())
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
			"@@com_github_buildbarn_bb_storage+:target",
			"@@com_github_buildbarn_bb_storage+//cmd//hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/./hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/../hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/.../hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/..../hello_world",
			"@@com_github_buildbarn_bb_storage+///cmd/hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world/",
			"@@com_github_buildbarn_bb_storage+//foo\nbar",
			"@@[]//:foo",
			"@@[this is an error message]",
		} {
			_, err := label.NewResolvedLabel(input)
			assert.ErrorContains(t, err, "resolved label must match ", input)
		}
	})

	t.Run("AsCanonical", func(t *testing.T) {
		t.Run("Success", func(t *testing.T) {
			for _, input := range []string{
				"@@com_github_buildbarn_bb_storage+",
				"@@com_github_buildbarn_bb_storage+//:foo",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world:go_default_library",
			} {
				canonicalLabel, err := util.Must(label.NewResolvedLabel(input)).AsCanonical()
				require.NoError(t, err)
				assert.Equal(t, input, canonicalLabel.String())
			}
		})

		t.Run("Failure", func(t *testing.T) {
			for _, input := range []string{
				"@@[this is an error message]//:foo",
				"@@[this is an error message]//cmd/hello_world",
				"@@[this is an error message]//cmd/hello_world:go_default_library",
			} {
				_, err := util.Must(label.NewResolvedLabel(input)).AsCanonical()
				assert.Equal(t, errors.New("this is an error message"), err)
			}
		})
	})

	t.Run("GetPackagePath", func(t *testing.T) {
		for input, output := range map[string]string{
			"@@com_github_buildbarn_bb_storage+":                                         "",
			"@@com_github_buildbarn_bb_storage+//:foo":                                   "",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world":                        "cmd/hello_world",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world:go_default_library":     "cmd/hello_world",
			`@@com_github_buildbarn_bb_storage+//cmd/! "#$%&'()*+,-.;<=>?@[]^_{|}` + "`": `cmd/! "#$%&'()*+,-.;<=>?@[]^_{|}` + "`",
			`@@com_github_buildbarn_bb_storage+//cmd/ℕ ⊆ ℕ₀ ⊂ ℤ ⊂ ℚ ⊂ ℝ ⊂ ℂ`:             `cmd/ℕ ⊆ ℕ₀ ⊂ ℤ ⊂ ℚ ⊂ ℝ ⊂ ℂ`,
			`@@com_github_buildbarn_bb_storage+//cmd/hello_world:ℕ ⊆ ℕ₀ ⊂ ℤ ⊂ ℚ ⊂ ℝ ⊂ ℂ`: "cmd/hello_world",
			"@@[this is an error message]//:foo":                                         "",
			"@@[this is an error message]//pkg":                                          "pkg",
			"@@[this is an error message]//pkg:foo":                                      "pkg",
			"@@[this is an error message]//:[this is an error message]":                  "",
		} {
			resolvedLabel := util.Must(label.NewResolvedLabel(input))
			assert.Equal(t, output, resolvedLabel.GetPackagePath())
		}
	})

	t.Run("GetTargetName", func(t *testing.T) {
		for input, output := range map[string]string{
			"@@com_github_buildbarn_bb_storage+":                        "com_github_buildbarn_bb_storage+",
			"@@com_github_buildbarn_bb_storage+//:foo":                  "foo",
			"@@com_github_buildbarn_bb_storage+//cmd":                   "cmd",
			"@@com_github_buildbarn_bb_storage+//cmd/hello_world":       "hello_world",
			"@@[this is an error message]//:foo":                        "foo",
			"@@[this is an error message]//pkg":                         "pkg",
			"@@[this is an error message]//pkg:foo":                     "foo",
			"@@[this is an error message]//:[this is an error message]": "[this is an error message]",
		} {
			resolvedLabel := util.Must(label.NewResolvedLabel(input))
			assert.Equal(t, output, resolvedLabel.GetTargetName().String())
		}
	})

	t.Run("AppendTargetName", func(t *testing.T) {
		require.Equal(
			t,
			"@@example+//:foo",
			util.Must(label.NewResolvedLabel("@@example+")).
				AppendTargetName(util.Must(label.NewTargetName("foo"))).
				String(),
		)
		require.Equal(
			t,
			"@@example+",
			util.Must(label.NewResolvedLabel("@@example+")).
				AppendTargetName(util.Must(label.NewTargetName("example+"))).
				String(),
		)
		require.Equal(
			t,
			"@@example+//hello_world:foo",
			util.Must(label.NewResolvedLabel("@@example+//hello_world")).
				AppendTargetName(util.Must(label.NewTargetName("foo"))).
				String(),
		)
		require.Equal(
			t,
			"@@example+//hello_world",
			util.Must(label.NewResolvedLabel("@@example+//hello_world")).
				AppendTargetName(util.Must(label.NewTargetName("hello_world"))).
				String(),
		)

		require.Equal(
			t,
			"@@[this is an error message]//:bar",
			util.Must(label.NewResolvedLabel("@@[this is an error message]//:foo")).
				AppendTargetName(util.Must(label.NewTargetName("bar"))).
				String(),
		)
		require.Equal(
			t,
			"@@[this is an error message]//pkg:bar",
			util.Must(label.NewResolvedLabel("@@[this is an error message]//pkg:foo")).
				AppendTargetName(util.Must(label.NewTargetName("bar"))).
				String(),
		)
		require.Equal(
			t,
			"@@[this is an error message]//pkg:bar",
			util.Must(label.NewResolvedLabel("@@[this is an error message]//pkg")).
				AppendTargetName(util.Must(label.NewTargetName("bar"))).
				String(),
		)
		require.Equal(
			t,
			"@@[this is an error message]//pkg",
			util.Must(label.NewResolvedLabel("@@[this is an error message]//pkg")).
				AppendTargetName(util.Must(label.NewTargetName("pkg"))).
				String(),
		)
		require.Equal(
			t,
			"@@[this is an error message]//pkg",
			util.Must(label.NewResolvedLabel("@@[this is an error message]//pkg")).
				AppendTargetName(util.Must(label.NewTargetName("pkg"))).
				String(),
		)
	})
}
