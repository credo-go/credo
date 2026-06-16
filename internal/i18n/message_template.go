package i18n

import (
	"fmt"

	"golang.org/x/text/feature/plural"
)

// messageTemplate pairs a Message with pre-compiled templates for each plural form.
type messageTemplate struct {
	*Message
	pluralTemplates map[plural.Form]*tmpl
}

func newMessageTemplate(msg *Message) (*messageTemplate, error) {
	mt := &messageTemplate{
		Message:         msg,
		pluralTemplates: make(map[plural.Form]*tmpl),
	}
	forms := []struct {
		form plural.Form
		src  string
	}{
		{plural.Zero, msg.Zero},
		{plural.One, msg.One},
		{plural.Two, msg.Two},
		{plural.Few, msg.Few},
		{plural.Many, msg.Many},
		{plural.Other, msg.Other},
	}
	for _, f := range forms {
		if f.src == "" {
			continue
		}
		t, err := newTemplate(f.src)
		if err != nil {
			return nil, fmt.Errorf("i18n: invalid template for message %q: %w", msg.ID, err)
		}
		mt.pluralTemplates[f.form] = t
	}
	return mt, nil
}

// execute renders the appropriate plural form template with data.
func (mt *messageTemplate) execute(form plural.Form, data any) (string, error) {
	t, ok := mt.pluralTemplates[form]
	if !ok {
		// Fallback to Other form.
		t, ok = mt.pluralTemplates[plural.Other]
		if !ok {
			return mt.ID, nil
		}
	}
	return t.execute(data)
}
