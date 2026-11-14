<script lang="ts">
  // Capacity adapter: a per-capability pool that covers a request the named
  // model cannot handle (an image sent to a text-only model, audio to a model
  // without input support).
  //
  // Ported from upstream's CapacityAdapterCap
  // (src/app/(dashboard)/dashboard/combos/page.js): the pool is a *combo
  // reference*, not an inline list. The combo already owns the models, their
  // order and the fallback/round-robin strategy, so duplicating those controls
  // here meant two places to edit the same pool and two orders that could
  // disagree. A stored row from before the combo reference still works — the
  // server resolves `models` as the fallback — it is just edited in the combo
  // itself now.
  import Card from '../../lib/ui/Card.svelte'
  import Toggle from '../../lib/ui/Toggle.svelte'
  import type { CapacityAdapterState } from './types'

  interface Props {
    capacityAdapter: CapacityAdapterState
    onSaveAdapter: (next: CapacityAdapterState) => void
    /** LLM combos only: a web-search combo is not a chat pool. */
    combos: Array<{ name: string }>
  }

  let { capacityAdapter, onSaveAdapter, combos }: Props = $props()

  function toggleAdapter(type: 'vision' | 'audioInput', enabled: boolean) {
    onSaveAdapter({
      ...capacityAdapter,
      [type]: { ...capacityAdapter[type], enabled },
    })
  }

  function setAdapterCombo(type: 'vision' | 'audioInput', comboName: string) {
    onSaveAdapter({
      ...capacityAdapter,
      [type]: { ...capacityAdapter[type], comboName },
    })
  }
</script>

<div class="flex flex-col gap-3">
  <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
    <div class="min-w-0">
      <p class="text-sm font-medium text-text-main">Vision Adapter</p>
      <p class="text-xs text-text-muted mt-0.5">
        Your model can't read image/audio? Auto-switches to the combo below.
      </p>
    </div>
  </div>
  <div class="flex flex-col gap-4">
    <!-- Vision Adapter Card -->
    <Card padding="sm" class={`group ${!capacityAdapter.vision.enabled ? 'opacity-50' : ''}`}>
      <div class="flex min-w-0 flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div class="flex min-w-0 flex-1 items-start gap-3 sm:items-center">
          <Toggle
            checked={capacityAdapter.vision.enabled}
            onChange={(v) => toggleAdapter('vision', v)}
          />

          <div class="size-8 rounded-lg bg-brand-500/10 flex items-center justify-center shrink-0">
            <span class="material-symbols-outlined text-brand-500 text-[18px]">visibility</span>
          </div>

          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5">
              <code class="font-mono text-sm font-medium text-text-main">Vision</code>
              <span class="text-[10px] text-text-muted">— images (png, jpg, webp, ...)</span>
            </div>
            <div class="mt-1.5 flex flex-wrap items-center gap-1.5">
              <label class="text-[10px] text-text-muted" for="vision-adapter-combo">Combo:</label>
              <select
                id="vision-adapter-combo"
                value={capacityAdapter.vision.comboName}
                onchange={(e) => setAdapterCombo('vision', e.currentTarget.value)}
                disabled={!capacityAdapter.vision.enabled}
                class="h-7 rounded-md border border-border-subtle bg-surface-2 px-2 text-xs text-text-main outline-none transition-colors hover:bg-surface-3 disabled:opacity-60"
              >
                <option value="">None</option>
                {#each combos as combo (combo.name)}
                  <option value={combo.name}>{combo.name}</option>
                {/each}
              </select>
            </div>
          </div>
        </div>
      </div>
    </Card>

    <!-- Audio Adapter Card -->
    <Card padding="sm" class={`group ${!capacityAdapter.audioInput.enabled ? 'opacity-50' : ''}`}>
      <div class="flex min-w-0 flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div class="flex min-w-0 flex-1 items-start gap-3 sm:items-center">
          <Toggle
            checked={capacityAdapter.audioInput.enabled}
            onChange={(v) => toggleAdapter('audioInput', v)}
          />

          <div class="size-8 rounded-lg bg-brand-500/10 flex items-center justify-center shrink-0">
            <span class="material-symbols-outlined text-brand-500 text-[18px]">graphic_eq</span>
          </div>

          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-1.5">
              <code class="font-mono text-sm font-medium text-text-main">Audio</code>
              <span class="text-[10px] text-text-muted">— audio input</span>
            </div>
            <div class="mt-1.5 flex flex-wrap items-center gap-1.5">
              <label class="text-[10px] text-text-muted" for="audio-adapter-combo">Combo:</label>
              <select
                id="audio-adapter-combo"
                value={capacityAdapter.audioInput.comboName}
                onchange={(e) => setAdapterCombo('audioInput', e.currentTarget.value)}
                disabled={!capacityAdapter.audioInput.enabled}
                class="h-7 rounded-md border border-border-subtle bg-surface-2 px-2 text-xs text-text-main outline-none transition-colors hover:bg-surface-3 disabled:opacity-60"
              >
                <option value="">None</option>
                {#each combos as combo (combo.name)}
                  <option value={combo.name}>{combo.name}</option>
                {/each}
              </select>
            </div>
          </div>
        </div>
      </div>
    </Card>
  </div>
</div>