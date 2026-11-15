<script lang="ts">
  import { readAntigravityEligibility } from '../../lib/antigravityEligibility'

  /**
   * Antigravity account-eligibility badge.
   *
   * Google can flag an account as ineligible and require a browser
   * re-verification (reasonCode "VALIDATION_REQUIRED"); the account refuses every
   * model until it is verified. The operator needs to see it at a glance and get
   * the verification link without hunting through the logs — the link is long
   * and single-use, so it is copied with one click and can also be opened.
   *
   * `ineligible` is the raw verdict off the usage response, so the state
   * selection lives in one tested place (readAntigravityEligibility): a missing
   * key means "nobody looked", and the badge stays off.
   */
  let {
    ineligible = null,
    copied = false,
    onCopy,
  }: {
    ineligible?: unknown
    copied?: boolean
    onCopy?: (url: string) => void
  } = $props()

  const verdict = $derived(readAntigravityEligibility(ineligible))
</script>

{#if verdict.state === 'flagged'}
  <div class="mt-1 flex flex-wrap items-center gap-1">
    <span
      class="inline-flex items-center gap-1 rounded-full bg-orange-500/15 px-2 py-0.5 text-[10px] font-semibold text-orange-600 dark:text-orange-400"
      title={`Google flagged this account (${verdict.reasonCode}). It refuses every model until it is verified in a browser.`}
    >
      <span class="material-symbols-outlined text-[12px]">gpp_maybe</span>
      Needs verification
    </span>

    {#if verdict.validationUrl}
      <button
        type="button"
        onclick={() => onCopy?.(verdict.validationUrl as string)}
        title="Copy the verification link"
        class="inline-flex max-w-full cursor-pointer items-center gap-1 rounded-full border border-border-subtle px-2 py-0.5 text-[10px] text-text-muted transition-colors hover:text-primary"
      >
        <span class="material-symbols-outlined text-[12px]">
          {copied ? 'check' : 'content_copy'}
        </span>
        <code class="truncate font-mono">Copy verify link</code>
      </button>
      <a
        href={verdict.validationUrl}
        target="_blank"
        rel="noopener noreferrer"
        title="Open the verification link in a new tab"
        class="inline-flex items-center gap-1 rounded-full border border-border-subtle px-2 py-0.5 text-[10px] text-text-muted transition-colors hover:text-primary"
      >
        <span class="material-symbols-outlined text-[12px]">open_in_new</span>
        Verify
      </a>
    {/if}
  </div>
{/if}
