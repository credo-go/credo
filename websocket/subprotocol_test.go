package websocket

import "testing"

func TestSubprotocolTokenValidation(t *testing.T) {
	for _, token := range []string{"chat", "echo.v1", "a_b", "!#$%&'*+-.^_`|~", "CaseSensitive"} {
		if !validSubprotocolToken(token) {
			t.Errorf("validSubprotocolToken(%q) = false", token)
		}
	}
	for _, token := range []string{"", "chat room", "chat,echo", "chat/echo", "écho", "\tchat"} {
		if validSubprotocolToken(token) {
			t.Errorf("validSubprotocolToken(%q) = true", token)
		}
	}
}

func TestParseAndSelectSubprotocol(t *testing.T) {
	client, err := parseClientSubprotocols([]string{"chat.v2, chat.v1", "other"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectSubprotocol(client, []string{"chat.v1", "chat.v2"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if selected != "chat.v1" {
		t.Errorf("selected = %q, want server-preferred chat.v1", selected)
	}
	if selected, err := selectSubprotocol([]string{"CHAT.V1"}, []string{"chat.v1"}, false); err == nil || selected != "" {
		t.Errorf("case-insensitive match unexpectedly succeeded: selected=%q err=%v", selected, err)
	}
	if selected, err := selectSubprotocol(nil, nil, false); err != nil || selected != "" {
		t.Errorf("optional empty negotiation = (%q, %v)", selected, err)
	}
	if _, err := selectSubprotocol(nil, []string{"chat"}, true); err == nil {
		t.Fatal("required empty client offer succeeded")
	}
	if _, err := parseClientSubprotocols([]string{"chat,,echo"}); err == nil {
		t.Fatal("empty client token succeeded")
	}
}
