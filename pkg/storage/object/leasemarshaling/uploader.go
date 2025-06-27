package leasemarshaling

import (
	"context"
	"sync/atomic"

	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"
)

// LeaseMarshaler is used by the implementations of object.Downloader
// and object.Uploader to convert leases from their native
// representation to byte slices, and vice versa.
type LeaseMarshaler[TLease any] interface {
	MarshalLease(lease TLease, dst []byte) []byte
	UnmarshalLease(src []byte) (TLease, error)
}

// LeaseMarshalerForTesting is an instantiation of LeaseMarshaler that
// is used to generate mocks as part of tests.
type LeaseMarshalerForTesting LeaseMarshaler[any]

type uploader[TReference any, TLease any] struct {
	base                  object.Uploader[TReference, TLease]
	marshaler             LeaseMarshaler[TLease]
	maximumLeaseSizeBytes atomic.Int64
}

// NewUploader creates a decorator for object.Uploader that converts
// leases in the format of byte slices from/to the native representation
// of the storage backend. This is typically needed if a storage backend
// is exposed via the network.
func NewUploader[TReference, TLease any](base object.Uploader[TReference, TLease], marshaler LeaseMarshaler[TLease]) object.Uploader[TReference, []byte] {
	return &uploader[TReference, TLease]{
		base:      base,
		marshaler: marshaler,
	}
}

func (u *uploader[TReference, TLease]) UploadObject(ctx context.Context, reference TReference, contents *object.Contents, childrenLeases [][]byte, wantContentsIfIncomplete bool) (object.UploadObjectResult[[]byte], error) {
	// Unmarshal the leases of child objects.
	unmarshaledChildrenLeases := make([]TLease, 0, len(childrenLeases))
	for i, marshaledLease := range childrenLeases {
		var unmarshaledLease TLease
		if len(marshaledLease) > 0 {
			var err error
			unmarshaledLease, err = u.marshaler.UnmarshalLease(marshaledLease)
			if err != nil {
				return nil, util.StatusWrapf(err, "Invalid lease at index %d", i)
			}
		}
		unmarshaledChildrenLeases = append(unmarshaledChildrenLeases, unmarshaledLease)
	}

	result, err := u.base.UploadObject(ctx, reference, contents, unmarshaledChildrenLeases, wantContentsIfIncomplete)
	if err != nil {
		return nil, err
	}

	switch resultType := result.(type) {
	case object.UploadObjectComplete[TLease]:
		// Marshal the lease contained in the result. Save the
		// maximum observed size, so that future calls can
		// immediately allocate the right amount of space.
		maximumLeaseSizeBytes := u.maximumLeaseSizeBytes.Load()
		marshaledLease := u.marshaler.MarshalLease(resultType.Lease, make([]byte, 0, maximumLeaseSizeBytes))
		if l := int64(len(marshaledLease)); maximumLeaseSizeBytes < l {
			u.maximumLeaseSizeBytes.Store(l)
		}

		return object.UploadObjectComplete[[]byte]{
			Lease: marshaledLease,
		}, nil
	case object.UploadObjectIncomplete[TLease]:
		return object.UploadObjectIncomplete[[]byte]{
			Contents:                     resultType.Contents,
			WantOutgoingReferencesLeases: resultType.WantOutgoingReferencesLeases,
		}, nil
	case object.UploadObjectMissing[TLease]:
		return object.UploadObjectMissing[[]byte]{}, nil
	default:
		panic("unknown upload object result type")
	}
}
