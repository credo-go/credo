package credo

import (
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// serveDirListing renders a minimal HTML directory listing.
func serveDirListing(ctx *Context, fsys fs.FS, cleanPath, urlPath string, cfg StaticConfig) error {
	entries, err := fs.ReadDir(fsys, cleanPath)
	if err != nil {
		return ErrNotFound
	}

	cacheCtx := setStaticHeaders(ctx, cleanPath, directoryListingName(urlPath), cfg)
	ctx.Response().Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx.Response().Header().Set("X-Content-Type-Options", "nosniff")
	applyStaticCacheControl(ctx.Response(), cacheCtx, cfg, http.StatusOK)
	ctx.Response().WriteHeader(http.StatusOK)

	// Determine the display path for the heading.
	displayPath := "/" + urlPath
	if !strings.HasSuffix(displayPath, "/") {
		displayPath += "/"
	}

	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head><meta charset=\"utf-8\"><title>Index of ")
	b.WriteString(html.EscapeString(displayPath))
	b.WriteString("</title></head>\n<body>\n<h1>Index of ")
	b.WriteString(html.EscapeString(displayPath))
	b.WriteString("</h1>\n<table>\n<tr><th>Name</th><th>Size</th><th>Modified</th></tr>\n")

	// Parent directory link.
	if urlPath != "" && urlPath != "." {
		b.WriteString("<tr><td><a href=\"../\">../</a></td><td>-</td><td>-</td></tr>\n")
	}

	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		name := entry.Name()
		displayName := name
		// URL-encode the name for the href attribute, then append
		// "/" for directories. html.EscapeString is applied on top
		// so that the already-percent-encoded value is safe inside
		// an HTML attribute.
		hrefName := url.PathEscape(name)
		if entry.IsDir() {
			displayName += "/"
			hrefName += "/"
		}

		size := "-"
		if !entry.IsDir() {
			size = formatFileSize(info.Size())
		}

		modified := info.ModTime().Format("2006-01-02 15:04:05")

		b.WriteString("<tr><td><a href=\"")
		b.WriteString(html.EscapeString(hrefName))
		b.WriteString("\">")
		b.WriteString(html.EscapeString(displayName))
		b.WriteString("</a></td><td>")
		b.WriteString(size)
		b.WriteString("</td><td>")
		b.WriteString(modified)
		b.WriteString("</td></tr>\n")
	}

	b.WriteString("</table>\n</body>\n</html>\n")

	_, writeErr := fmt.Fprint(ctx.Response(), b.String())
	return writeErr
}

// directoryListingName returns a stable download name for HTML listings.
func directoryListingName(urlPath string) string {
	trimmed := strings.Trim(strings.TrimSpace(urlPath), "/")
	if trimmed == "" || trimmed == "." {
		return "listing.html"
	}

	name := path.Base(trimmed)
	if name == "" || name == "." || name == "/" {
		return "listing.html"
	}
	return name + ".html"
}

// formatFileSize formats a byte count into a human-readable string.
func formatFileSize(bytes int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
