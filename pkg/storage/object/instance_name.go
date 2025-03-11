package object

// InstanceName denotes the name of a namespace in storage.
//
// In this implementation instance names can have arbitrary string
// values. This differs from REv2, where the instance name is path-like
// and cannot contain certain keywords.
type InstanceName struct {
	value string
}

// NewInstanceName creates a new InstanceName that corresponds to the
// provided value.
func NewInstanceName(value string) InstanceName {
	return InstanceName{
		value: value,
	}
}

// WithLocalReference upgrades a LocalReference to a GlobalReference
// that is associated with the current instance name, allowing it to be
// used as part of an RPC.
func (in InstanceName) WithLocalReference(localReference LocalReference) GlobalReference {
	return GlobalReference{
		InstanceName:   in,
		LocalReference: localReference,
	}
}
