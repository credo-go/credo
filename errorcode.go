package credo

import "fmt"

// statusToCode is the frozen table mapping HTTP status codes to Credo's
// default machine-readable error codes.
//
// The table was generated once from Go 1.27 http.StatusText values using
// lower-snake-case sanitization (lowercase; spaces and hyphens become "_";
// anything outside [a-z0-9_] is dropped), reviewed, and committed as source.
// It is a wire contract: entries never change when the standard library
// rewords a status text, and any edit is a deliberate wire decision reviewed
// on its own. Runtime code must never derive a wire identity from
// http.StatusText; StatusText remains presentation only.
var statusToCode = map[int]string{
	100: "continue",                        // Continue
	101: "switching_protocols",             // Switching Protocols
	102: "processing",                      // Processing
	103: "early_hints",                     // Early Hints
	200: "ok",                              // OK
	201: "created",                         // Created
	202: "accepted",                        // Accepted
	203: "non_authoritative_information",   // Non-Authoritative Information
	204: "no_content",                      // No Content
	205: "reset_content",                   // Reset Content
	206: "partial_content",                 // Partial Content
	207: "multi_status",                    // Multi-Status
	208: "already_reported",                // Already Reported
	226: "im_used",                         // IM Used
	300: "multiple_choices",                // Multiple Choices
	301: "moved_permanently",               // Moved Permanently
	302: "found",                           // Found
	303: "see_other",                       // See Other
	304: "not_modified",                    // Not Modified
	305: "use_proxy",                       // Use Proxy
	307: "temporary_redirect",              // Temporary Redirect
	308: "permanent_redirect",              // Permanent Redirect
	400: "bad_request",                     // Bad Request
	401: "unauthorized",                    // Unauthorized
	402: "payment_required",                // Payment Required
	403: "forbidden",                       // Forbidden
	404: "not_found",                       // Not Found
	405: "method_not_allowed",              // Method Not Allowed
	406: "not_acceptable",                  // Not Acceptable
	407: "proxy_authentication_required",   // Proxy Authentication Required
	408: "request_timeout",                 // Request Timeout
	409: "conflict",                        // Conflict
	410: "gone",                            // Gone
	411: "length_required",                 // Length Required
	412: "precondition_failed",             // Precondition Failed
	413: "request_entity_too_large",        // Request Entity Too Large
	414: "request_uri_too_long",            // Request URI Too Long
	415: "unsupported_media_type",          // Unsupported Media Type
	416: "requested_range_not_satisfiable", // Requested Range Not Satisfiable
	417: "expectation_failed",              // Expectation Failed
	418: "im_a_teapot",                     // I'm a teapot
	421: "misdirected_request",             // Misdirected Request
	422: "unprocessable_entity",            // Unprocessable Entity
	423: "locked",                          // Locked
	424: "failed_dependency",               // Failed Dependency
	425: "too_early",                       // Too Early
	426: "upgrade_required",                // Upgrade Required
	428: "precondition_required",           // Precondition Required
	429: "too_many_requests",               // Too Many Requests
	431: "request_header_fields_too_large", // Request Header Fields Too Large
	451: "unavailable_for_legal_reasons",   // Unavailable For Legal Reasons
	500: "internal_server_error",           // Internal Server Error
	501: "not_implemented",                 // Not Implemented
	502: "bad_gateway",                     // Bad Gateway
	503: "service_unavailable",             // Service Unavailable
	504: "gateway_timeout",                 // Gateway Timeout
	505: "http_version_not_supported",      // HTTP Version Not Supported
	506: "variant_also_negotiates",         // Variant Also Negotiates
	507: "insufficient_storage",            // Insufficient Storage
	508: "loop_detected",                   // Loop Detected
	510: "not_extended",                    // Not Extended
	511: "network_authentication_required", // Network Authentication Required
}

// defaultCodeForStatus returns the frozen default machine code for an HTTP
// status. A status absent from the frozen table yields the stable,
// collision-resistant fallback "http_<status>" (for example, 499 yields
// "http_499").
func defaultCodeForStatus(status int) string {
	if code, ok := statusToCode[status]; ok {
		return code
	}
	return fmt.Sprintf("http_%d", status)
}

// isValidHTTPStatus reports whether status lies in the valid HTTP status
// domain 100..999.
func isValidHTTPStatus(status int) bool {
	return status >= 100 && status <= 999
}

// isValidErrorCode reports whether code satisfies Credo's machine-code
// grammar: one or more lower-snake-case segments of [a-z0-9], joined by
// single underscores — ^[a-z0-9]+(_[a-z0-9]+)*$. Dots, spaces, hyphens,
// uppercase, and leading/trailing/doubled underscores are all rejected.
func isValidErrorCode(code string) bool {
	if code == "" {
		return false
	}
	previousUnderscore := true // a leading underscore is invalid
	for i := range len(code) {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			previousUnderscore = false
		case c == '_':
			if previousUnderscore {
				return false
			}
			previousUnderscore = true
		default:
			return false
		}
	}
	return !previousUnderscore // a trailing underscore is invalid
}
