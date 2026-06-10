// Copyright (c) 2014 Nick Snyder https://github.com/nicksnyder/go-i18n
// License: MIT (https://github.com/nicksnyder/go-i18n/blob/main/LICENSE)
//
// Adapted for the Credo framework (github.com/credo-go/credo).
// Simplified: no custom delimiters, no Parser interface, no IdentityParser.

package i18n

import (
	"bytes"
	"strings"
	"sync"
	"text/template"
)

// bufPool recycles render buffers across executes: every templated
// translation (notably each validation error in a 4xx response) would
// otherwise allocate a fresh buffer.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// tmpl wraps a text/template with a pre-parsed template.
//
// text/template is deliberate (matching go-i18n upstream): messages are
// plain text rendered into JSON bodies and logs, so html/template's
// unconditional HTML escaping would corrupt them. The trust model is that
// locale files are developer-controlled code artifacts — review them like
// code (templates can call methods on the data passed to T). Escaping for
// HTML output is the responsibility of the HTML rendering layer.
type tmpl struct {
	src    string
	parsed *template.Template
}

// newTemplate creates a tmpl and eagerly parses the source if it contains
// template delimiters. Returns an error for malformed templates.
func newTemplate(src string) (*tmpl, error) {
	t := &tmpl{src: src}
	if strings.Contains(src, "{{") {
		parsed, err := template.New("").Option("missingkey=default").Parse(src)
		if err != nil {
			return nil, err
		}
		t.parsed = parsed
	}
	return t, nil
}

// execute renders the template with the given data.
// Fast path: if no template delimiters, returns the source directly (zero alloc).
func (t *tmpl) execute(data any) (string, error) {
	if t.parsed == nil {
		return t.src, nil
	}
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	if err := t.parsed.Execute(buf, data); err != nil {
		return "", err
	}
	// buf.String() copies, so the buffer is safe to recycle.
	return buf.String(), nil
}
