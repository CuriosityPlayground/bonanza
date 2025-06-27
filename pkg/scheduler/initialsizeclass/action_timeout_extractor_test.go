package initialsizeclass_test

import (
	"testing"
	"time"

	remoteexecution_pb "bonanza.build/pkg/proto/remoteexecution"
	"bonanza.build/pkg/scheduler/initialsizeclass"

	"github.com/buildbarn/bb-storage/pkg/testutil"
	"github.com/stretchr/testify/require"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestActionTimeoutExtractor(t *testing.T) {
	actionTimeoutExtractor := initialsizeclass.NewActionTimeoutExtractor(2 * time.Hour)

	t.Run("MissingExecutionTimeout", func(t *testing.T) {
		// Whereas REv2 allows the timeout to be omitted, this
		// protocol requires that an explicit timeout is
		// provided. Clients must provide explicit timeouts for
		// their actions.
		_, err := actionTimeoutExtractor.ExtractTimeout(&remoteexecution_pb.Action{})
		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "No execution timeout provided"), err)
	})

	t.Run("InvalidTimeout", func(t *testing.T) {
		_, err := actionTimeoutExtractor.ExtractTimeout(&remoteexecution_pb.Action{
			AdditionalData: &remoteexecution_pb.Action_AdditionalData{
				ExecutionTimeout: &durationpb.Duration{Nanos: 1000000000},
			},
		})
		testutil.RequirePrefixedStatus(t, status.Error(codes.InvalidArgument, "Invalid execution timeout: "), err)
	})

	t.Run("TimeoutTooLow", func(t *testing.T) {
		_, err := actionTimeoutExtractor.ExtractTimeout(&remoteexecution_pb.Action{
			AdditionalData: &remoteexecution_pb.Action_AdditionalData{
				ExecutionTimeout: &durationpb.Duration{Seconds: -1},
			},
		})
		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "Execution timeout of -1s is outside permitted range [0s, 2h0m0s]"), err)
	})

	t.Run("TimeoutTooHigh", func(t *testing.T) {
		_, err := actionTimeoutExtractor.ExtractTimeout(&remoteexecution_pb.Action{
			AdditionalData: &remoteexecution_pb.Action_AdditionalData{
				ExecutionTimeout: &durationpb.Duration{Seconds: 7201},
			},
		})
		testutil.RequireEqualStatus(t, status.Error(codes.InvalidArgument, "Execution timeout of 2h0m1s is outside permitted range [0s, 2h0m0s]"), err)
	})

	t.Run("Success", func(t *testing.T) {
		timeout, err := actionTimeoutExtractor.ExtractTimeout(&remoteexecution_pb.Action{
			AdditionalData: &remoteexecution_pb.Action_AdditionalData{
				ExecutionTimeout: &durationpb.Duration{Seconds: 900},
			},
		})
		require.NoError(t, err)
		require.Equal(t, 15*time.Minute, timeout)
	})
}
