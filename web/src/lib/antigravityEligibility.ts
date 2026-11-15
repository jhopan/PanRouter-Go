/**
 * Antigravity account eligibility — the state the quota tracker's badge renders.
 *
 * Google can flag an account as ineligible and require a browser
 * re-verification (reasonCode "VALIDATION_REQUIRED"). The account refuses every
 * model until it is verified, so the operator has to see it at a glance and get
 * the verification link without hunting through the logs. The gateway publishes
 * the verdict on the connection's usage response
 * (internal/handlers/dashboard/usage_providers.go, upstream
 * src/app/api/usage/[connectionId]/route.js):
 *
 *   {"eligible": false, "reasonCode": "VALIDATION_REQUIRED", "validationUrl": "…"}  → flagged
 *   {"eligible": true}                                                              → healthy
 *   (key absent)                                                                    → unknown
 *
 * "unknown" is not "healthy": the usage API is not asked about eligibility on
 * every poll, so an absent verdict means nobody looked — an operator must not
 * read that as a clean bill of health, and the badge stays off either way.
 */

export type AntigravityEligibilityState = 'flagged' | 'healthy' | 'unknown'

export interface AntigravityEligibilityVerdict {
  state: AntigravityEligibilityState
  /** Reason Google gave; the documented gate when it named none. */
  reasonCode: string
  /** One-use browser verification link, or null when the payload carried none. */
  validationUrl: string | null
}

/** The reason code Google uses when an account must be re-verified. */
export const VALIDATION_REQUIRED = 'VALIDATION_REQUIRED'

/**
 * Reads the eligibility verdict out of a usage response's `ineligible` field.
 * Accepts anything: an absent field, a non-object, or an object with missing
 * members all degrade to `unknown` rather than throwing, because a malformed
 * payload must not blank the page.
 */
export function readAntigravityEligibility(raw: unknown): AntigravityEligibilityVerdict {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) {
    return { state: 'unknown', reasonCode: '', validationUrl: null }
  }
  const verdict = raw as { eligible?: unknown; reasonCode?: unknown; validationUrl?: unknown }
  const validationUrl = typeof verdict.validationUrl === 'string' && verdict.validationUrl ? verdict.validationUrl : null

  if (verdict.eligible === false) {
    const reasonCode = typeof verdict.reasonCode === 'string' && verdict.reasonCode ? verdict.reasonCode : VALIDATION_REQUIRED
    return { state: 'flagged', reasonCode, validationUrl }
  }
  if (verdict.eligible === true) {
    return {
      state: 'healthy',
      reasonCode: typeof verdict.reasonCode === 'string' ? verdict.reasonCode : '',
      validationUrl,
    }
  }
  // A verdict object that names neither state establishes nothing.
  return { state: 'unknown', reasonCode: '', validationUrl: null }
}

/** Convenience for the badge: only a flagged account renders one. */
export function isAntigravityFlagged(raw: unknown): boolean {
  return readAntigravityEligibility(raw).state === 'flagged'
}
