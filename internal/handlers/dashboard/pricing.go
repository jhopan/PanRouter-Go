package dashboard

import (
	"net/http"

	"panrouter/proxy/internal/handlerutil"
	"panrouter/proxy/internal/pricing"
)

// HandleGetPricing serves GET /api/pricing.
//
// Upstream (src/app/api/pricing/route.js) answers getPricing(): the provider →
// model → rate table the pricing editor renders, with the operator's stored
// overrides merged over it. This port has no pricing override store — the
// gateway bills from internal/pricing's compiled tables and nothing writes a
// user rate — so the response is the default table itself. That is the same
// body an install with no overrides gets upstream, and it stays the same shape
// if an override store is added later (merge here, not in the table).
//
// Maps are constants, so no request can mutate what another request reads:
// DefaultPricing hands back a copy.
func (h *DashboardHandler) HandleGetPricing(w http.ResponseWriter, r *http.Request) {
	handlerutil.WriteJSON(w, http.StatusOK, pricing.DefaultPricing())
}
