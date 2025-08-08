package main

import (
	"context"
	"net/http"
	"os"

	model_parser "bonanza.build/pkg/model/parser"
	buildqueuestate_pb "bonanza.build/pkg/proto/buildqueuestate"
	"bonanza.build/pkg/proto/configuration/bonanza_browser"
	object_pb "bonanza.build/pkg/proto/storage/object"
	object_grpc "bonanza.build/pkg/storage/object/grpc"

	"github.com/buildbarn/bb-storage/pkg/global"
	bb_http "github.com/buildbarn/bb-storage/pkg/http"
	"github.com/buildbarn/bb-storage/pkg/program"
	"github.com/buildbarn/bb-storage/pkg/util"

	// Included to ensure message of these types are displayable.
	_ "bonanza.build/pkg/proto/model/analysis"
	_ "bonanza.build/pkg/proto/model/command"
	_ "bonanza.build/pkg/proto/model/core"
	_ "bonanza.build/pkg/proto/model/evaluation"
	_ "bonanza.build/pkg/proto/model/filesystem"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func main() {
	program.RunMain(func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
		if len(os.Args) != 2 {
			return status.Error(codes.InvalidArgument, "Usage: bonanza_browser bonanza_browser.jsonnet")
		}
		var configuration bonanza_browser.ApplicationConfiguration
		if err := util.UnmarshalConfigurationFromFile(os.Args[1], &configuration); err != nil {
			return util.StatusWrapf(err, "Failed to read configuration from %s", os.Args[1])
		}
		lifecycleState, grpcClientFactory, err := global.ApplyConfiguration(configuration.Global)
		if err != nil {
			return util.StatusWrap(err, "Failed to apply global configuration options")
		}

		buildQueueStateGRPCClient, err := grpcClientFactory.NewClientFromConfiguration(configuration.BuildQueueStateGrpcClient)
		if err != nil {
			return util.StatusWrap(err, "Failed to create build queue state gRPC client")
		}

		storageGRPCClient, err := grpcClientFactory.NewClientFromConfiguration(configuration.StorageGrpcClient)
		if err != nil {
			return util.StatusWrap(err, "Failed to create storage gRPC client")
		}
		objectDownloader := object_grpc.NewGRPCDownloader(
			object_pb.NewDownloaderClient(storageGRPCClient),
		)
		parsedObjectPool, err := model_parser.NewParsedObjectPoolFromConfiguration(configuration.ParsedObjectPool)
		if err != nil {
			return util.StatusWrap(err, "Failed to create parsed object pool")
		}

		mux := http.NewServeMux()
		browserService := NewBrowserService(
			buildqueuestate_pb.NewBuildQueueStateClient(buildQueueStateGRPCClient),
			objectDownloader,
			parsedObjectPool,
		)
		browserService.RegisterHandlers(mux)
		bb_http.NewServersFromConfigurationAndServe(
			configuration.HttpServers,
			bb_http.NewMetricsHandler(mux, "BrowserUI"),
			siblingsGroup,
			grpcClientFactory,
		)

		lifecycleState.MarkReadyAndWait(siblingsGroup)
		return nil
	})
}
