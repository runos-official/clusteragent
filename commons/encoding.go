// Package commons holds small, dependency-free helpers shared across the
// cluster agent. Today that is the JSON+base64 message codec (JsonB64Encode /
// JsonB64Decode) used to frame instruction payloads on the control-plane
// stream.
package commons

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

func JsonB64Encode(message any) (string, error) {
	// JSON encode the message
	messageBytes, err := json.Marshal(message)
	if err != nil {
		return "", fmt.Errorf("error encoding message: %w", err)
	}

	// Base64 encode the message
	finalMessage := base64.StdEncoding.EncodeToString(messageBytes)

	return finalMessage, nil
}

func JsonB64Decode(encodedMessage string, target any) error {
	// Base64 decode the message
	messageBytes, err := base64.StdEncoding.DecodeString(encodedMessage)
	if err != nil {
		return fmt.Errorf("error decoding base64: %w", err)
	}

	// JSON decode the message into the target interface
	err = json.Unmarshal(messageBytes, target)
	if err != nil {
		return fmt.Errorf("error decoding JSON: %w", err)
	}

	return nil
}
