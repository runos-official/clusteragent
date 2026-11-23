package instructions

import (
	"encoding/json"
	"errors"

	"github.com/runos-official/clusteragent/datastore"
)

// Omitted classification preserves legacy senders. A present classification must name a supported protocol value.
type vcsAuthorizationClass string

func (class *vcsAuthorizationClass) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch value {
	case datastore.AuthorizationLegacy, datastore.AuthorizationScoped:
		*class = vcsAuthorizationClass(value)
		return nil
	default:
		return errors.New("unknown VCS authorization classification")
	}
}
