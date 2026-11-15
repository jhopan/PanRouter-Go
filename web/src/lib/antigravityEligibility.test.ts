import { describe, expect, it } from 'bun:test'
import {
  isAntigravityFlagged,
  readAntigravityEligibility,
  VALIDATION_REQUIRED,
} from './antigravityEligibility'

// The badge has exactly one job: tell the operator that Google flagged the
// account and hand over the verification link. It must therefore distinguish
// three states, and "nobody looked" must never read as "the account is fine".
describe('readAntigravityEligibility', () => {
  it('reads a flagged verdict with its reason and link', () => {
    const verdict = readAntigravityEligibility({
      eligible: false,
      reasonCode: 'VALIDATION_REQUIRED',
      validationUrl: 'https://accounts.google.com/v3/signin/continue?authuser=1',
    })
    expect(verdict).toEqual({
      state: 'flagged',
      reasonCode: 'VALIDATION_REQUIRED',
      validationUrl: 'https://accounts.google.com/v3/signin/continue?authuser=1',
    })
  })

  it('names the documented gate when the payload omitted the reason', () => {
    expect(readAntigravityEligibility({ eligible: false }).reasonCode).toBe(VALIDATION_REQUIRED)
  })

  it('flags without a link when the payload carried none', () => {
    const verdict = readAntigravityEligibility({ eligible: false, reasonCode: 'SOMETHING_ELSE' })
    expect(verdict.state).toBe('flagged')
    expect(verdict.reasonCode).toBe('SOMETHING_ELSE')
    expect(verdict.validationUrl).toBeNull()
  })

  it('reads a healthy verdict', () => {
    expect(readAntigravityEligibility({ eligible: true }).state).toBe('healthy')
  })

  it('treats an absent verdict as unknown, not healthy', () => {
    for (const raw of [undefined, null, {}, [], 'flagged', 0]) {
      expect(readAntigravityEligibility(raw).state).toBe('unknown')
    }
  })

  it('treats a verdict that names no state as unknown', () => {
    expect(readAntigravityEligibility({ reasonCode: 'VALIDATION_REQUIRED' }).state).toBe('unknown')
    expect(readAntigravityEligibility({ eligible: 'false' }).state).toBe('unknown')
  })

  it('ignores a non-string link rather than sending the operator to it', () => {
    expect(readAntigravityEligibility({ eligible: false, validationUrl: 42 }).validationUrl).toBeNull()
    expect(readAntigravityEligibility({ eligible: true, validationUrl: '' }).validationUrl).toBeNull()
  })
})

describe('isAntigravityFlagged', () => {
  it('is true only for a flagged verdict', () => {
    expect(isAntigravityFlagged({ eligible: false })).toBe(true)
    expect(isAntigravityFlagged({ eligible: true })).toBe(false)
    expect(isAntigravityFlagged(undefined)).toBe(false)
  })
})
