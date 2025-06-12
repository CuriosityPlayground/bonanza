package label_test

import (
	"testing"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/label"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApparentLabel(t *testing.T) {
	t.Run("AsCanonical", func(t *testing.T) {
		t.Run("Failure", func(t *testing.T) {
			for _, input := range []string{
				"@@//:foo",
				"@@//cmd/hello_world",
				"@com_github_buildbarn_bb_storage",
				"@com_github_buildbarn_bb_storage//:foo",
				"@com_github_buildbarn_bb_storage//cmd/hello_world",
			} {
				_, ok := util.Must(label.NewApparentLabel(input)).AsCanonical()
				require.False(t, ok)
			}
		})

		t.Run("Success", func(t *testing.T) {
			for _, input := range []string{
				"@@com_github_buildbarn_bb_storage+",
				"@@com_github_buildbarn_bb_storage+//:foo",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
			} {
				canonicalLabel, ok := util.Must(label.NewApparentLabel(input)).AsCanonical()
				require.True(t, ok)
				assert.Equal(t, input, canonicalLabel.String())
			}
		})
	})

	t.Run("GetApparentRepo", func(t *testing.T) {
		t.Run("Failure", func(t *testing.T) {
			for _, input := range []string{
				"@@//:foo",
				"@@//cmd/hello_world",
				"@@com_github_buildbarn_bb_storage+",
				"@@com_github_buildbarn_bb_storage+//:foo",
				"@@com_github_buildbarn_bb_storage+//cmd/hello_world",
			} {
				_, ok := util.Must(label.NewApparentLabel(input)).GetApparentRepo()
				require.False(t, ok)
			}
		})

		t.Run("Success", func(t *testing.T) {
			for _, input := range []string{
				"@com_github_buildbarn_bb_storage",
				"@com_github_buildbarn_bb_storage//:foo",
				"@com_github_buildbarn_bb_storage//cmd/hello_world",
			} {
				apparentRepo, ok := util.Must(label.NewApparentLabel(input)).GetApparentRepo()
				require.True(t, ok)
				assert.Equal(t, "com_github_buildbarn_bb_storage", apparentRepo.String())
			}
		})
	})

	t.Run("WithCanonicalRepo", func(t *testing.T) {
		toRepo := util.Must(label.NewCanonicalRepo("target+"))

		for input, output := range map[string]string{
			"@rules_go//tests/legacy/info": "@@target+//tests/legacy/info",
			"@rules_go//:rules_go":         "@@target+//:rules_go",
			"@rules_go":                    "@@target+//:rules_go",
			"@rules_go//:target+":          "@@target+",
			"@@//:rules_go":                "@@target+//:rules_go",
			"@@//hello/world":              "@@target+//hello/world",
		} {
			require.Equal(t, output, util.Must(label.NewApparentLabel(input)).WithCanonicalRepo(toRepo).String())
		}
	})

	t.Run("AsResolvedWithError", func(t *testing.T) {
		for input, output := range map[string]string{
			"@@rules_go+//package:target":  "@@[message]//package:target",
			"@rules_go//tests/legacy/info": "@@[message]//tests/legacy/info",
			"@rules_go//:rules_go":         "@@[message]//:rules_go",
			"@rules_go":                    "@@[message]//:rules_go",
			"@rules_go//:target":           "@@[message]//:target",
			"@@//:rules_go":                "@@[message]//:rules_go",
			"@@//hello/world":              "@@[message]//hello/world",
		} {
			require.Equal(t, output, util.Must(label.NewApparentLabel(input)).AsResolvedWithError("message").String())
		}
	})
}
