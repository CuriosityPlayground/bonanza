package tag

import (
	"context"

	"bonanza.build/pkg/proto/storage/tag"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type updaterServer struct {
	updater Updater[object.GlobalReference, []byte]
}

// NewUpdaterServer creates a gRPC server that is capable of creating or
// updating existing tags contained in the tag store.
func NewUpdaterServer(updater Updater[object.GlobalReference, []byte]) tag.UpdaterServer {
	return &updaterServer{
		updater: updater,
	}
}

func (s *updaterServer) UpdateTag(ctx context.Context, request *tag.UpdateTagRequest) (*emptypb.Empty, error) {
	if request.Tag == nil {
		return nil, status.Error(codes.InvalidArgument, "No tag provided")
	}
	namespace, err := object.NewNamespace(request.Namespace)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid namespace")
	}
	reference, err := namespace.NewGlobalReference(request.Reference)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid reference")
	}
	if err := s.updater.UpdateTag(ctx, request.Tag, reference, request.Lease, request.Overwrite); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
