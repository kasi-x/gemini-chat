package gemini

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Endpoints, with optional /u/<authuser> prefix applied via baseURL.
const (
	appPath       = "/app"
	generatePath  = "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	batchExecPath = "/_/BardChatUi/data/batchexecute"
)

const (
	host             = "https://gemini.google.com"
	grpcGetUserStat  = "otAQ7b"
	defaultLanguage  = "en"
	streamFlagIndex  = 7
	tempChatFlagIdx  = 45
	batchExecHdrKey  = "x-goog-ext-525001261-jspb"
	batchExecHdrBase = `[1,null,null,null,null,null,null,null,[4,5,6,8],null,null,null,null,null,null,null]`
)

var (
	accessTokenRe = regexp.MustCompile(`"SNlM0e":\s*"(.*?)"`)
	buildLabelRe  = regexp.MustCompile(`"cfb2h":\s*"(.*?)"`)
	sessionIDRe   = regexp.MustCompile(`"FdrFJe":\s*"(.*?)"`)
	languageRe    = regexp.MustCompile(`"TuX5cc":\s*"(.*?)"`)
	pushIDRe      = regexp.MustCompile(`"qKIAYe":\s*"(.*?)"`)
)

// defaultMetadata mirrors constants.DEFAULT_METADATA.
func defaultMetadata() []any {
	return []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
}

// ChatSession mirrors the subset of gemini_webapi.ChatSession the CLI uses:
// metadata (10 slots), cid/rid/rcid stored at slots 0/1/2.
type ChatSession struct {
	Metadata []any
}

func NewChatSession() *ChatSession {
	return &ChatSession{Metadata: defaultMetadata()}
}

// ResumeChatSession builds a session from saved state, porting
// ChatSession.__init__ + the cid/rid/rcid setters (which write slots 0-2).
func ResumeChatSession(metadata []any, cid, rid, rcid string) *ChatSession {
	c := NewChatSession()
	if metadata != nil {
		c.Metadata = mergeMetadata(c.Metadata, metadata)
	}
	if cid != "" {
		c.Metadata[0] = cid
	}
	if rid != "" {
		c.Metadata[1] = rid
	}
	if rcid != "" {
		c.Metadata[2] = rcid
	}
	return c
}

// mergeMetadata ports the ChatSession.metadata setter: only the first 10
// slots are updated, and only with non-nil values. The result always has
// 10 slots so positional cid/rid/rcid/context access stays safe.
func mergeMetadata(dst, src []any) []any {
	if len(dst) < 10 {
		d := make([]any, 10)
		copy(d, dst)
		dst = d
	}
	for i := 0; i < 10 && i < len(src); i++ {
		if src[i] != nil {
			dst[i] = src[i]
		}
	}
	return dst
}

// Client is the Go port of the parts of GeminiClient the CLI exercises.
type Client struct {
	HTTP       *http.Client // must carry the cookie jar
	AuthUser   int          // 0 = no /u/N prefix
	language   string
	buildLabel string
	sessionID  string // FdrFJe
	accessTok  string // SNlM0e
	pushID     string
	reqid      int
	sessionid  string // per-client uuid appended to model headers
	models     map[string]*AvailableModel
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func upperUUID() string {
	return strings.ToUpper(strings.ReplaceAll(newUUID(), "-", ""))
}

// baseURL prefixes gemini.google.com paths with /u/<authuser> when needed.
func (c *Client) baseURL(path string) string {
	if c.AuthUser > 0 {
		return fmt.Sprintf("%s/u/%d%s", host, c.AuthUser, path)
	}
	return host + path
}

// Init performs the GET /app token extraction and the user-status RPC that
// populates the model registry. timeout applies to every request.
func (c *Client) Init(ctx context.Context, timeout time.Duration) error {
	c.language = defaultLanguage
	c.pushID = "feeds/mcudyrk2a4khkz"
	c.reqid = 10000 + int(time.Now().UnixNano()%89000)
	c.sessionid = strings.ToUpper(newUUID())

	if err := c.fetchAccessToken(ctx, timeout); err != nil {
		return err
	}
	return c.fetchUserStatus(ctx, timeout)
}

func (c *Client) fetchAccessToken(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL(appPath), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &GeminiError{Msg: fmt.Sprintf("Failed to initialize client: %v", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return &TemporarilyBlockedError{Msg: "Your IP address has been temporarily flagged or blocked by Google (HTTP 429). Please try using a proxy, a different network, or wait for a while before retrying."}
	}
	if resp.StatusCode != 200 {
		return &APIError{Msg: fmt.Sprintf("Init request failed with status code %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	html := string(body)

	extract := func(re *regexp.Regexp) string {
		if m := re.FindStringSubmatch(html); m != nil {
			return m[1]
		}
		return ""
	}
	c.accessTok = extract(accessTokenRe)
	c.buildLabel = extract(buildLabelRe)
	c.sessionID = extract(sessionIDRe)
	if l := extract(languageRe); l != "" {
		c.language = l
	}
	if p := extract(pushIDRe); p != "" {
		c.pushID = p
	}
	if c.accessTok == "" {
		return &GeminiError{Msg: "Failed to initialize client: no access token (SNlM0e) found in /app response"}
	}
	return nil
}

// rpcData is one entry of the f.req payload for batchexecute.
type rpcData struct {
	RPCID      string
	Payload    string
	Identifier string
}

func (r rpcData) serialize() []any {
	return []any{r.RPCID, r.Payload, nil, r.Identifier}
}

// batchExecute posts one RPC batch. Mirrors _batch_execute.
func (c *Client) batchExecute(ctx context.Context, timeout time.Duration, sourcePath string, payloads []rpcData) (string, error) {
	reqid := c.reqid
	c.reqid += 100000

	ids := make([]string, len(payloads))
	for i, p := range payloads {
		ids[i] = p.RPCID
	}
	params := url.Values{}
	params.Set("rpcids", strings.Join(ids, ","))
	params.Set("hl", c.language)
	params.Set("_reqid", strconv.Itoa(reqid))
	params.Set("rt", "c")
	params.Set("source-path", sourcePath)
	if c.buildLabel != "" {
		params.Set("bl", c.buildLabel)
	}
	if c.sessionID != "" {
		params.Set("f.sid", c.sessionID)
	}

	hdrJSON := batchExecHdrBase
	var arr []any
	if err := json.Unmarshal([]byte(hdrJSON), &arr); err == nil {
		arr = append(arr, c.sessionid)
		if b, err := json.Marshal(arr); err == nil {
			hdrJSON = string(b)
		}
	}

	inner := make([]any, len(payloads))
	for i, p := range payloads {
		inner[i] = p.serialize()
	}
	fReqBytes, err := json.Marshal([]any{inner})
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("at", c.accessTok)
	form.Set("f.req", string(fReqBytes))

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL(batchExecPath)+"?"+params.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set(batchExecHdrKey, hdrJSON)
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", &APIError{Msg: fmt.Sprintf("Batch execution failed: %v", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", &APIError{Msg: fmt.Sprintf("Batch execution failed with status code %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// parseRPCResults yields part bodies for targetID, porting _parse_rpc_results.
func (c *Client) parseRPCResults(responseText, targetID string) ([]any, error) {
	parts, err := extractJSONFromResponse(responseText)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, part := range parts {
		if asString(part, 1) != targetID {
			continue
		}
		// Server-side rejection (e.g. code 7 for permission denied).
		if code, ok := getNestedValue(part, []any{5, 0}, nil).(float64); ok && int(code) == 7 {
			return nil, &GeminiError{Msg: "RPC request failed: Permission denied or unauthenticated."}
		}
		bodyStr := asString(part, 2)
		if bodyStr == "" {
			continue
		}
		var body any
		if err := json.Unmarshal([]byte(bodyStr), &body); err != nil {
			continue
		}
		out = append(out, body)
	}
	return out, nil
}

// fetchUserStatus ports _fetch_user_status: discovers the account's model
// registry from the GET_USER_STATUS RPC.
func (c *Client) fetchUserStatus(ctx context.Context, timeout time.Duration) error {
	c.models = map[string]*AvailableModel{}
	respText, err := c.batchExecute(ctx, timeout, "/app", []rpcData{
		{RPCID: grpcGetUserStat, Payload: "[]", Identifier: "generic"},
	})
	if err != nil {
		return err
	}
	bodies, err := c.parseRPCResults(respText, grpcGetUserStat)
	if err != nil {
		return err
	}
	for _, body := range bodies {
		modelsList, _ := getNestedValue(body, []any{15}, nil).([]any)
		if modelsList == nil {
			continue
		}
		tierFlags, _ := getNestedValue(body, []any{16}, []any{}).([]any)
		capFlags, _ := getNestedValue(body, []any{17}, []any{}).([]any)
		capacity, capacityField := computeCapacity(tierFlags, capFlags)
		unauth := false // account status check is best-effort; treat as authed
		parsedAny := false
		for _, md := range modelsList {
			if m := modelFromRPC(md, capacity, capacityField, unauth && parsedAny); m != nil {
				c.models[m.ModelID] = m
				parsedAny = true
			}
		}
		return nil
	}
	return nil
}

// ListModels returns discovered models sorted by model id for stable output.
func (c *Client) ListModels() []*AvailableModel {
	out := make([]*AvailableModel, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out
}

// ResolveModel ports resolve_model.
func (c *Client) ResolveModel(name string) (*AvailableModel, error) {
	return resolveModelByName(c.models, name)
}

// ModelLabel is used in error messages.
func modelLabel(m *AvailableModel) string {
	if m == nil {
		return "the default model"
	}
	if m.ModelName != "" {
		return m.ModelName
	}
	return m.DisplayName
}

// SendMessage ports ChatSession.send_message → generate_content: runs one
// StreamGenerate request, consumes the whole stream, and returns the final
// text/thoughts plus the updated chat session state.
func (c *Client) SendMessage(ctx context.Context, chat *ChatSession, prompt string,
	model *AvailableModel, extendedThinking, temporary bool, timeout time.Duration) (text, thoughts string, err error) {

	if chat == nil {
		chat = NewChatSession()
	}

	reqid := c.reqid
	c.reqid += 100000

	params := url.Values{}
	params.Set("hl", c.language)
	params.Set("_reqid", strconv.Itoa(reqid))
	params.Set("rt", "c")
	if c.buildLabel != "" {
		params.Set("bl", c.buildLabel)
	}
	if c.sessionID != "" {
		params.Set("f.sid", c.sessionID)
	}

	innerReqList := make([]any, 81)
	for i := range innerReqList {
		innerReqList[i] = nil
	}
	innerReqList[0] = []any{prompt, 0, nil, nil, nil, nil, 0}
	innerReqList[1] = []any{c.language}
	innerReqList[2] = chat.Metadata
	innerReqList[6] = []any{1}
	innerReqList[streamFlagIndex] = 1
	innerReqList[10] = 1
	innerReqList[11] = 0
	innerReqList[17] = []any{[]any{0}}
	innerReqList[18] = 0
	innerReqList[27] = 1
	innerReqList[30] = []any{4}
	innerReqList[41] = []any{1}
	if temporary {
		innerReqList[tempChatFlagIdx] = 1
	}
	innerReqList[53] = 0
	innerReqList[61] = []any{}
	innerReqList[68] = 1
	innerReqList[79] = 1
	if extendedThinking {
		innerReqList[80] = 2
	} else {
		innerReqList[80] = 1
	}

	uuidVal := upperUUID()
	innerReqList[59] = uuidVal

	modelHeaders := map[string]string{}
	if model != nil {
		modelHeaders[modelHeaderKey] = model.ModelHeaderJSON()
		for k, v := range model.ExtraHeaders() {
			modelHeaders[k] = v
		}
	}
	if h, ok := modelHeaders[modelHeaderKey]; ok {
		var modelHeader []any
		if err := json.Unmarshal([]byte(h), &modelHeader); err == nil && len(modelHeader) > 0 {
			if n, ok := modelHeader[len(modelHeader)-1].(float64); ok {
				innerReqList[79] = int(n)
			}
			thinkVal := 1
			if extendedThinking {
				thinkVal = 2
			}
			modelHeader = append(modelHeader, thinkVal, c.sessionid)
			if b, err := json.Marshal(modelHeader); err == nil {
				modelHeaders[modelHeaderKey] = string(b)
			}
		}
	}

	innerJSON, err := json.Marshal(innerReqList)
	if err != nil {
		return "", "", err
	}
	// f.req = [null, "<inner json string>"] — inner list JSON-encoded as a
	// string inside the outer array, exactly like the Python client.
	fReqInner, err := json.Marshal(string(innerJSON))
	if err != nil {
		return "", "", err
	}
	fReq := append([]byte("[null,"), fReqInner...)
	fReq = append(fReq, ']')

	form := url.Values{}
	form.Set("at", c.accessTok)
	form.Set("f.req", string(fReq))

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL(generatePath)+"?"+params.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("x-goog-ext-525005358-jspb", fmt.Sprintf(`["%s",1]`, uuidVal))
	for k, v := range modelHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", &APIError{Msg: fmt.Sprintf("Failed to generate contents: %v", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", &APIError{Msg: fmt.Sprintf("Failed to generate contents. Status: %d", resp.StatusCode)}
	}

	label := modelLabel(model)
	parser := NewStreamingFrameParser()

	cid := ""
	if s, ok := chat.Metadata[0].(string); ok {
		cid = s
	}
	rid := ""
	if s, ok := chat.Metadata[1].(string); ok {
		rid = s
	}

	lastTexts := map[string]string{}
	lastThoughts := map[string]string{}
	lastIdxTexts := map[int]string{}
	lastIdxThoughts := map[int]string{}

	var finalText, finalThoughts string
	var finalRCID string
	hasOutput := false

	processParts := func(parts []any) error {
		for _, part := range parts {
			// Fatal error codes at part[5][2][0][1][0].
			if code := asString(part, 5, 2, 0, 1, 0); code != "" {
				codeNum := 0
				if f, ok := getNestedValue(part, []any{5, 2, 0, 1, 0}, nil).(float64); ok {
					codeNum = int(f)
				} else {
					codeNum, _ = strconv.Atoi(code)
				}
				switch codeNum {
				case 1037:
					return &UsageLimitExceededError{Msg: fmt.Sprintf("Usage limit exceeded for model '%s'. You can switch to a different model (e.g., Gemini Flash), or check your account limits on gemini.google.com.", label)}
				case 1050:
					return &ModelInvalidError{Msg: "The specified model is inconsistent with the conversation history. Please ensure you are using the same 'model' parameter throughout the entire ChatSession."}
				case 1052:
					return &ModelInvalidError{Msg: fmt.Sprintf("The model '%s' is currently unavailable or the request structure is outdated. Please update 'gemini-chat' or report this if the problem persists.", label)}
				case 1060:
					return &TemporarilyBlockedError{Msg: "Your IP address has been temporarily flagged or blocked by Google. Please try using a proxy, a different network, or wait for a while before retrying."}
				case 1013:
					return &APIError{Msg: "Gemini encountered a temporary error (1013). Retrying..."}
				default:
					return &APIError{Msg: fmt.Sprintf("Failed to generate contents (stream). Unknown API error code: %d. This might be a temporary Google service issue.", codeNum)}
				}
			}

			innerJSONStr := asString(part, 2)
			if innerJSONStr == "" {
				continue
			}
			var partJSON any
			if err := json.Unmarshal([]byte(innerJSONStr), &partJSON); err != nil {
				continue
			}

			// Chat metadata at part_json[1] → update cid/rid and merge into chat.
			if mData, ok := getNestedValue(partJSON, []any{1}, nil).([]any); ok && len(mData) > 0 {
				if v := asString(mData, 0); v != "" {
					cid = v
				}
				if v := asString(mData, 1); v != "" {
					rid = v
				}
				chat.Metadata = mergeMetadata(chat.Metadata, mData)
			}

			// Final-chunk context string at part_json[25].
			if contextStr, ok := getNestedValue(partJSON, []any{25}, nil).(string); ok {
				newMeta := make([]any, 10)
				for i := range newMeta {
					newMeta[i] = nil
				}
				newMeta[9] = contextStr
				chat.Metadata = mergeMetadata(chat.Metadata, newMeta)
			}

			candidates, _ := getNestedValue(partJSON, []any{4}, []any{}).([]any)
			for i, candidate := range candidates {
				rcid := asString(candidate, 0)
				if rcid == "" {
					continue
				}
				if len(chat.Metadata) < 3 {
					chat.Metadata = append(chat.Metadata, make([]any, 3-len(chat.Metadata))...)
				}
				chat.Metadata[2] = rcid

				text, thoughts := parseCandidate(candidate)
				indicator := getNestedValue(candidate, []any{8, 0}, nil)
				isCompleted := indicator != nil && intVal(indicator) == 2
				isFinal := isCompleted || indicator == nil

				lastSentText := lastTexts[rcid]
				if lastSentText == "" {
					lastSentText = lastIdxTexts[i]
				}
				textDelta, newFullText := getDeltaByFPLen(text, lastSentText, isFinal)
				lastSentThought := lastThoughts[rcid]
				if lastSentThought == "" {
					lastSentThought = lastIdxThoughts[i]
				}
				thoughtsDelta := ""
				newFullThought := ""
				if thoughts != "" {
					thoughtsDelta, newFullThought = getDeltaByFPLen(thoughts, lastSentThought, isFinal)
				}

				if textDelta != "" || thoughtsDelta != "" {
					hasOutput = true
					finalText = text
					finalThoughts = thoughts
					finalRCID = rcid
				}

				lastTexts[rcid] = newFullText
				lastIdxTexts[i] = newFullText
				lastThoughts[rcid] = newFullThought
				lastIdxThoughts[i] = newFullThought
			}
		}
		return nil
	}

	// Read the stream in chunks, feeding the frame parser.
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			parts := parser.Feed(string(buf[:n]))
			if err := processParts(parts); err != nil {
				return "", "", err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", "", &APIError{Msg: fmt.Sprintf("Failed to parse response body from Google (%v). This might be a temporary API change or invalid data.", rerr)}
		}
	}
	parts := parser.Feed("")
	parts = append(parts, parser.Flush()...)
	if err := processParts(parts); err != nil {
		return "", "", err
	}

	if !hasOutput {
		return "", "", &GeminiError{Msg: "Failed to generate contents. No output data found in response."}
	}

	// Persist final chat state: metadata array + cid/rid/rcid slots.
	if finalRCID != "" && len(chat.Metadata) >= 3 {
		chat.Metadata[2] = finalRCID
	}
	_ = cid
	_ = rid
	return finalText, finalThoughts, nil
}

func intVal(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}
