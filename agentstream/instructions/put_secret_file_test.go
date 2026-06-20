package instructions

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestSecretAlreadyExists pins that the create-vs-update branch keys on the
// typed k8s AlreadyExists error (via k8serrors.IsAlreadyExists), not on a
// substring of the error text. A real apierrors.NewAlreadyExists must route to
// "update"; any other error (including one whose text happens to differ) must
// not.
func TestSecretAlreadyExists(t *testing.T) {
	secretsGR := schema.GroupResource{Group: "", Resource: "secrets"}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "typed AlreadyExists routes to update",
			err:  apierrors.NewAlreadyExists(secretsGR, "my-secret"),
			want: true,
		},
		{
			name: "generic error does not route to update",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "typed NotFound does not route to update",
			err:  apierrors.NewNotFound(secretsGR, "my-secret"),
			want: false,
		},
		{
			name: "nil error does not route to update",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := secretAlreadyExists(tc.err); got != tc.want {
				t.Errorf("secretAlreadyExists(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
