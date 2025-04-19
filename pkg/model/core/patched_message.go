package core

import (
	"github.com/buildbarn/bonanza/pkg/encoding/varint"
	model_encoding "github.com/buildbarn/bonanza/pkg/model/encoding"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"google.golang.org/protobuf/proto"
)

// PatchedMessage is a tuple for storing a Protobuf message that
// contains model_core_pb.Reference messages, and the associated
// ReferenceMessagePatcher that can be used to assign indices to these
// references.
type PatchedMessage[TMessage any, TMetadata ReferenceMetadata] struct {
	Message TMessage
	Patcher *ReferenceMessagePatcher[TMetadata]
}

// NewPatchedMessage creates a PatchedMessage, given an existing Protobuf
// message and reference message patcher.
func NewPatchedMessage[TMessage any, TMetadata ReferenceMetadata](
	message TMessage,
	patcher *ReferenceMessagePatcher[TMetadata],
) PatchedMessage[TMessage, TMetadata] {
	return PatchedMessage[TMessage, TMetadata]{
		Message: message,
		Patcher: patcher,
	}
}

// NewPatchedMessageFromExisting creates a PatchedMessage, given an
// existing Protobuf message that may contain one or more references to
// other objects. For each reference that is found, a callback is
// invoked to create metadata to associate with the reference.
func NewPatchedMessageFromExisting[
	TMessage any,
	TMetadata ReferenceMetadata,
	TMessagePtr interface {
		*TMessage
		proto.Message
	},
	TReference object.BasicReference,
](
	existing Message[TMessagePtr, TReference],
	createMetadata ReferenceMetadataCreator[TMetadata],
) PatchedMessage[TMessagePtr, TMetadata] {
	patcher := NewReferenceMessagePatcher[TMetadata]()
	if existing.Message == nil || existing.OutgoingReferences.GetDegree() == 0 {
		return NewPatchedMessage(existing.Message, patcher)
	}

	clonedMessage := proto.Clone(existing.Message)
	a := referenceMessageAdder[TMetadata, TReference]{
		patcher:            patcher,
		outgoingReferences: existing.OutgoingReferences,
		createMetadata:     createMetadata,
	}
	a.addReferenceMessagesRecursively(clonedMessage.ProtoReflect())
	return NewPatchedMessage(clonedMessage.(TMessagePtr), patcher)
}

// NewSimplePatchedMessage is a helper function for creating instances
// of PatchedMessage for messages that don't contain any references.
func NewSimplePatchedMessage[TMetadata ReferenceMetadata, TMessage any](v TMessage) PatchedMessage[TMessage, TMetadata] {
	return NewPatchedMessage(v, NewReferenceMessagePatcher[TMetadata]())
}

// IsSet returns true if the PatchedMessage is assigned to a message
// and its associated reference message patcher.
func (m PatchedMessage[T, TMetadata]) IsSet() bool {
	return m.Patcher != nil
}

// Clear the instance of PatchedMessage, disassociating it from its
// message and reference message patcher. The reference message patcher
// is not discarded, meaning that any resources owned by reference
// metadata is not released.
func (m *PatchedMessage[T, TMetadata]) Clear() {
	*m = PatchedMessage[T, TMetadata]{}
}

// Discard the reference message patcher, releasing any resources owned
// by reference metadata.
func (m *PatchedMessage[T, TMetadata]) Discard() {
	if m.Patcher != nil {
		m.Patcher.Discard()
	}
	m.Clear()
}

// SortAndSetReferences assigns indices to outgoing references.
func (m PatchedMessage[T, TMetadata]) SortAndSetReferences() (Message[T, object.LocalReference], []TMetadata) {
	references, metadata := m.Patcher.SortAndSetReferences()
	return Message[T, object.LocalReference]{
		Message:            m.Message,
		OutgoingReferences: references,
	}, metadata
}

func encode[TMetadata ReferenceMetadata](
	data []byte,
	references []object.LocalReference,
	metadata []TMetadata,
	referenceFormat object.ReferenceFormat,
	encoder model_encoding.BinaryEncoder,
) (Decodable[CreatedObject[TMetadata]], error) {
	encodedData, decodingParameters, err := encoder.EncodeBinary(data)
	if err != nil {
		return Decodable[CreatedObject[TMetadata]]{}, err
	}
	contents, err := referenceFormat.NewContents(references, encodedData)
	if err != nil {
		return Decodable[CreatedObject[TMetadata]]{}, err
	}
	return NewDecodable(
		CreatedObject[TMetadata]{
			Contents: contents,
			Metadata: metadata,
		},
		decodingParameters,
	), nil
}

var marshalOptions = proto.MarshalOptions{
	Deterministic: true,
	UseCachedSize: true,
}

// MarshalAndEncodePatchedMessage marshals a Protobuf message, encodes
// it, and converts it to an object that can be written to storage.
func MarshalAndEncodePatchedMessage[TMessage proto.Message, TMetadata ReferenceMetadata](
	m PatchedMessage[TMessage, TMetadata],
	referenceFormat object.ReferenceFormat,
	encoder model_encoding.BinaryEncoder,
) (Decodable[CreatedObject[TMetadata]], error) {
	references, metadata := m.Patcher.SortAndSetReferences()
	data, err := marshalOptions.Marshal(m.Message)
	if err != nil {
		return Decodable[CreatedObject[TMetadata]]{}, err
	}
	return encode(data, references, metadata, referenceFormat, encoder)
}

// MarshalAndEncodePatchedListMessage marshals a list of Protobuf
// messages, encodes them, and converts them to a single object that can
// be written to storage.
func MarshalAndEncodePatchedListMessage[TMessage proto.Message, TMetadata ReferenceMetadata](
	m PatchedMessage[[]TMessage, TMetadata],
	referenceFormat object.ReferenceFormat,
	encoder model_encoding.BinaryEncoder,
) (Decodable[CreatedObject[TMetadata]], error) {
	references, metadata := m.Patcher.SortAndSetReferences()
	var data []byte
	for _, node := range m.Message {
		data = varint.AppendForward(data, marshalOptions.Size(node))
		var err error
		data, err = marshalOptions.MarshalAppend(data, node)
		if err != nil {
			return Decodable[CreatedObject[TMetadata]]{}, err
		}
	}
	return encode(data, references, metadata, referenceFormat, encoder)
}
