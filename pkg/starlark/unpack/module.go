package unpack

import (
	"fmt"

	"bonanza.build/pkg/label"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type moduleUnpackerInto struct{}

// Module is capable of unpacking a string value containing a Bazel
// module name (e.g., "rules_go").
var Module UnpackerInto[label.Module] = moduleUnpackerInto{}

func (moduleUnpackerInto) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *label.Module) error {
	s, ok := starlark.AsString(v)
	if !ok {
		return fmt.Errorf("got %s, want string", v.Type())
	}
	m, err := label.NewModule(s)
	if err != nil {
		return fmt.Errorf("invalid module: %w", err)
	}
	*dst = m
	return nil
}

func (ui moduleUnpackerInto) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	var m label.Module
	if err := ui.UnpackInto(thread, v, &m); err != nil {
		return nil, err
	}
	return starlark.String(m.String()), nil
}

func (moduleUnpackerInto) GetConcatenationOperator() syntax.Token {
	return 0
}
