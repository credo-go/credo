package httpclient

import (
	"errors"
	"fmt"
	"net/http"
)

const methodQuery = "QUERY"

var queryBodyHeaders = [...]string{
	"Content-Type",
	"Content-Encoding",
	"Content-Language",
	"Content-Location",
}

// checkRedirect retains net/http's default ten-hop limit and repairs the
// historical 301/302 non-GET-to-GET rewrite for RFC 10008 QUERY requests.
// URL resolution, sensitive-header stripping, cookie-jar handling, Referer,
// response draining, and the redirect loop remain owned by net/http.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 || req.Response == nil {
		return nil
	}
	previous := via[len(via)-1]
	if previous.Method != methodQuery || req.Method != http.MethodGet {
		return nil
	}
	if status := req.Response.StatusCode; status != http.StatusMovedPermanently && status != http.StatusFound {
		return nil
	}

	original := via[0]
	hasBody := original.Body != nil && original.Body != http.NoBody
	if hasBody && original.GetBody == nil {
		return http.ErrUseLastResponse
	}

	req.Method = methodQuery
	restoreQueryBodyHeaders(req.Header, original.Header)
	if !hasBody {
		return nil
	}

	body, err := original.GetBody()
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return fmt.Errorf("httpclient: replay QUERY redirect body: %w", err)
	}
	req.Body = body
	req.GetBody = original.GetBody
	req.ContentLength = original.ContentLength
	return nil
}

func restoreQueryBodyHeaders(dst, src http.Header) {
	for _, name := range queryBodyHeaders {
		dst.Del(name)
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}
