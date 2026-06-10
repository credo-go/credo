package config

import (
	"reflect"
	"testing"
)

func TestParseDotenvBasic(t *testing.T) {
	data := []byte("KEY=value\nOTHER=123\n")
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{"KEY": "value", "OTHER": "123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseDotenvComments(t *testing.T) {
	data := []byte("# This is a comment\nKEY=value\n# Another comment\n")
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got["KEY"] != "value" {
		t.Errorf("got %v", got)
	}
}

func TestParseDotenvEmptyLines(t *testing.T) {
	data := []byte("\n\nKEY=value\n\n\n")
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got["KEY"] != "value" {
		t.Errorf("got %v", got)
	}
}

func TestParseDotenvExportPrefix(t *testing.T) {
	data := []byte("export KEY=value\nexport OTHER=123\n")
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{"KEY": "value", "OTHER": "123"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseDotenvDoubleQuoted(t *testing.T) {
	data := []byte(`KEY="hello world"
ESCAPED="line1\nline2"
QUOTES="say \"hello\""
BACKSLASH="path\\to\\file"
`)
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		key, want string
	}{
		{"KEY", "hello world"},
		{"ESCAPED", "line1\nline2"},
		{"QUOTES", `say "hello"`},
		{"BACKSLASH", `path\to\file`},
	}
	for _, tt := range tests {
		if got[tt.key] != tt.want {
			t.Errorf("%s: got %q, want %q", tt.key, got[tt.key], tt.want)
		}
	}
}

func TestParseDotenvSingleQuoted(t *testing.T) {
	data := []byte(`KEY='literal $VALUE'
ESCAPES='no \n escapes'
`)
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got["KEY"] != "literal $VALUE" {
		t.Errorf("KEY: got %q, want %q", got["KEY"], "literal $VALUE")
	}
	if got["ESCAPES"] != `no \n escapes` {
		t.Errorf("ESCAPES: got %q, want %q", got["ESCAPES"], `no \n escapes`)
	}
}

func TestParseDotenvWhitespace(t *testing.T) {
	data := []byte("  KEY  =  value  \n")
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["KEY"] != "value" {
		t.Errorf("KEY: got %q, want %q", got["KEY"], "value")
	}
}

func TestParseDotenvMalformed(t *testing.T) {
	data := []byte("MALFORMED_NO_EQUALS\n")
	_, err := parseDotenv(data)
	if err == nil {
		t.Error("expected error for malformed line")
	}
}

func TestParseDotenvEmptyKey(t *testing.T) {
	data := []byte("=value\n")
	_, err := parseDotenv(data)
	if err == nil {
		t.Error("expected error for empty key")
	}
}

func TestParseDotenvEmptyValue(t *testing.T) {
	data := []byte("KEY=\n")
	got, err := parseDotenv(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["KEY"] != "" {
		t.Errorf("KEY: got %q, want empty", got["KEY"])
	}
}
