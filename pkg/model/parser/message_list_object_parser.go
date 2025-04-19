package parser

import (
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/encoding/varint"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type messageListObjectParser[
	TReference any,
	TMessage any,
	TMessagePtr interface {
		*TMessage
		proto.Message
	},
] struct{}

// NewMessageListObjectParser is capable of unmarshaling objects
// containing a list of Protobuf messages. Messages are prefixed with
// their size, encoded as a variable length integer.
func NewMessageListObjectParser[
	TReference any,
	TMessage any,
	TMessagePtr interface {
		*TMessage
		proto.Message
	},
]() ObjectParser[TReference, model_core.Message[[]TMessagePtr, TReference]] {
	return &messageListObjectParser[TReference, TMessage, TMessagePtr]{}
}

func (p *messageListObjectParser[TReference, TMessage, TMessagePtr]) ParseObject(in model_core.Message[[]byte, TReference], decodingParameters []byte) (model_core.Message[[]TMessagePtr, TReference], int, error) {
	if len(decodingParameters) > 0 {
		return model_core.Message[[]TMessagePtr, TReference]{}, 0, status.Error(codes.InvalidArgument, "Unexpected decoding parameters")
	}

	data := in.Message
	originalDataLength := len(data)
	var elements []TMessagePtr
	for len(data) > 0 {
		// Extract the size of the element.
		offset := originalDataLength - len(data)
		length, lengthLength := varint.ConsumeForward[uint](data)
		if lengthLength < 0 {
			return model_core.Message[[]TMessagePtr, TReference]{}, 0, status.Errorf(codes.InvalidArgument, "Invalid element length at offset %d", offset)
		}

		// Validate the size.
		data = data[lengthLength:]
		if length > uint(len(data)) {
			return model_core.Message[[]TMessagePtr, TReference]{}, 0, status.Errorf(codes.InvalidArgument, "Length of element at offset %d is %d bytes, which exceeds maximum permitted size of %d bytes", offset, length, len(data))
		}

		// Unmarshal the element.
		var element TMessage
		if err := proto.Unmarshal(data[:length], TMessagePtr(&element)); err != nil {
			return model_core.Message[[]TMessagePtr, TReference]{}, 0, util.StatusWrapfWithCode(err, codes.InvalidArgument, "Failed to unmarshal element at offset %d", offset)
		}
		elements = append(elements, &element)
		data = data[length:]
	}

	return model_core.NewMessage(elements, in.OutgoingReferences), len(in.Message), nil
}

func (p *messageListObjectParser[TReference, TMessage, TMessagePtr]) GetDecodingParametersSizeBytes() int {
	return 0
}
