package instructions

import (
	"encoding/json"
	"testing"
)

func TestVCSAuthorizationClassDecoding(t *testing.T) {
	for _, example := range []struct {
		input string
		valid bool
	}{
		{`{}`, true},
		{`{"authorizationClassification":"legacy"}`, true},
		{`{"authorizationClassification":"scoped"}`, true},
		{`{"authorizationClassification":"other"}`, false},
		{`{"authorizationClassification":""}`, false},
		{`{"authorizationClassification":null}`, false},
		{`{"authorizationClassification":true}`, false},
	} {
		for _, target := range []any{new(VcsBuildRequest), new(VcsFetchSourceRequest)} {
			if err := json.Unmarshal([]byte(example.input), target); (err == nil) != example.valid {
				t.Errorf("%T: input %s: error %v, expected valid=%v", target, example.input, err, example.valid)
			}
		}
	}
}
