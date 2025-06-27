package grpc

import (
	"context"

	tag_pb "bonanza.build/pkg/proto/storage/tag"
	"bonanza.build/pkg/storage/object"
	"bonanza.build/pkg/storage/tag"

	"google.golang.org/protobuf/types/known/anypb"
)

type grpcUpdater struct {
	client tag_pb.UpdaterClient
}

// NewGRPCUpdater creates a tag updater that forwards all requests to
// update tags to a remote server using gRPC.
func NewGRPCUpdater(client tag_pb.UpdaterClient) tag.Updater[object.GlobalReference, []byte] {
	return &grpcUpdater{
		client: client,
	}
}

func (d *grpcUpdater) UpdateTag(ctx context.Context, tag *anypb.Any, reference object.GlobalReference, lease []byte, overwrite bool) error {
	_, err := d.client.UpdateTag(ctx, &tag_pb.UpdateTagRequest{
		Namespace: reference.GetNamespace().ToProto(),
		Tag:       tag,
		Reference: reference.GetRawReference(),
		Lease:     lease,
		Overwrite: overwrite,
	})
	return err
}
