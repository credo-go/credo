// Copyright (c) 2014 Nick Snyder https://github.com/nicksnyder/go-i18n
// License: MIT (https://github.com/nicksnyder/go-i18n/blob/main/LICENSE)
//
// Adapted for the Credo framework (github.com/credo-go/credo).
// Simplified: removed Hash, Description, LeftDelim, RightDelim, NewMessage(data).

package i18n

import (
	"encoding/json"
	"fmt"
)

// Message holds the translations for a single message ID.
// It supports CLDR plural forms; for messages that don't need plurals,
// only the Other field is populated (string shorthand in JSON).
type Message struct {
	// ID is the unique message identifier (e.g., "v.required", "http.404").
	ID string

	// CLDR plural forms.
	Zero  string
	One   string
	Two   string
	Few   string
	Many  string
	Other string // required fallback
}

// messageFromJSON parses a single JSON message value (string or object)
// and returns a Message with the given id.
//
// String value shorthand:
//
//	"v.required": "is required"
//
// Object value with plural forms:
//
//	"v.min_items": {"one": "must have at least {{.min}} item", "other": "must have at least {{.min}} items"}
func messageFromJSON(id string, raw json.RawMessage) (*Message, error) {
	msg := &Message{ID: id}

	// Try string shorthand first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		msg.Other = s
		return msg, nil
	}

	// Try object with plural forms.
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("i18n: message %q: expected string or object, got %s", id, string(raw))
	}

	msg.Zero = obj["zero"]
	msg.One = obj["one"]
	msg.Two = obj["two"]
	msg.Few = obj["few"]
	msg.Many = obj["many"]
	msg.Other = obj["other"]

	if msg.Other == "" && len(obj) > 0 {
		// Deterministic fallback: pick first available in CLDR priority order.
		for _, key := range []string{"one", "few", "many", "two", "zero"} {
			if v, ok := obj[key]; ok {
				msg.Other = v
				break
			}
		}
	}

	return msg, nil
}
