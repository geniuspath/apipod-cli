package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/geniuspath/apipod-cli/internal/media"
)

// splitPair splits "key=value" on the first '='. A malformed pair is not
// quoted back: it is free text from the command line and may be a secret
// pasted in the wrong place.
func splitPair(flag, pair string) (string, string, error) {
	key, value, ok := strings.Cut(pair, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return "", "", usagef("%s takes key=value", flag)
	}
	return key, value, nil
}

// extras is the parsed form of repeated -x key=value flags.
type extras struct {
	// values are merged into the request body's top level.
	values map[string]any
	// files maps a key to the local paths given as key=@path. Repeating a
	// key collects several files.
	files map[string][]string
	// order keeps file keys in the order they first appeared.
	order []string
}

// listKey reports whether a parameter takes a list of URLs.
func listKey(key string) bool {
	return strings.HasSuffix(key, "_urls") || strings.HasSuffix(key, "_images") || strings.HasSuffix(key, "_videos")
}

// extraKey matches the only parameter names -x accepts. The server decodes
// request fields case-insensitively but prices dimensions by exact key, so a
// key such as "N" or "Duration" would be billed without being estimated, and
// "Callback_URL" would slip past the callback_url filter. Every published
// model parameter is lowercase snake_case; anything else is refused.
var extraKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// parseExtras parses -x flags. A value is decoded as JSON when it is valid
// JSON (5, true, [1,2], {"a":1}, "text") and kept as a string otherwise.
// key=@path marks a local file to upload; its URL becomes the value.
func parseExtras(pairs []string) (extras, error) {
	out := extras{values: map[string]any{}, files: map[string][]string{}}
	for _, pair := range pairs {
		key, value, err := splitPair("-x", pair)
		if err != nil {
			return extras{}, err
		}
		if !extraKey.MatchString(key) {
			return extras{}, usagef("-x: parameter names are lowercase letters, digits and underscores (see `apipod models params <model>`)")
		}
		if strings.HasPrefix(value, "@") {
			path := strings.TrimPrefix(value, "@")
			if path == "" {
				return extras{}, usagef("-x %s=@: missing file path after @", key)
			}
			if _, seen := out.files[key]; !seen {
				out.order = append(out.order, key)
			}
			out.files[key] = append(out.files[key], path)
			continue
		}
		out.values[key] = parseValue(value)
	}
	for key := range out.files {
		if _, both := out.values[key]; both {
			return extras{}, usagef("-x %s: given both as a value and as @file", key)
		}
	}
	return out, nil
}

// parseValue decodes a JSON value, keeping numbers exact; anything that is
// not valid JSON is a plain string.
func parseValue(raw string) any {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return raw
	}
	return v
}

// parseDimensions parses -d flags into the string map the pricing API takes.
func parseDimensions(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, err := splitPair("-d", pair)
		if err != nil {
			return nil, err
		}
		if !extraKey.MatchString(key) {
			return nil, usagef("-d: dimension names are lowercase letters, digits and underscores (see `apipod models params <model>`)")
		}
		out[key] = value
	}
	return out, nil
}

// nonPricingKeys never influence the price and are left out of the estimate.
var nonPricingKeys = map[string]bool{"model": true, "prompt": true, "negative_prompt": true, "callback_url": true}

// pricingDimensions derives the estimate's dimensions from a request body:
// every scalar field becomes a string dimension, the form the pricing API
// expects. Lists, objects and URLs (reference media, nested options) are
// skipped; reference videos reach the estimate through has_video_material.
func pricingDimensions(body map[string]any) map[string]string {
	dims := map[string]string{}
	for key, value := range body {
		if nonPricingKeys[key] {
			continue
		}
		switch v := value.(type) {
		case string:
			// A URL is a media reference, not a pricing parameter.
			if !media.IsRemote(strings.TrimSpace(v)) {
				dims[key] = v
			}
		case json.Number:
			dims[key] = v.String()
		case bool:
			dims[key] = strconv.FormatBool(v)
		case int:
			dims[key] = strconv.Itoa(v)
		case float64:
			dims[key] = strconv.FormatFloat(v, 'f', -1, 64)
		}
	}
	return dims
}

func formatUSD(v float64) string {
	if v != 0 && v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}
