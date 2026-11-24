package gemini

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// getNestedValue safely walks data (decoded JSON: []any, map[string]any,
// scalars) along path. Returns def when any step cannot be taken.
func getNestedValue(data any, path []any, def any) any {
	cur := data
	for _, key := range path {
		switch k := key.(type) {
		case int:
			arr, ok := cur.([]any)
			if !ok || k < 0 || k >= len(arr) {
				return def
			}
			cur = arr[k]
		case float64:
			arr, ok := cur.([]any)
			if !ok {
				return def
			}
			i := int(k)
			if i < 0 || i >= len(arr) {
				return def
			}
			cur = arr[i]
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return def
			}
			v, ok := m[k]
			if !ok {
				return def
			}
			cur = v
		default:
			return def
		}
	}
	if cur == nil {
		return def
	}
	return cur
}

// asString returns the string value at path, or def.
func asString(data any, path ...any) string {
	if s, ok := getNestedValue(data, path, "").(string); ok {
		return s
	}
	return ""
}

var (
	cardContentRe = regexp.MustCompile(`^https?://googleusercontent\.com/card_content/\d+`)
	artifactsRe   = regexp.MustCompile(`https?://googleusercontent\.com/(?:\w+/)+\d+\n*`)
	flickerEscRe  = regexp.MustCompile(`\\+[` + "`" + `*_~].*$`)
)

// getCleanText mirrors gemini_webapi.utils.parsing.get_clean_text.
func getCleanText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.TrimSuffix(s, "\n```")
	return flickerEscRe.ReplaceAllString(s, "")
}

// getDeltaByFPLen ports get_delta_by_fp_len. newRaw is the provider's latest
// full text; lastSentClean is the cleaned text we last emitted for this
// candidate. Returns (delta, newFullClean).
func getDeltaByFPLen(newRaw, lastSentClean string, isFinal bool) (string, string) {
	newC := newRaw
	if !isFinal {
		newC = getCleanText(newRaw)
	}
	if strings.HasPrefix(newC, lastSentClean) {
		return newC[len(lastSentClean):], newC
	}

	// Find the matching suffix to handle differences gracefully.
	searchLen := min(max(len(lastSentClean), 1000), 3000)
	searchLen = min(searchLen, len(lastSentClean), len(newC))
	if searchLen == 0 {
		return newC, newC
	}
	tailLast := lastSentClean[len(lastSentClean)-searchLen:]
	tailNew := newC[len(newC)-searchLen:]

	// Approximation of difflib.SequenceMatcher's last matching block on the
	// tails: longest common suffix of (tail_last, tail_new). For the append-
	// style deltas Gemini produces this matches SequenceMatcher's behavior.
	if n := commonSuffixLen(tailLast, tailNew); n > 0 {
		return tailNew[len(tailNew)-n:], newC
	}
	// Fallback: longest common suffix of the full strings.
	if n := commonSuffixLen(lastSentClean, newC); n > 0 {
		return newC[len(newC)-n:], newC
	}
	return newC, newC
}

func commonSuffixLen(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[len(a)-1-i] != b[len(b)-1-i] {
			return i
		}
	}
	return n
}

// StreamingFrameParser incrementally parses Google's length-prefixed
// streaming frames. The length marker counts UTF-16 code units, matching
// JavaScript string length semantics used by Google.
type StreamingFrameParser struct {
	buf           string // pending text
	expectedUnits int    // -1 when no frame is in progress
	payloadStart  int    // byte offset of payload within buf
	scannedChars  int    // runes of payload already accounted
	scannedUnits  int
	prefixChecked bool
}

func NewStreamingFrameParser() *StreamingFrameParser {
	p := &StreamingFrameParser{}
	p.resetFrame()
	return p
}

func (p *StreamingFrameParser) resetFrame() {
	p.expectedUnits = -1
	p.payloadStart = 0
	p.scannedChars = 0
	p.scannedUnits = 0
}

// Feed appends decoded stream text and returns all complete JSON frames.
// Each frame's decoded JSON is appended to the result; when a frame decodes
// to a JSON array its elements are appended individually (mirroring the
// Python parser's extend behavior).
func (p *StreamingFrameParser) Feed(content string) []any {
	if content != "" {
		p.buf += content
	}
	p.stripPrefixOnce()

	var frames []any
	for {
		if p.expectedUnits < 0 && !p.readLengthMarker() {
			break
		}
		if p.expectedUnits < 0 {
			break
		}
		p.scanAvailablePayload()
		if p.scannedUnits < p.expectedUnits {
			break
		}
		endPos := p.payloadStart + p.scannedChars
		chunk := p.buf[p.payloadStart:endPos]
		p.buf = p.buf[endPos:]
		p.resetFrame()

		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(chunk), &parsed); err != nil {
			continue
		}
		if arr, ok := parsed.([]any); ok {
			frames = append(frames, arr...)
		} else {
			frames = append(frames, parsed)
		}
	}
	return frames
}

// Flush parses any complete frame left after the final feed.
func (p *StreamingFrameParser) Flush() []any {
	return p.Feed("")
}

func (p *StreamingFrameParser) stripPrefixOnce() {
	if p.prefixChecked {
		return
	}
	const prefix = ")]}'"
	if len(p.buf) < len(prefix) && strings.HasPrefix(prefix, p.buf) {
		return
	}
	if strings.HasPrefix(p.buf, prefix) {
		p.buf = strings.TrimLeft(p.buf[len(prefix):], " \t\n\r\v\f")
	}
	p.prefixChecked = true
}

var lengthMarkerRe = regexp.MustCompile(`^(\d+)\n`)

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

func (p *StreamingFrameParser) readLengthMarker() bool {
	consumed := 0
	for consumed < len(p.buf) && isSpaceByte(p.buf[consumed]) {
		consumed++
	}
	if consumed > 0 {
		p.buf = p.buf[consumed:]
	}
	if len(p.buf) == 0 {
		return false
	}
	m := lengthMarkerRe.FindStringSubmatch(p.buf)
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return false
	}
	p.expectedUnits = n
	p.payloadStart = len(m[1]) // newline after digits counts as a payload unit
	p.scannedChars = 0
	p.scannedUnits = 0
	return true
}

func (p *StreamingFrameParser) scanAvailablePayload() {
	if p.expectedUnits < 0 {
		return
	}
	idx := p.payloadStart + p.scannedChars
	limit := len(p.buf)
	for p.scannedUnits < p.expectedUnits && idx < limit {
		r, size := utf8.DecodeRuneInString(p.buf[idx:])
		units := 1
		if r > 0xFFFF {
			units = 2 // surrogate pair in UTF-16
		}
		if p.scannedUnits+units > p.expectedUnits {
			break
		}
		p.scannedUnits += units
		p.scannedChars += size // byte offset into buf
		idx += size
	}
}

// extractJSONFromResponse ports extract_json_from_response: length-prefixed
// frames first, then a bare JSON document, then NDJSON lines.
func extractJSONFromResponse(text string) ([]any, error) {
	content := strings.TrimPrefix(text, ")]}'")
	content = strings.TrimLeft(content, " \t\n\r\v\f")

	fp := NewStreamingFrameParser()
	result := fp.Feed(content)
	result = append(result, fp.Flush()...)
	if len(result) > 0 {
		return result, nil
	}

	stripped := strings.TrimSpace(content)
	var parsed any
	if err := json.Unmarshal([]byte(stripped), &parsed); err == nil {
		if arr, ok := parsed.([]any); ok {
			return arr, nil
		}
		return []any{parsed}, nil
	}

	var collected []any
	for _, line := range strings.Split(stripped, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p any
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			continue
		}
		switch v := p.(type) {
		case []any:
			collected = append(collected, v...)
		case map[string]any:
			collected = append(collected, v)
		}
	}
	if len(collected) > 0 {
		return collected, nil
	}
	return nil, &APIError{Msg: "Could not find a valid JSON object or array in the response."}
}

// parseCandidate extracts (text, thoughts) from one candidate array,
// porting the text/thoughts parts of GeminiClient._parse_candidate plus the
// Candidate model's HTML entity unescaping.
func parseCandidate(candidate any) (text, thoughts string) {
	text = asString(candidate, 1, 0)
	if cardContentRe.MatchString(text) {
		if alt := asString(candidate, 22, 0); alt != "" {
			text = alt
		}
	}
	text = artifactsRe.ReplaceAllString(text, "")
	thoughts = asString(candidate, 37, 0, 0)
	return html.UnescapeString(text), html.UnescapeString(thoughts)
}
