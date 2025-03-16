package label

import (
	"errors"
	"regexp"
	"strings"
)

// ModuleVersion are version number strings that correspond to the
// format that Bazel uses to version modules. The format is similar, but
// not identical to Semantic Versions.
//
// More details: https://bazel.build/external/module#version_format
type ModuleVersion struct {
	value string
}

const (
	versionIdentifierPattern      = `(0|[1-9][0-9]*|[0-9]*[a-zA-Z][0-9a-zA-Z]*)`
	prereleaseIdentifierPattern   = `(0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)`
	buildIdentifierPattern        = `[0-9a-zA-Z-]+`
	canonicalModuleVersionPattern = versionIdentifierPattern + `(\.` + versionIdentifierPattern + `)*` +
		`(-` + prereleaseIdentifierPattern + `(\.` + prereleaseIdentifierPattern + `)*)?`
	validModuleVersionPattern = canonicalModuleVersionPattern +
		`(\+` + buildIdentifierPattern + `(\.` + buildIdentifierPattern + `)*)?`
)

var (
	validModuleVersionRegexp = regexp.MustCompile("^" + validModuleVersionPattern + "$")
	errInvalidModuleVersion  = errors.New("module version must match " + validModuleVersionPattern)
)

// NewModuleVersion validates that a string contains a valid a Bazel
// module version (e.g., "1.7.1" or "0.0.0-20241220-5e258e33". If so, it
// wraps the value in a ModuleVersion.
func NewModuleVersion(value string) (ModuleVersion, error) {
	if !validModuleVersionRegexp.MatchString(value) {
		return ModuleVersion{}, errInvalidModuleVersion
	}
	return ModuleVersion{value: value}, nil
}

// MustNewModuleVersion is the same as NewModuleVersion, except that it
// panics if the provided value is not a valid Bazel module version.
func MustNewModuleVersion(value string) ModuleVersion {
	m, err := NewModuleVersion(value)
	if err != nil {
		panic(err)
	}
	return m
}

func (mv ModuleVersion) String() string {
	return mv.value
}

func stripIdentifier(in *string, separators string) string {
	i := strings.IndexAny(*in, separators)
	if i < 0 {
		i = len(*in)
	}
	out := (*in)[:i]
	*in = (*in)[i:]
	return out
}

func identifierIsNumeric(in string) int {
	for i := 0; i < len(in); i++ {
		if in[i] < '0' || in[i] > '9' {
			return 0
		}
	}
	return 1
}

func stripSeparator(in *string) int {
	if len(*in) == 0 {
		return 0
	}
	c := (*in)[0]
	*in = (*in)[1:]
	switch c {
	case '-':
		return -1
	case '+':
		return 0
	case '.':
		return 1
	default:
		panic("strip*Identifier() stripped up to an unknown character type")
	}
}

// Compare two Bazel module version numbers along a total order.
func (mv ModuleVersion) Compare(other ModuleVersion) int {
	separators := "+.-"
	a, b := mv.value, other.value
	for {
		aIdentifier, bIdentifier := stripIdentifier(&a, separators), stripIdentifier(&b, separators)

		aIsNumeric, bIsNumeric := identifierIsNumeric(aIdentifier), identifierIsNumeric(bIdentifier)
		if d := bIsNumeric - aIsNumeric; d != 0 {
			// Numeric identifiers always have lower
			// precedence than non-numeric identifiers.
			return d
		}
		if aIsNumeric != 0 {
			// Identifiers consisting of only digits are
			// compared numerically. Because our regular
			// expression does not accept leading zeros,
			// it is sufficient to compare the lengths.
			if d := len(aIdentifier) - len(bIdentifier); d != 0 {
				return d
			}
		}
		if cmp := strings.Compare(aIdentifier, bIdentifier); cmp != 0 {
			return cmp
		}

		aSeparator, bSeparator := stripSeparator(&a), stripSeparator(&b)
		if d := aSeparator - bSeparator; d != 0 || aSeparator == 0 {
			return d
		}
		if aSeparator <= 0 {
			// Prerelease and build identifiers may contain
			// dashes. Stop treating the dash as the end of
			// an identifier.
			separators = "+."
		}
	}
}
