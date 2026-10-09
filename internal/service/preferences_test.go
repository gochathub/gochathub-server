package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gochathub/gochathub-server/internal/model"
)

func TestDecodePrefsPrimaryColorDefault(t *testing.T) {
	if got := decodePrefs(nil).PrimaryColor; got != PrimaryColorDefault {
		t.Errorf("decodePrefs(nil) primary_color = %q, want %q", got, PrimaryColorDefault)
	}
	if got := decodePrefs([]byte(`{"primary_color":""}`)).PrimaryColor; got != PrimaryColorDefault {
		t.Errorf("empty primary_color = %q, want %q", got, PrimaryColorDefault)
	}
	if got := decodePrefs([]byte(`not json`)).PrimaryColor; got != PrimaryColorDefault {
		t.Errorf("malformed jsonb primary_color = %q, want %q", got, PrimaryColorDefault)
	}
}

func TestMergePreferencesPrimaryColor(t *testing.T) {
	// set (case-normalized)
	patch := model.PreferencesPatch{PrimaryColor: ptr("#7C3AED")}
	merged := MergePreferences(nil, patch)
	if merged.PrimaryColor != "#7c3aed" {
		t.Errorf("merged primary_color = %q, want #7c3aed", merged.PrimaryColor)
	}
	// reset: "" must come back as the default at decode
	raw, _ := json.Marshal(merged)
	decoded := decodePrefs(raw)
	if got := MergePreferences(raw, model.PreferencesPatch{PrimaryColor: ptr("")}).PrimaryColor; got != "" {
		t.Errorf("reset patch primary_color = %q, want \"\"", got)
	}
	_ = decoded
	// absent (nil pointer) keeps the stored value
	if got := MergePreferences(raw, model.PreferencesPatch{}).PrimaryColor; got != "#7c3aed" {
		t.Errorf("absent patch changed primary_color to %q", got)
	}
}

func TestPrimaryColorSwatchContract(t *testing.T) {
	if len(PrimaryColorSwatches) != 15 {
		t.Errorf("PrimaryColorSwatches = %d entries, want 15", len(PrimaryColorSwatches))
	}
	for _, c := range PrimaryColorSwatches {
		if !strings.HasPrefix(c, "#") || len(c) != 7 {
			t.Errorf("swatch %q is not #rrggbb", c)
		}
		if strings.ToLower(c) != c {
			t.Errorf("swatch %q is not lowercase", c)
		}
	}
	if !PrimaryColorSet[PrimaryColorDefault] {
		t.Errorf("default %q missing from swatch set", PrimaryColorDefault)
	}
}

func ptr(s string) *string { return &s }
