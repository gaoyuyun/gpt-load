package errors

import (
	"fmt"
	"gpt-load/internal/models"
	"gpt-load/internal/types"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var validationStatusPattern = regexp.MustCompile(`^\[status (\d{3})\]\s*`)

// KeyFailureDecision describes how a failed upstream response should affect retries and key state.
type KeyFailureDecision struct {
	Action            string `json:"action"`
	StatusCode        int    `json:"status_code"`
	ErrorMessage      string `json:"error_message"`
	OriginalError     string `json:"original_error"`
	MatchedStatusCode int    `json:"matched_status_code"`
	Retryable         bool   `json:"retryable"`
}

// ClassifyKeyFailure classifies an upstream failure into retry/key-management behavior.
// parsedError is the extracted message for classification; originalError is the raw upstream response for storage.
func ClassifyKeyFailure(statusCode int, parsedError string, originalError string, settings types.SystemSettings) KeyFailureDecision {
	decision := KeyFailureDecision{
		Action:        models.KeyActionNormalFailure,
		StatusCode:    statusCode,
		ErrorMessage:  truncateString(strings.TrimSpace(originalError), maxErrorBodyLength),
		OriginalError: truncateString(strings.TrimSpace(originalError), maxErrorBodyLength),
		Retryable:     true,
	}

	if matchedCode := matchConfiguredStatusCode(statusCode, settings.DisableStatusCodes); matchedCode != 0 || hasPaymentRequiredKeywords(parsedError) {
		decision.Action = models.KeyActionAutoDisable
		decision.MatchedStatusCode = matchedCode
		return decision
	}

	if matchedCode := matchConfiguredStatusCode(statusCode, settings.CooldownStatusCodes); matchedCode != 0 || hasRateLimitKeywords(parsedError) {
		decision.Action = models.KeyActionCooldown
		decision.MatchedStatusCode = matchedCode
		return decision
	}

	if matchedCode := matchConfiguredStatusCode(statusCode, settings.DirectFailStatusCodes); matchedCode != 0 || hasDirectFailKeywords(parsedError) {
		decision.Action = models.KeyActionDirectFail
		decision.MatchedStatusCode = matchedCode
		decision.Retryable = false
		return decision
	}

	return decision
}

// ParseStatusCodeList parses a comma/whitespace separated list of HTTP status codes.
func ParseStatusCodeList(raw string) ([]int, error) {
	parts := splitStatusCodeTokens(raw)
	if len(parts) == 0 {
		return nil, nil
	}

	seen := make(map[int]struct{}, len(parts))
	codes := make([]int, 0, len(parts))
	for _, part := range parts {
		code, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid status code %q", part)
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("status code out of range: %d", code)
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}

	sort.Ints(codes)
	return codes, nil
}

// NormalizeStatusCodeList deduplicates and canonicalizes a code list.
func NormalizeStatusCodeList(raw string) (string, error) {
	codes, err := ParseStatusCodeList(raw)
	if err != nil {
		return "", err
	}
	if len(codes) == 0 {
		return "", nil
	}

	normalized := make([]string, 0, len(codes))
	for _, code := range codes {
		normalized = append(normalized, strconv.Itoa(code))
	}
	return strings.Join(normalized, ","), nil
}

// ValidateStatusCodeLists validates syntax and prevents overlaps across behavior buckets.
func ValidateStatusCodeLists(cooldownCodes, disableCodes, directFailCodes string) error {
	categories := []struct {
		name  string
		value string
	}{
		{name: "cooldown", value: cooldownCodes},
		{name: "disable", value: disableCodes},
		{name: "direct_fail", value: directFailCodes},
	}

	owners := make(map[int]string)
	for _, category := range categories {
		codes, err := ParseStatusCodeList(category.value)
		if err != nil {
			return fmt.Errorf("%s status codes: %w", category.name, err)
		}
		for _, code := range codes {
			if owner, exists := owners[code]; exists {
				return fmt.Errorf("status code %d is configured in both %s and %s", code, owner, category.name)
			}
			owners[code] = category.name
		}
	}

	return nil
}

// ExtractStatusCodeAndMessage parses "[status 402] xxx" style validator errors.
func ExtractStatusCodeAndMessage(err error) (int, string) {
	if err == nil {
		return 0, ""
	}

	message := strings.TrimSpace(err.Error())
	match := validationStatusPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0, message
	}

	statusCode, convErr := strconv.Atoi(match[1])
	if convErr != nil {
		return 0, message
	}

	cleanedMessage := strings.TrimSpace(validationStatusPattern.ReplaceAllString(message, ""))
	return statusCode, cleanedMessage
}

func matchConfiguredStatusCode(statusCode int, raw string) int {
	if statusCode <= 0 {
		return 0
	}

	codes, err := ParseStatusCodeList(raw)
	if err != nil {
		return 0
	}

	for _, code := range codes {
		if code == statusCode {
			return code
		}
	}

	return 0
}

func splitStatusCodeTokens(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == ';' || r == '|' || r == '，' || r == '、'
	})
}

func hasPaymentRequiredKeywords(message string) bool {
	lowerMsg := strings.ToLower(message)
	keywords := []string{
		"payment required",
		"payment_required",
		"insufficient funds",
		"insufficient_funds",
		"insufficient credits",
		"insufficient_credits",
		"credit too low",
		"credits too low",
		"balance too low",
		"insufficient balance",
		"billing",
		"out of credits",
		"no credits",
		"account suspended",
		"subscription expired",
	}

	for _, keyword := range keywords {
		if strings.Contains(lowerMsg, keyword) {
			return true
		}
	}

	return false
}

func hasRateLimitKeywords(message string) bool {
	lowerMsg := strings.ToLower(message)
	keywords := []string{
		"rate limit",
		"rate_limit",
		"too many requests",
		"too_many_requests",
		"requests per",
		"throttled",
		"slow down",
		"resource has been exhausted",
	}

	for _, keyword := range keywords {
		if strings.Contains(lowerMsg, keyword) {
			return true
		}
	}

	return false
}

func hasDirectFailKeywords(message string) bool {
	lowerMsg := strings.ToLower(message)
	keywords := []string{
		"request too large",
		"payload too large",
		"content too large",
		"context length exceeded",
		"maximum context length",
		"maximum context size",
		"prompt is too long",
		"too many tokens",
		"reduce the length of the messages",
	}

	for _, keyword := range keywords {
		if strings.Contains(lowerMsg, keyword) {
			return true
		}
	}

	return false
}
