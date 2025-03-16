package unpack

import (
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type anyUnpackerInto struct{}

// Any can unpack any Starlark value argument and store it into the
// provided value. This can be useful for situations where the actual
// parsing of an argument needs to be delayed to a later point in time
// (e.g., after interpreting other arguments that control the type of
// this argument.
var Any UnpackerInto[starlark.Value] = anyUnpackerInto{}

func (anyUnpackerInto) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *starlark.Value) error {
	*dst = v
	return nil
}

func (anyUnpackerInto) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	return v, nil
}

func (anyUnpackerInto) GetConcatenationOperator() syntax.Token {
	return 0
}
