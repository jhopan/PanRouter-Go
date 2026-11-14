package providers

import (
	"slices"
	"testing"
)

// TestRegistryModelsCarryUpstreamNewerLanes pins three lanes upstream publishes
// and this table did not, each for a different reason:
//
//   - cline's `cline-free/*` ids: Cline's free tier is a separate product lane
//     from ClinePass, and the ids are gated on Cline product headers rather than
//     on the account (registry/cline.js). Without them a free-tier request
//     resolved to no provider at all.
//   - qoder's `dfmodel_v41`: upstream lists it as paid, and omitting it made a
//     model an account can buy unroutable while the free `dfmodel` worked.
//   - xiaomi-mimo's v2.6 trio: three models the provider serves through their
//     `xiaomi/` upstream ids, absent while the older v2.5 row was present.
//
// The catalogue is what dispatch and the model picker both read, so an id that
// is missing here is missing everywhere; `TestDashboardCatalogListsEveryRoutedModel`
// is what keeps web/src/lib/models.ts in step with it.
func TestRegistryModelsCarryUpstreamNewerLanes(t *testing.T) {
	tests := []struct {
		provider string
		model    string
	}{
		{"cline", "cline-free/muse-spark-1.3-contributor"},
		{"cline", "cline-free/solar-pro4"},
		{"cline", "cline-free/longcat-2.0"},
		{"qoder", "dfmodel_v41"},
		{"qoder-cn", "dfmodel_v41"},
		{"xiaomi-mimo", "mimo-v2.6-pro"},
		{"xiaomi-mimo", "mimo-v2.6-flash"},
		{"xiaomi-mimo", "mimo-v2.6-pro-ultraspeed"},
	}

	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			if !slices.Contains(ProviderModels[tt.provider], tt.model) {
				t.Fatalf("%s is served by the gateway but absent from ProviderModels[%q]", tt.model, tt.provider)
			}
		})
	}
}
