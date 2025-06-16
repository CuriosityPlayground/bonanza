package filesystem

import (
	"github.com/buildbarn/bb-storage/pkg/util"
	model_core "github.com/buildbarn/bonanza/pkg/model/core"
	"github.com/buildbarn/bonanza/pkg/model/encoding"
	model_parser "github.com/buildbarn/bonanza/pkg/model/parser"
	model_filesystem_pb "github.com/buildbarn/bonanza/pkg/proto/model/filesystem"
	"github.com/buildbarn/bonanza/pkg/storage/object"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FileAccessParameters contains parameters that were used when creating
// Merkle trees of files that should also be applied when attempting to
// access its contents afterwards. Parameters include whether files were
// compressed or encrypted.
type FileAccessParameters struct {
	chunkEncoder            encoding.BinaryEncoder
	fileContentsListEncoder encoding.BinaryEncoder
}

// NewFileAccessParametersFromProto creates an instance of
// FileAccessParameters that matches the configuration stored in a
// Protobuf message. This, for example, permits a server to access files
// that were uploaded by a client.
func NewFileAccessParametersFromProto(m *model_filesystem_pb.FileAccessParameters, referenceFormat object.ReferenceFormat) (*FileAccessParameters, error) {
	if m == nil {
		return nil, status.Error(codes.InvalidArgument, "No file access parameters provided")
	}

	maximumObjectSizeBytes := uint32(referenceFormat.GetMaximumObjectSizeBytes())
	chunkEncoder, err := encoding.NewBinaryEncoderFromProto(m.ChunkEncoders, maximumObjectSizeBytes)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid chunk encoder")
	}
	fileContentsListEncoder, err := encoding.NewBinaryEncoderFromProto(m.FileContentsListEncoders, maximumObjectSizeBytes)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid file contents list encoder")
	}

	return &FileAccessParameters{
		chunkEncoder:            chunkEncoder,
		fileContentsListEncoder: fileContentsListEncoder,
	}, nil
}

// DecodeFileContentsList extracts the FileContents list that is stored
// in an object backed by storage.
//
// TODO: Maybe we should simply throw out this method? It doesn't
// provide a lot of value.
func (p *FileAccessParameters) DecodeFileContentsList(contents *object.Contents, decodingParameters []byte) ([]*model_filesystem_pb.FileContents, error) {
	fileContentsList, _, err := model_parser.NewChainedObjectParser(
		model_parser.NewEncodedObjectParser[object.LocalReference](p.fileContentsListEncoder),
		model_parser.NewProtoListObjectParser[object.LocalReference, model_filesystem_pb.FileContents](),
	).ParseObject(
		model_core.NewMessage(contents.GetPayload(), object.OutgoingReferences[object.LocalReference](contents)),
		decodingParameters,
	)
	if err != nil {
		return nil, err
	}
	return fileContentsList.Message, nil
}

func (p *FileAccessParameters) GetChunkEncoder() encoding.BinaryEncoder {
	return p.chunkEncoder
}

func (p *FileAccessParameters) GetFileContentsListEncoder() encoding.BinaryEncoder {
	return p.fileContentsListEncoder
}
