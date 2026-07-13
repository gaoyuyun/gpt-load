package services

import (
	"testing"

	"gpt-load/internal/config"
)

func TestGetGroupConfigOptionsIncludesStatusCodeOverrides(t *testing.T) {
	service := &GroupService{settingsManager: config.NewSystemSettingsManager()}

	options, err := service.GetGroupConfigOptions()
	if err != nil {
		t.Fatalf("GetGroupConfigOptions returned error: %v", err)
	}

	keys := make(map[string]struct{}, len(options))
	for _, option := range options {
		keys[option.Key] = struct{}{}
	}

	for _, key := range []string{
		"cooldown_status_codes",
		"disable_status_codes",
		"direct_fail_status_codes",
	} {
		if _, ok := keys[key]; !ok {
			t.Fatalf("expected group config option %q", key)
		}
	}
}

func TestValidateAndCleanConfigNormalizesStatusCodeOverrides(t *testing.T) {
	service := &GroupService{settingsManager: config.NewSystemSettingsManager()}

	cleaned, err := service.validateAndCleanConfig(map[string]any{
		"cooldown_status_codes": "503 429，503\n430",
	})
	if err != nil {
		t.Fatalf("validateAndCleanConfig returned error: %v", err)
	}

	if got := cleaned["cooldown_status_codes"]; got != "429,430,503" {
		t.Fatalf("expected normalized cooldown status codes, got %#v", got)
	}
}
