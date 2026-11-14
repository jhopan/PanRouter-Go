package chat

import (
	"testing"

	json "encoding/json/v2"

	"panrouter/proxy/internal/db"
)

// TestNormalizeCapEntryAcceptsComboReference pins the entry shapes a stored
// settings row can carry. Upstream's normalizeCapEntry accepts a combo name
// under either spelling and keeps the inline model list alongside it, so an old
// row and a new one both read.
func TestNormalizeCapEntryAcceptsComboReference(t *testing.T) {
	tests := []struct {
		name    string
		raw     any
		enabled bool
		combo   string
		models  []string
	}{
		{
			name:    "combo name",
			raw:     map[string]any{"enabled": true, "comboName": "vision-pool"},
			enabled: true,
			combo:   "vision-pool",
		},
		{
			name:    "combo alias",
			raw:     map[string]any{"enabled": true, "combo": " vision-pool "},
			enabled: true,
			combo:   "vision-pool",
		},
		{
			name:    "comboName wins over the alias",
			raw:     map[string]any{"comboName": "primary", "combo": "secondary"},
			enabled: true,
			combo:   "primary",
		},
		{
			name:    "legacy inline pool survives",
			raw:     map[string]any{"enabled": true, "models": []any{"ag/gemini-3.8-flash-high"}},
			enabled: true,
			models:  []string{"ag/gemini-3.8-flash-high"},
		},
		{
			name:    "disabled row stays disabled",
			raw:     map[string]any{"enabled": false, "comboName": "vision-pool"},
			enabled: false,
			combo:   "vision-pool",
		},
		{
			name: "non-string combo is not a name",
			raw:  map[string]any{"enabled": true, "comboName": 42},
			// enabled defaults true when the key is absent
			enabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCapEntry(tt.raw)
			if got.Enabled != tt.enabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tt.enabled)
			}
			if got.ComboName != tt.combo {
				t.Errorf("ComboName = %q, want %q", got.ComboName, tt.combo)
			}
			if len(got.Models) != len(tt.models) {
				t.Fatalf("Models = %v, want %v", got.Models, tt.models)
			}
			for i := range tt.models {
				if got.Models[i] != tt.models[i] {
					t.Errorf("Models[%d] = %q, want %q", i, got.Models[i], tt.models[i])
				}
			}
		})
	}
}

// TestAdapterConfigExpandsComboReference pins the resolution the dashboard's
// combo selector depends on: the capability's models come from the named combo,
// in the combo's order, and the inline list is only the fallback for a combo
// that cannot be read.
func TestAdapterConfigExpandsComboReference(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	h := NewChatHandler(db.NewRepo(database))

	modelsJSON, _ := json.Marshal([]string{"ag/gemini-3.8-flash-high", "oc/mimo-v2.6-flash-free"})
	if err := h.Repo.CreateCombo("combo-1", "vision-pool", "llm", string(modelsJSON), "fallback"); err != nil {
		t.Fatalf("create combo: %v", err)
	}

	t.Run("combo supplies the models", func(t *testing.T) {
		entry := h.adapterConfig("vision", map[string]any{
			"vision": map[string]any{"enabled": true, "comboName": "vision-pool"},
		})
		want := []string{"ag/gemini-3.8-flash-high", "oc/mimo-v2.6-flash-free"}
		if len(entry.Models) != len(want) {
			t.Fatalf("Models = %v, want %v", entry.Models, want)
		}
		for i := range want {
			if entry.Models[i] != want[i] {
				t.Errorf("Models[%d] = %q, want %q", i, entry.Models[i], want[i])
			}
		}
	})

	t.Run("the combo wins over an inline list", func(t *testing.T) {
		entry := h.adapterConfig("vision", map[string]any{
			"vision": map[string]any{
				"enabled":   true,
				"comboName": "vision-pool",
				"models":    []any{"ds/deepseek-v4-pro"},
			},
		})
		if len(entry.Models) == 0 || entry.Models[0] != "ag/gemini-3.8-flash-high" {
			t.Errorf("Models = %v, want the combo's leaves", entry.Models)
		}
	})

	t.Run("an unreadable combo falls back to the inline list", func(t *testing.T) {
		entry := h.adapterConfig("vision", map[string]any{
			"vision": map[string]any{
				"enabled":   true,
				"comboName": "does-not-exist",
				"models":    []any{"ds/deepseek-v4-pro"},
			},
		})
		if len(entry.Models) != 1 || entry.Models[0] != "ds/deepseek-v4-pro" {
			t.Errorf("Models = %v, want the inline fallback", entry.Models)
		}
	})

	t.Run("an unreadable combo with no inline list keeps the default pool", func(t *testing.T) {
		entry := h.adapterConfig("vision", map[string]any{
			"vision": map[string]any{"enabled": true, "comboName": "does-not-exist"},
		})
		if len(entry.Models) != 1 || entry.Models[0] != defaultAdapterFallbackModel {
			t.Errorf("Models = %v, want the default fallback model", entry.Models)
		}
	})
}
