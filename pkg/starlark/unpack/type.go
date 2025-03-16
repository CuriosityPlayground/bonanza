package unpack

import (
	"fmt"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

type typeUnpackerInto[T starlark.Value] struct {
	name string
}

// Type is capable of unpacking values of an exact Go type. This can be
// used to validate that an argument corresponds to a custom Starlark
// type written in Go (e.g., a struct).
func Type[T starlark.Value](name string) UnpackerInto[T] {
	return &typeUnpackerInto[T]{
		name: name,
	}
}

func (ui *typeUnpackerInto[T]) UnpackInto(thread *starlark.Thread, v starlark.Value, dst *T) error {
	if typedV, ok := v.(T); ok {
		*dst = typedV
		return nil
	}
	return fmt.Errorf("got %s, want %s", v.Type(), ui.name)
}

func (ui *typeUnpackerInto[T]) Canonicalize(thread *starlark.Thread, v starlark.Value) (starlark.Value, error) {
	if _, ok := v.(T); ok {
		return v, nil
	}
	return nil, fmt.Errorf("got %s, want %s", v.Type(), ui.name)
}

func (typeUnpackerInto[T]) GetConcatenationOperator() syntax.Token {
	return 0
}
