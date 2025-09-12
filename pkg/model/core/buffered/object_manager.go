package buffered

import (
	"context"

	model_core "bonanza.build/pkg/model/core"
	model_parser "bonanza.build/pkg/model/parser"
	dag_pb "bonanza.build/pkg/proto/storage/dag"
	"bonanza.build/pkg/storage/dag"
	"bonanza.build/pkg/storage/object"

	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Reference struct {
	object.LocalReference
	embeddedMetadata ReferenceMetadata
}

type ReferenceMetadata struct {
	contents *object.Contents
	children []ReferenceMetadata
}

func (ReferenceMetadata) Discard()     {}
func (ReferenceMetadata) IsCloneable() {}

func (m ReferenceMetadata) GetContents(ctx context.Context) (*object.Contents, []dag.ObjectContentsWalker, error) {
	if m.contents == nil {
		return nil, nil, status.Error(codes.Internal, "Contents for this object are not available for upload, as this object was expected to already exist")
	}

	walkers := make([]dag.ObjectContentsWalker, 0, len(m.children))
	for _, child := range m.children {
		walkers = append(walkers, child)
	}
	return m.contents, walkers, nil
}

type objectManager struct{}

func NewObjectManager() model_core.ObjectManager[Reference, ReferenceMetadata] {
	return objectManager{}
}

func (objectManager) CaptureCreatedObject(createdObject model_core.CreatedObject[ReferenceMetadata]) ReferenceMetadata {
	return ReferenceMetadata{
		contents: createdObject.Contents,
		children: createdObject.Metadata,
	}
}

func (objectManager) CaptureExistingObject(reference Reference) ReferenceMetadata {
	if reference.embeddedMetadata.contents != nil {
		return reference.embeddedMetadata
	}
	return ReferenceMetadata{}
}

func (objectManager) ReferenceObject(capturedObject model_core.CapturedObject[ReferenceMetadata]) Reference {
	return Reference{
		LocalReference:   capturedObject.LocalReference,
		embeddedMetadata: capturedObject.Metadata,
	}
}

type objectExporter struct {
	dagUploaderClient             dag_pb.UploaderClient
	instanceName                  object.InstanceName
	objectContentsWalkerSemaphore *semaphore.Weighted
}

func NewObjectExporter(
	dagUploaderClient dag_pb.UploaderClient,
	instanceName object.InstanceName,
	objectContentsWalkerSemaphore *semaphore.Weighted,
) model_core.ObjectExporter[Reference, object.LocalReference] {
	return &objectExporter{
		dagUploaderClient:             dagUploaderClient,
		instanceName:                  instanceName,
		objectContentsWalkerSemaphore: objectContentsWalkerSemaphore,
	}
}

func (oe *objectExporter) ExportReference(ctx context.Context, internalReference Reference) (object.LocalReference, error) {
	err := dag.UploadDAG(
		ctx,
		oe.dagUploaderClient,
		oe.instanceName.WithLocalReference(internalReference.LocalReference),
		internalReference.embeddedMetadata,
		oe.objectContentsWalkerSemaphore,
		// Assume everything we attempt to upload is memory backed.
		object.Unlimited,
	)
	if err != nil {
		var badReference object.LocalReference
		return badReference, nil
	}
	return internalReference.LocalReference, nil
}

func (objectExporter) ImportReference(externalReference object.LocalReference) Reference {
	return Reference{LocalReference: externalReference}
}

type parsedObjectReader struct {
	base model_parser.ParsedObjectReader[object.LocalReference, model_core.Message[[]byte, object.LocalReference]]
}

func NewParsedObjectReader(
	base model_parser.ParsedObjectReader[object.LocalReference, model_core.Message[[]byte, object.LocalReference]],
) model_parser.ParsedObjectReader[Reference, model_core.Message[[]byte, Reference]] {
	return &parsedObjectReader{
		base: base,
	}
}

func (r *parsedObjectReader) ReadParsedObject(ctx context.Context, reference Reference) (model_core.Message[[]byte, Reference], error) {
	if contents := reference.embeddedMetadata.contents; contents != nil {
		// Object has not been written to storage yet.
		// Return the copy that lives in memory.
		//
		// TODO: We should return some kind of hint to indicate
		// that the caller is not permitted to cache this!
		degree := contents.GetDegree()
		outgoingReferences := make(object.OutgoingReferencesList[Reference], 0, degree)
		children := reference.embeddedMetadata.children
		for i := range degree {
			outgoingReferences = append(outgoingReferences, Reference{
				LocalReference:   contents.GetOutgoingReference(i),
				embeddedMetadata: children[i],
			})
		}
		return model_core.NewMessage(contents.GetPayload(), outgoingReferences), nil
	}

	// Read object from storage.
	m, err := r.base.ReadParsedObject(ctx, reference.GetLocalReference())
	if err != nil {
		return model_core.Message[[]byte, Reference]{}, err
	}

	degree := m.OutgoingReferences.GetDegree()
	outgoingReferences := make(object.OutgoingReferencesList[Reference], 0, degree)
	for i := range degree {
		outgoingReferences = append(outgoingReferences, Reference{
			LocalReference: m.OutgoingReferences.GetOutgoingReference(i),
		})
	}
	return model_core.NewMessage(m.Message, outgoingReferences), nil
}

func (r *parsedObjectReader) GetDecodingParametersSizeBytes() int {
	return 0
}
