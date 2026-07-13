package config

import (
	"testing"

	"gorm.io/datatypes"
)

func TestGetEffectiveConfigAppliesGroupStatusCodeOverrides(t *testing.T) {
	manager := NewSystemSettingsManager()

	effective := manager.GetEffectiveConfig(datatypes.JSONMap{
		"cooldown_status_codes":    "503",
		"disable_status_codes":     "401",
		"direct_fail_status_codes": "400",
	})

	if effective.CooldownStatusCodes != "503" {
		t.Fatalf("expected cooldown status codes override, got %q", effective.CooldownStatusCodes)
	}
	if effective.DisableStatusCodes != "401" {
		t.Fatalf("expected disable status codes override, got %q", effective.DisableStatusCodes)
	}
	if effective.DirectFailStatusCodes != "400" {
		t.Fatalf("expected direct-fail status codes override, got %q", effective.DirectFailStatusCodes)
	}
}

func TestNormalizeStatusCodeSettings(t *testing.T) {
	normalized, err := NormalizeStatusCodeSettings(map[string]any{
		"cooldown_status_codes":    "503 429，503\n430",
		"disable_status_codes":     "402",
		"direct_fail_status_codes": "413",
	})
	if err != nil {
		t.Fatalf("NormalizeStatusCodeSettings returned error: %v", err)
	}

	if got := normalized["cooldown_status_codes"]; got != "429,430,503" {
		t.Fatalf("expected normalized cooldown status codes, got %#v", got)
	}
}

func TestValidateGroupConfigOverridesRejectsEffectiveStatusCodeOverlap(t *testing.T) {
	manager := NewSystemSettingsManager()

	err := manager.ValidateGroupConfigOverrides(map[string]any{
		"cooldown_status_codes": "402",
	})
	if err == nil {
		t.Fatal("expected overlap between cooldown override and default disable status codes")
	}
}
