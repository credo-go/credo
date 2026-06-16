package validation_test

import (
	"errors"
	"mime/multipart"
	"net/textproto"
	"testing"

	"github.com/credo-go/credo/validation"
)

// fileHeader builds a *multipart.FileHeader for tests with the given filename,
// declared content type, and size.
func fileHeader(filename, contentType string, size int64) *multipart.FileHeader {
	h := textproto.MIMEHeader{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &multipart.FileHeader{
		Filename: filename,
		Header:   h,
		Size:     size,
	}
}

// --- MaxFileSize ---

func TestMaxFileSize(t *testing.T) {
	tests := []struct {
		name  string
		file  *multipart.FileHeader
		max   int64
		valid bool
	}{
		{"under limit", fileHeader("a.txt", "text/plain", 500), 1024, true},
		{"at limit", fileHeader("a.txt", "text/plain", 1024), 1024, true},
		{"over limit", fileHeader("a.txt", "text/plain", 1025), 1024, false},
		{"empty file", fileHeader("a.txt", "text/plain", 0), 1024, true},
		{"nil passes", nil, 1024, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validation.MaxFileSize(tt.max).Validate(tt.file)
			if tt.valid && err != nil {
				t.Errorf("expected nil, got %v", err)
			}
			if !tt.valid {
				assertValidationError(t, err, "max_file_size", "")
			}
		})
	}
}

// --- AllowedMimeTypes ---

func TestAllowedMimeTypes(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		allowed     []string
		valid       bool
	}{
		{"exact match", "image/png", []string{"image/png", "image/jpeg"}, true},
		{"second match", "image/jpeg", []string{"image/png", "image/jpeg"}, true},
		{"case insensitive", "Image/PNG", []string{"image/png"}, true},
		{"with params", "text/plain; charset=utf-8", []string{"text/plain"}, true},
		{"allow-list with params", "text/plain", []string{"text/plain; charset=utf-8"}, true},
		{"not allowed", "application/pdf", []string{"image/png"}, false},
		{"missing content type", "", []string{"image/png"}, false},
		{"nil passes", "skip", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var file *multipart.FileHeader
			if tt.contentType != "skip" {
				file = fileHeader("f", tt.contentType, 1)
			}
			err := validation.AllowedMimeTypes(tt.allowed...).Validate(file)
			if tt.valid && err != nil {
				t.Errorf("expected nil, got %v", err)
			}
			if !tt.valid {
				assertValidationError(t, err, "allowed_mime_types", "")
			}
		})
	}
}

// --- AllowedExtensions ---

func TestAllowedExtensions(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		allowed  []string
		valid    bool
	}{
		{"dotted allow-list", "photo.jpg", []string{".jpg", ".png"}, true},
		{"bare allow-list", "photo.jpg", []string{"jpg", "png"}, true},
		{"case insensitive name", "PHOTO.JPG", []string{"jpg"}, true},
		{"case insensitive allow", "photo.jpg", []string{"JPG"}, true},
		{"not allowed", "doc.pdf", []string{"jpg", "png"}, false},
		{"no extension", "README", []string{"jpg"}, false},
		{"compound extension uses last", "archive.tar.gz", []string{"gz"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := fileHeader(tt.filename, "application/octet-stream", 1)
			err := validation.AllowedExtensions(tt.allowed...).Validate(file)
			if tt.valid && err != nil {
				t.Errorf("expected nil, got %v", err)
			}
			if !tt.valid {
				assertValidationError(t, err, "allowed_extensions", "")
			}
		})
	}
}

func TestAllowedExtensions_NilPasses(t *testing.T) {
	if err := validation.AllowedExtensions("jpg").Validate(nil); err != nil {
		t.Errorf("expected nil for nil file, got %v", err)
	}
}

// --- Integration: file rules inside ValidateStruct with Required ---

type uploadForm struct {
	Avatar *multipart.FileHeader `json:"avatar"`
}

func (f *uploadForm) Validate() error {
	return validation.ValidateStruct(f,
		validation.Field(&f.Avatar,
			validation.Required[*multipart.FileHeader](),
			validation.MaxFileSize(1024),
			validation.AllowedExtensions("png"),
		),
	)
}

func TestFileRules_StructIntegration(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		f := &uploadForm{Avatar: fileHeader("a.png", "image/png", 512)}
		if err := f.Validate(); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("missing file fails Required", func(t *testing.T) {
		f := &uploadForm{Avatar: nil}
		err := f.Validate()
		if err == nil {
			t.Fatal("expected error for missing file")
		}
		errs, ok := errors.AsType[validation.Errors](err)
		if !ok {
			t.Fatalf("expected validation.Errors, got %T", err)
		}
		if errs[0].Field != "avatar" {
			t.Errorf("field = %q, want %q", errs[0].Field, "avatar")
		}
		if errs[0].Code != "required" {
			t.Errorf("code = %q, want %q", errs[0].Code, "required")
		}
	})

	t.Run("oversize and wrong extension both reported", func(t *testing.T) {
		f := &uploadForm{Avatar: fileHeader("a.gif", "image/gif", 2048)}
		err := f.Validate()
		errs, ok := errors.AsType[validation.Errors](err)
		if !ok {
			t.Fatalf("expected validation.Errors, got %T: %v", err, err)
		}
		if len(errs) != 2 {
			t.Fatalf("expected 2 errors, got %d: %v", len(errs), errs)
		}
		for _, ve := range errs {
			if ve.Field != "avatar" {
				t.Errorf("field = %q, want %q", ve.Field, "avatar")
			}
		}
	})
}
