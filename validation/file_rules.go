package validation

import (
	"fmt"
	"mime"
	"mime/multipart"
	"path/filepath"
	"strings"
)

// File validation rules operate on *multipart.FileHeader, the value produced
// by net/http's multipart parser for each uploaded file. They are designed to
// be used inside a Validatable.Validate method alongside the other rules:
//
//	type UploadForm struct {
//	    Avatar *multipart.FileHeader
//	}
//
//	func (f *UploadForm) Validate() error {
//	    return validation.ValidateStruct(f,
//	        validation.Field(&f.Avatar,
//	            validation.Required[*multipart.FileHeader](),
//	            validation.MaxFileSize(5<<20), // 5 MiB
//	            validation.AllowedExtensions("jpg", "png"),
//	        ),
//	    )
//	}
//
// A nil *multipart.FileHeader passes every file rule — use [Required] to
// enforce that a file was actually uploaded.

// MaxFileSize creates a [Rule] that fails if the uploaded file is larger than
// maxBytes. The size is read from [multipart.FileHeader.Size], which the
// multipart parser fills in from the request. A nil file header passes — use
// [Required] to enforce presence.
func MaxFileSize(maxBytes int64) Rule[*multipart.FileHeader] {
	return &maxFileSizeRule{max: maxBytes}
}

type maxFileSizeRule struct {
	max int64
}

func (r *maxFileSizeRule) Validate(value *multipart.FileHeader) error {
	if value == nil {
		return nil
	}
	if value.Size > r.max {
		return newRuleError("max_file_size",
			fmt.Sprintf("file size must not exceed %d bytes", r.max),
			map[string]any{"max": r.max, "size": value.Size},
		)
	}
	return nil
}

// AllowedMimeTypes creates a [Rule] that restricts the uploaded file's declared
// MIME type to the given list. The MIME type is read from the part's
// Content-Type header, which is client-supplied and therefore not
// authoritative — pair it with content sniffing (e.g. http.DetectContentType)
// when stronger guarantees are needed. Comparison is case-insensitive and
// ignores media-type parameters such as "; charset=utf-8". A nil file header
// passes — use [Required] to enforce presence.
func AllowedMimeTypes(types ...string) Rule[*multipart.FileHeader] {
	allowed := make(map[string]struct{}, len(types))
	for _, t := range types {
		if n := normalizeMime(t); n != "" {
			allowed[n] = struct{}{}
		}
	}
	return &allowedMimeTypesRule{allowed: allowed, types: types}
}

type allowedMimeTypesRule struct {
	allowed map[string]struct{}
	types   []string
}

func (r *allowedMimeTypesRule) Validate(value *multipart.FileHeader) error {
	if value == nil {
		return nil
	}
	got := normalizeMime(value.Header.Get("Content-Type"))
	if _, ok := r.allowed[got]; !ok {
		return newRuleError("allowed_mime_types",
			fmt.Sprintf("file type must be one of: %s", strings.Join(r.types, ", ")),
			map[string]any{"allowed": r.types, "got": got},
		)
	}
	return nil
}

// AllowedExtensions creates a [Rule] that restricts the uploaded file's
// extension (taken from [multipart.FileHeader.Filename]) to the given list.
// Extensions are compared case-insensitively, and a leading dot is optional in
// the allow-list — "jpg" and ".jpg" are equivalent. A file whose name has no
// extension fails unless "" is explicitly allowed. A nil file header passes —
// use [Required] to enforce presence.
func AllowedExtensions(exts ...string) Rule[*multipart.FileHeader] {
	allowed := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		allowed[normalizeExt(e)] = struct{}{}
	}
	return &allowedExtensionsRule{allowed: allowed, exts: exts}
}

type allowedExtensionsRule struct {
	allowed map[string]struct{}
	exts    []string
}

func (r *allowedExtensionsRule) Validate(value *multipart.FileHeader) error {
	if value == nil {
		return nil
	}
	got := normalizeExt(filepath.Ext(value.Filename))
	if _, ok := r.allowed[got]; !ok {
		return newRuleError("allowed_extensions",
			fmt.Sprintf("file extension must be one of: %s", strings.Join(r.exts, ", ")),
			map[string]any{"allowed": r.exts, "got": got},
		)
	}
	return nil
}

// normalizeMime lowercases a MIME type and strips any media-type parameters
// (e.g. "Text/Plain; charset=utf-8" → "text/plain"). It returns "" for the
// empty string.
func normalizeMime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if mt, _, err := mime.ParseMediaType(s); err == nil {
		return mt // ParseMediaType lowercases the media type.
	}
	return strings.ToLower(s)
}

// normalizeExt lowercases a file extension and ensures a single leading dot.
// It returns "" for the empty string (a name with no extension).
func normalizeExt(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, ".") {
		s = "." + s
	}
	return s
}
