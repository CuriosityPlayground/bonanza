package tag

import (
	"context"

	"bonanza.build/pkg/proto/storage/tag"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type resolverServer struct {
	resolver Resolver[object.Namespace]
}

// NewResolverServer creates a gRPC server that is capable of resolving
// tags to an object.
func NewResolverServer(resolver Resolver[object.Namespace]) tag.ResolverServer {
	return &resolverServer{
		resolver: resolver,
	}
}

func (s *resolverServer) ResolveTag(ctx context.Context, request *tag.ResolveTagRequest) (*tag.ResolveTagResponse, error) {
	namespace, err := object.NewNamespace(request.Namespace)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid namespace")
	}
	if request.Tag == nil {
		return nil, status.Error(codes.InvalidArgument, "No tag provided")
	}
	reference, complete, err := s.resolver.ResolveTag(ctx, namespace, request.Tag)
	if err != nil {
		return nil, err
	}
	return &tag.ResolveTagResponse{
		Reference: reference.GetRawReference(),
		Complete:  complete,
	}, nil
}
