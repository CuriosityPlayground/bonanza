package object

// OutgoingReferences is a list of outgoing references of an object. It
// may either be implemented by a simple slice, or it's possible to
// provide an implementation that reads references directly out of the
// object's contents.
type OutgoingReferences[TReference any] interface {
	GetDegree() int
	GetOutgoingReference(index int) TReference

	// If the OutgoingReferences object is part of a larger object
	// (e.g., part of object.Contents), copy it, so that the
	// original instance may be garbage ollected.
	DetachOutgoingReferences() OutgoingReferences[TReference]
}

// OutgoingReferencesList is a list of outgoing references of an object
// that is backed by a simple slice.
type OutgoingReferencesList []LocalReference

var _ OutgoingReferences[LocalReference] = OutgoingReferencesList{}

func (l OutgoingReferencesList) GetDegree() int {
	return len(l)
}

func (l OutgoingReferencesList) GetOutgoingReference(index int) LocalReference {
	return l[index]
}

func (l OutgoingReferencesList) DetachOutgoingReferences() OutgoingReferences[LocalReference] {
	// Underlying slice is already detached.
	return l
}
