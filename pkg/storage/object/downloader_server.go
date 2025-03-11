package object

import (
	"context"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/buildbarn/bonanza/pkg/proto/storage/object"
)

type downloaderServer struct {
	downloader Downloader[GlobalReference]
}

// NewDownloaderServer creates a gRPC server object for exposing a
// Downloader. This can be used to make objects contained in an object
// store accessible via the network.
func NewDownloaderServer(downloader Downloader[GlobalReference]) object.DownloaderServer {
	return &downloaderServer{
		downloader: downloader,
	}
}

func (s *downloaderServer) DownloadObject(ctx context.Context, request *object.DownloadObjectRequest) (*object.DownloadObjectResponse, error) {
	namespace, err := NewNamespace(request.Namespace)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid namespace")
	}
	reference, err := namespace.NewGlobalReference(request.Reference)
	if err != nil {
		return nil, util.StatusWrap(err, "Invalid reference")
	}
	contents, err := s.downloader.DownloadObject(ctx, reference)
	if err != nil {
		return nil, err
	}
	return &object.DownloadObjectResponse{
		Contents: contents.GetFullData(),
	}, nil
}
