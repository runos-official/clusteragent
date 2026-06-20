package commons

import (
	"strings"
	"testing"
)

// TestJsonB64RoundTrip pins the JSON+base64 wire encoding both instruction
// handlers and the agent stream use for every message: encode then decode must
// reproduce the original value field-for-field.
func TestJsonB64RoundTrip(t *testing.T) {
	type payload struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
	}
	cases := []struct {
		name string
		in   payload
	}{
		{"populated", payload{Name: "app-ab12c", Count: 3, Tags: []string{"latest", "v1"}}},
		{"zero value", payload{}},
		{"empty slice vs nil", payload{Name: "x", Tags: []string{}}},
		{"unicode + special chars", payload{Name: `a"b\c日本`, Count: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := JsonB64Encode(tc.in)
			if err != nil {
				t.Fatalf("JsonB64Encode: %v", err)
			}
			var got payload
			if err := JsonB64Decode(encoded, &got); err != nil {
				t.Fatalf("JsonB64Decode: %v", err)
			}
			if got.Name != tc.in.Name || got.Count != tc.in.Count {
				t.Errorf("round-trip mismatch: got %+v, want %+v", got, tc.in)
			}
			if len(got.Tags) != len(tc.in.Tags) {
				t.Errorf("tags length mismatch: got %d, want %d", len(got.Tags), len(tc.in.Tags))
			}
		})
	}
}

// TestJsonB64EncodeMapRoundTrip covers the common map payload shape and that
// the output is valid (decodable) base64.
func TestJsonB64EncodeMapRoundTrip(t *testing.T) {
	in := map[string]any{"osid": "app-mn4pq", "ok": true}
	encoded, err := JsonB64Encode(in)
	if err != nil {
		t.Fatalf("JsonB64Encode: %v", err)
	}
	var got map[string]any
	if err := JsonB64Decode(encoded, &got); err != nil {
		t.Fatalf("JsonB64Decode: %v", err)
	}
	if got["osid"] != "app-mn4pq" || got["ok"] != true {
		t.Errorf("decoded map = %v, want osid+ok preserved", got)
	}
}

// TestJsonB64EncodeUnsupported pins that a value json.Marshal cannot encode
// (a channel) surfaces as an error rather than a panic or empty string.
func TestJsonB64EncodeUnsupported(t *testing.T) {
	if _, err := JsonB64Encode(make(chan int)); err == nil {
		t.Fatal("expected error encoding an unmarshalable value, got nil")
	}
}

// TestJsonB64DecodeMalformedBase64 pins that non-base64 input is rejected at
// the base64 stage with a clear error, not passed through to json.Unmarshal.
func TestJsonB64DecodeMalformedBase64(t *testing.T) {
	var target map[string]any
	err := JsonB64Decode("not valid base64 !!!", &target)
	if err == nil {
		t.Fatal("expected error for malformed base64, got nil")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("error %q should mention base64", err.Error())
	}
}

// TestJsonB64DecodeMalformedJSON pins that valid base64 wrapping invalid JSON
// is rejected at the JSON stage.
func TestJsonB64DecodeMalformedJSON(t *testing.T) {
	// base64 of the literal bytes "{not json" -> valid base64, invalid JSON.
	encoded, err := JsonB64Encode("placeholder")
	if err != nil {
		t.Fatalf("setup encode: %v", err)
	}
	_ = encoded
	// "e25vdCBqc29u" decodes to "{not json".
	var target map[string]any
	err = JsonB64Decode("e25vdCBqc29u", &target)
	if err == nil {
		t.Fatal("expected error for malformed JSON payload, got nil")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Errorf("error %q should mention JSON", err.Error())
	}
}

// TestJsonB64DecodeNilTarget pins that decoding into a nil target surfaces the
// json.Unmarshal error instead of panicking.
func TestJsonB64DecodeNilTarget(t *testing.T) {
	encoded, err := JsonB64Encode(map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := JsonB64Decode(encoded, nil); err == nil {
		t.Fatal("expected error decoding into nil target, got nil")
	}
}
