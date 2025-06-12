package unpack

import (
	"fmt"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/label"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type apparentLabelUnpackerInto struct{}

// ApparentLabel is capable of unpacking string arguments that contain
// an apparent label (e.g., "@rules_go//go/platform:darwin_arm64").
//
// If the label is not prefixed with a repo name (e.g., "//cmd/mytool")
// or relative (e.g., ":go_default_library"), the label is resolved to
// be relative to the package to which the current Starlark file
// belongs.
var ApparentLabel UnpackerInto[label.ApparentLabel] = apparentLabelUnpackerInto{}

func (apparentLabelUnpackerInto) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *label.ApparentLabel) error {
	s, ok := starlark.AsString(v)
	if !ok {
		return fmt.Errorf("got %s, want string", v.Type())
	}
	canonicalPackage := util.Must(label.NewCanonicalLabel(thread.CallFrame(1).Pos.Filename())).GetCanonicalPackage()
	l, err := canonicalPackage.AppendLabel(s)
	if err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}
	*dst = l
	return nil
}

func (ui apparentLabelUnpackerInto) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	var l label.ApparentLabel
	if err := ui.UnpackInto(thread, v, &l); err != nil {
		return nil, err
	}
	return starlark.String(l.String()), nil
}

func (apparentLabelUnpackerInto) GetConcatenationOperator() syntax.Token {
	return 0
}
