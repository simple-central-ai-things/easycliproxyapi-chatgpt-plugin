package upstream

// sentinel.go ports the reference's sentinel chat-requirements handshake
// (openai_backend_api.py _bootstrap + _get_chat_requirements): warm up the
// homepage to extract the PoW script references, build the legacy
// requirements token, POST prepare, solve whatever the challenge mix
// demands (PoW, turnstile — refuse arkose), then POST finalize for the
// token bundle every conversation/image run carries.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/simple-central-ai-things/easycliproxyapi-chatgpt-plugin/internal/model"
)

// SentinelTokens is the token bundle one sentinel handshake acquires. An
// empty string means not required / not acquired.
type SentinelTokens struct {
	// ChatRequirements is the finalize "token" every upstream call needs.
	ChatRequirements string
	// Proof is the mined "gAAAAAB" proof token ("" when proofofwork is not required).
	Proof string
	// Turnstile is the VM token ("" when turnstile is not required).
	Turnstile string
	// SO is the finalize "so_token".
	SO string
}

// SentinelError is the typed failure contract for the handshake. Code is
// stable (later tasks map it to RPC error codes); Retryable drives host-side
// retry decisions; HTTPStatus is the upstream status when the error came
// from one. Message text never carries upstream body bytes (secret rule).
type SentinelError struct {
	Code       string
	HTTPStatus int
	Retryable  bool
	message    string
}

func (e *SentinelError) Error() string {
	return e.Code + ": " + e.message
}

var (
	// ErrChallengeArkoseUnsupported refuses the arkose challenge: the
	// reference raises RuntimeError there; this provider never attempts it.
	ErrChallengeArkoseUnsupported = &SentinelError{
		Code:      "challenge_arkose_unsupported",
		message:   "the upstream challenges with arkose, which this provider does not solve",
		Retryable: false,
	}
	// ErrSentinelDenied is a 403 at any handshake step.
	ErrSentinelDenied = &SentinelError{
		Code:       "sentinel_denied",
		HTTPStatus: http.StatusForbidden,
		message:    "the upstream denied the sentinel handshake",
		Retryable:  false,
	}
	// ErrSentinelMissingToken is a finalize response without "token".
	ErrSentinelMissingToken = &SentinelError{
		Code:      "sentinel_missing_token",
		message:   "finalize returned no chat requirements token",
		Retryable: false,
	}
	// ErrSentinelMalformed is a non-JSON body where the handshake expects JSON.
	ErrSentinelMalformed = &SentinelError{
		Code:      "sentinel_malformed",
		message:   "the upstream returned a malformed sentinel response",
		Retryable: false,
	}
	// ErrSentinelProofFailed is a proof-of-work unsolved within the limit.
	ErrSentinelProofFailed = &SentinelError{
		Code:      "sentinel_proof_failed",
		message:   "proof-of-work was not solved within the attempt limit",
		Retryable: false,
	}
)

// buildLegacyToken builds the legacy requirements token. AcquireSentinel uses
// its return both as the prepare body "p" and as the turnstile XOR key — the
// reference's single p_token. Tests swap this seam for a deterministic value
// so the turnstile dx (which is XORed with that token upstream) decodes.
var buildLegacyToken = func(ua string, sources []string, dataBuild string) (string, error) {
	return BuildLegacyRequirementsToken(BuildPowConfig(ua, sources, dataBuild))
}

// sentinelPrepareRequest is the exact prepare body the oracle posts.
type sentinelPrepareRequest struct {
	P string `json:"p"`
}

// sentinelFinalizeRequest is the exact finalize body the oracle posts —
// field order included (pinned by TestSentinelHappyPath).
type sentinelFinalizeRequest struct {
	PrepareToken   string `json:"prepare_token"`
	ProofToken     string `json:"proof_token"`
	TurnstileToken string `json:"turnstile_token"`
}

// AcquireSentinel runs the full handshake through u. cred supplies the
// wire fingerprint and (unless anon) the bearer token; anon switches both
// the path family (/backend-anon/) and drops Authorization, mirroring the
// reference's access-token branch.
func AcquireSentinel(ctx context.Context, u Upstream, cred model.Credential, anon bool) (SentinelTokens, error) {
	ua := sentinelUserAgent(&cred)

	// 1. Bootstrap: warm the homepage and extract the PoW references. The
	// bootstrap carries the reference's page-load headers — never the
	// session headers and never Authorization.
	home, err := u.Do(ctx, Req{
		Method:  http.MethodGet,
		URL:     chatGPTBaseURL + "/",
		Headers: bootstrapHeaders(ua),
	})
	if err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel bootstrap: %w", err)
	}
	if err := sentinelStepError("bootstrap", home.StatusCode); err != nil {
		return SentinelTokens{}, err
	}
	scriptSources, dataBuild := parsePowResources(string(home.Body))

	// 2. Legacy requirements token from the 25-field fingerprint config.
	legacyToken, err := buildLegacyToken(ua, scriptSources, dataBuild)
	if err != nil {
		return SentinelTokens{}, err
	}

	base := "/backend-api/sentinel/chat-requirements"
	if anon {
		base = "/backend-anon/sentinel/chat-requirements"
	}

	// 3. Prepare.
	preparePath := base + "/prepare"
	prepareHeaders := SessionHeaders(&cred, preparePath)
	if !anon {
		prepareHeaders = Authed(&cred, prepareHeaders)
	}
	prepareBody, err := json.Marshal(sentinelPrepareRequest{P: legacyToken})
	if err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel prepare body: %w", err)
	}
	prepare, err := u.Do(ctx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + preparePath,
		Headers:     prepareHeaders,
		Body:        prepareBody,
		ContentType: "application/json",
	})
	if err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel prepare: %w", err)
	}
	if err := sentinelStepError("prepare", prepare.StatusCode); err != nil {
		return SentinelTokens{}, err
	}
	var prepareData struct {
		PrepareToken string          `json:"prepare_token"`
		Arkose       json.RawMessage `json:"arkose"`
		ProofOfWork  json.RawMessage `json:"proofofwork"`
		Turnstile    json.RawMessage `json:"turnstile"`
	}
	if err := json.Unmarshal(prepare.Body, &prepareData); err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel prepare: %w", ErrSentinelMalformed)
	}

	// 4. The challenge mix. arkose is refused typed and never attempted;
	// PoW and turnstile are solved when (and only when) required.
	if arkose, ok := sentinelObject(prepareData.Arkose); ok && sentinelTruthy(arkose["required"]) {
		return SentinelTokens{}, fmt.Errorf("sentinel prepare: %w", ErrChallengeArkoseUnsupported)
	}

	proofToken := ""
	if powInfo, ok := sentinelObject(prepareData.ProofOfWork); ok && sentinelTruthy(powInfo["required"]) {
		seed, _ := powInfo["seed"].(string)
		difficulty, _ := powInfo["difficulty"].(string)
		proof, solved := MineProof(seed, difficulty, BuildPowConfig(ua, scriptSources, dataBuild), sentinelPowMaxTries)
		if !solved {
			return SentinelTokens{}, fmt.Errorf("sentinel prepare: %w", ErrSentinelProofFailed)
		}
		proofToken = proof
	}

	turnstileToken := ""
	if tsInfo, ok := sentinelObject(prepareData.Turnstile); ok && sentinelTruthy(tsInfo["required"]) {
		dx, _ := tsInfo["dx"].(string)
		if dx != "" {
			// The oracle XORs the dx payload with the legacy token itself.
			turnstileToken, err = SolveTurnstile(dx, legacyToken)
			if err != nil {
				return SentinelTokens{}, err
			}
		}
	}

	// 5. Finalize.
	finalizePath := base + "/finalize"
	finalizeHeaders := SessionHeaders(&cred, finalizePath)
	if !anon {
		finalizeHeaders = Authed(&cred, finalizeHeaders)
	}
	finalizeBody, err := json.Marshal(sentinelFinalizeRequest{
		PrepareToken:   prepareData.PrepareToken,
		ProofToken:     proofToken,
		TurnstileToken: turnstileToken,
	})
	if err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel finalize body: %w", err)
	}
	finalize, err := u.Do(ctx, Req{
		Method:      http.MethodPost,
		URL:         chatGPTBaseURL + finalizePath,
		Headers:     finalizeHeaders,
		Body:        finalizeBody,
		ContentType: "application/json",
	})
	if err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel finalize: %w", err)
	}
	if err := sentinelStepError("finalize", finalize.StatusCode); err != nil {
		return SentinelTokens{}, err
	}
	var finalized struct {
		Token   string `json:"token"`
		SoToken string `json:"so_token"`
	}
	if err := json.Unmarshal(finalize.Body, &finalized); err != nil {
		return SentinelTokens{}, fmt.Errorf("sentinel finalize: %w", ErrSentinelMalformed)
	}
	if finalized.Token == "" {
		// The oracle embeds the response body in its error text; this port
		// keeps error text free of upstream payload (secret rule).
		return SentinelTokens{}, fmt.Errorf("sentinel finalize: %w", ErrSentinelMissingToken)
	}

	return SentinelTokens{
		ChatRequirements: finalized.Token,
		Proof:            proofToken,
		Turnstile:        turnstileToken,
		SO:               finalized.SoToken,
	}, nil
}

// sentinelStepError maps one upstream status the way the oracle's ensure_ok
// would have surfaced it: 2xx passes, 403 is the typed denial, 429/5xx are
// retryable unavailability, everything else non-retryable.
func sentinelStepError(step string, status int) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusForbidden {
		return fmt.Errorf("sentinel %s: %w", step, ErrSentinelDenied)
	}
	retryable := status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	return fmt.Errorf("sentinel %s: %w", step, &SentinelError{
		Code:       "sentinel_unavailable",
		HTTPStatus: status,
		Retryable:  retryable,
		message:    fmt.Sprintf("the upstream returned HTTP %d", status),
	})
}

func sentinelObject(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// sentinelTruthy is Python truthiness over JSON values (the oracle's
// `.get("required")` checks).
func sentinelTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

// sentinelUserAgent is the UA both the headers and the PoW config carry:
// the credential's stored fingerprint UA when set, else the pinned default
// (same rule as SessionHeaders).
func sentinelUserAgent(cred *model.Credential) string {
	if cred != nil && strings.TrimSpace(cred.Fp.UserAgent) != "" {
		return cred.Fp.UserAgent
	}
	return defaultUserAgent
}

// bootstrapHeaders is the oracle's _bootstrap_headers page-load set.
func bootstrapHeaders(ua string) http.Header {
	h := make(http.Header, 11)
	h.Set("User-Agent", ua)
	h.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Sec-Ch-Ua", defaultSecChUa)
	h.Set("Sec-Ch-Ua-Mobile", defaultSecChUaMobile)
	h.Set("Sec-Ch-Ua-Platform", defaultSecChUaPlatform)
	h.Set("Sec-Fetch-Dest", "document")
	h.Set("Sec-Fetch-Mode", "navigate")
	h.Set("Sec-Fetch-Site", "none")
	h.Set("Sec-Fetch-User", "?1")
	h.Set("Upgrade-Insecure-Requests", "1")
	return h
}

// The oracle's homepage scan (pow.py ScriptSrcParser): every <script> tag
// with a src attribute contributes its source in document order; the first
// c/<something>/_ pattern found in ANY src becomes data-build (later srcs
// overwrite earlier ones); with no script srcs the default PoW script is
// used, and with no c/ pattern the <html data-build="..."> attribute is the
// fallback. Attributes may be single- or double-quoted, like html.parser.
// (RE2 has no backreferences, so the two quote styles get their own pattern
// and the matches merge back into document order by start offset.)
var (
	scriptSrcDoubleRe  = regexp.MustCompile(`<script\b[^>]*?\bsrc\s*=\s*"(.*?)"[^>]*>`)
	scriptSrcSingleRe  = regexp.MustCompile(`<script\b[^>]*?\bsrc\s*=\s*'(.*?)'[^>]*>`)
	htmlDataBuildRe    = regexp.MustCompile(`<html[^>]*data-build="([^"]*)"`)
	powScriptPatternRe = regexp.MustCompile(`c/[^/]*/_`)
)

// parsePowResources is the differential port of pow.py parse_pow_resources.
// One fixture-unreachable divergence: the port matches <script src=...>
// case-sensitively, while the oracle's html.parser lowercases tag and
// attribute names (so <SCRIPT SRC=...> also matches it); the fixtures never
// carry an upper-cased tag, so the port keeps the simpler match.
func parsePowResources(html string) ([]string, string) {
	type srcMatch struct {
		start int
		src   string
	}
	var matches []srcMatch
	for _, re := range []*regexp.Regexp{scriptSrcDoubleRe, scriptSrcSingleRe} {
		for _, loc := range re.FindAllStringSubmatchIndex(html, -1) {
			matches = append(matches, srcMatch{start: loc[0], src: html[loc[2]:loc[3]]})
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })

	var sources []string
	dataBuild := ""
	for _, m := range matches {
		sources = append(sources, m.src)
		if pattern := powScriptPatternRe.FindString(m.src); pattern != "" {
			dataBuild = pattern // last match wins, like the oracle's parser
		}
	}
	if len(sources) == 0 {
		sources = []string{powDefaultScript}
	}
	if dataBuild == "" {
		if match := htmlDataBuildRe.FindStringSubmatch(html); match != nil {
			dataBuild = match[1]
		}
	}
	return sources, dataBuild
}
