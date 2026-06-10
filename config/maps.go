// Copyright (c) 2019 Kailash Nadh.
// Derived from github.com/knadh/koanf/maps (MIT License).

package config

import (
	"fmt"
	"strings"
)

// lookup walks the nested map along the dotted key path. The boolean
// reports presence, so nil values (e.g. JSON null) are distinguishable
// from missing keys. Dots always act as path separators: a literal key
// containing a dot is not addressable.
func lookup(m map[string]any, key string) (any, bool) {
	var val any
	for part := range strings.SplitSeq(key, keyDelim) {
		v, ok := m[part]
		if !ok {
			return nil, false
		}
		val = v
		m, _ = v.(map[string]any)
	}
	return val, true
}

// unflatten converts a flat map with dotted keys into a nested map.
//
// Example:
//
//	{"server.port": 8080} → {"server": {"port": 8080}}
func unflatten(m map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range m {
		parts := strings.Split(k, keyDelim)
		setNested(out, parts, v)
	}
	return out
}

// setNested sets a value in a nested map at the given path, creating
// intermediate maps as needed.
func setNested(m map[string]any, path []string, val any) {
	for i, p := range path {
		if i == len(path)-1 {
			m[p] = val
			return
		}
		if sub, ok := m[p]; ok {
			if subMap, ok := sub.(map[string]any); ok {
				m = subMap
			} else {
				// Overwrite non-map value with a new map.
				newMap := make(map[string]any)
				m[p] = newMap
				m = newMap
			}
		} else {
			newMap := make(map[string]any)
			m[p] = newMap
			m = newMap
		}
	}
}

// mergeMaps recursively merges src into dst. Values in src take precedence.
// Maps are merged recursively; all other types are overwritten.
func mergeMaps(src, dst map[string]any) {
	for k, sv := range src {
		dv, exists := dst[k]
		if !exists {
			dst[k] = sv
			continue
		}

		srcMap, srcOK := sv.(map[string]any)
		dstMap, dstOK := dv.(map[string]any)
		if srcOK && dstOK {
			mergeMaps(srcMap, dstMap)
		} else {
			dst[k] = sv
		}
	}
}

// copyMap performs a recursive deep copy of a configuration map.
// It handles map[string]any (recursive), []any (shallow element copy),
// and primitive types (direct assignment).
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			out[k] = copyMap(val)
		case []any:
			s := make([]any, len(val))
			for i, elem := range val {
				if sub, ok := elem.(map[string]any); ok {
					s[i] = copyMap(sub)
				} else {
					s[i] = elem
				}
			}
			out[k] = s
		default:
			out[k] = v
		}
	}
	return out
}

// intfaceKeysToStrings recursively converts any map[any]any values
// (produced by some YAML parsers) to map[string]any.
func intfaceKeysToStrings(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = convertValue(v)
	}
	return out
}

// convertValue converts a single value, handling nested maps and slices.
func convertValue(v any) any {
	switch val := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(val))
		for mk, mv := range val {
			m[fmt.Sprintf("%v", mk)] = convertValue(mv)
		}
		return m
	case map[string]any:
		return intfaceKeysToStrings(val)
	case []any:
		s := make([]any, len(val))
		for i, elem := range val {
			s[i] = convertValue(elem)
		}
		return s
	default:
		return v
	}
}
