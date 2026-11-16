<script lang="ts">
  // Automatic backup + mirror tokens.
  //
  // Port of the two cards in upstream's profile page
  // (src/app/(dashboard)/dashboard/profile/page.js): the scheduled snapshot to a
  // private GitHub repo, and the download tokens a second machine uses to pull
  // the database itself. The backend was already ported (internal/dbbackup +
  // /api/backup/*); this is the panel that drives it.
  import Card from '../lib/ui/Card.svelte'
  import Toggle from '../lib/ui/Toggle.svelte'
  import Button from '../lib/ui/Button.svelte'
  import Input from '../lib/ui/Input.svelte'
  import { api, type BackupMirrorToken } from '../api/client'
  import { copyToClipboard } from '../lib/clipboard'
  import {
    BACKUP_INTERVALS,
    BACKUP_TOKEN_PLACEHOLDER,
    backupCronLine,
    backupDownloadCommand,
    backupMirrorScript,
  } from '../lib/backupMirror'

  interface Props {
    /** Overrides the origin in the shell snippets; tests pass one. */
    origin?: string
  }

  let { origin = '' }: Props = $props()

  // The scheduled GitHub snapshot.
  let enabled = $state(false)
  let intervalHours = $state(24)
  let githubUser = $state('')
  // The operator sees a URL here; the server parses either shape.
  let githubRepo = $state('')
  let githubToken = $state('')
  let tokenConfigured = $state(false)
  let saving = $state(false)
  let backingUp = $state(false)
  let status = $state<{ type: '' | 'error' | 'success'; message: string }>({ type: '', message: '' })

  // The mirror credentials.
  let tokens = $state<BackupMirrorToken[]>([])
  let tokenName = $state('')
  let newToken = $state('')
  let mirrorBusy = $state(false)
  let mirrorStatus = $state<{ type: '' | 'error' | 'success'; message: string }>({ type: '', message: '' })

  let resolvedOrigin = $state(origin)

  async function loadConfig() {
    try {
      const cfg = await api.getBackupConfig()
      enabled = cfg.enabled
      intervalHours = cfg.intervalHours || 24
      githubUser = cfg.githubUser || ''
      // Show the full URL so the field can be pasted straight into GitHub.
      githubRepo = cfg.githubRepoUrl || cfg.githubRepo || ''
      githubToken = ''
      tokenConfigured = cfg.githubTokenConfigured
    } catch {
      // Upstream leaves the panel empty here: a failed read is not worth an
      // error box, and the global 401 handler already redirects to login.
    }
  }

  async function loadTokens() {
    try {
      const res = await api.getBackupTokens()
      tokens = Array.isArray(res.tokens) ? res.tokens : []
    } catch {
      /* the panel simply stays empty */
    }
  }

  $effect(() => {
    if (!resolvedOrigin && typeof window !== 'undefined') resolvedOrigin = window.location.origin
    void loadConfig()
    void loadTokens()
  })

  async function handleSave() {
    saving = true
    status = { type: '', message: '' }
    try {
      await api.saveBackupConfig({ enabled, intervalHours, githubUser, githubRepo, githubToken })
      githubToken = ''
      status = { type: 'success', message: 'Backup settings saved' }
      await loadConfig()
    } catch (err) {
      status = { type: 'error', message: err instanceof Error ? err.message : 'Failed to save the backup settings' }
    } finally {
      saving = false
    }
  }

  async function handleRunNow() {
    backingUp = true
    status = { type: '', message: '' }
    try {
      const res = await api.runBackup()
      status = {
        type: 'success',
        message: `Backup pushed — ${res.fileName} (${(res.bytes / 1024 / 1024).toFixed(2)} MB)`,
      }
    } catch (err) {
      status = { type: 'error', message: err instanceof Error ? err.message : 'The backup failed' }
    } finally {
      backingUp = false
    }
  }

  async function handleCreateToken() {
    mirrorBusy = true
    mirrorStatus = { type: '', message: '' }
    try {
      const res = await api.createBackupToken({ name: tokenName })
      newToken = res.token || ''
      tokenName = ''
      mirrorStatus = { type: 'success', message: 'Token created — copy it now, it is shown only once' }
      await loadTokens()
    } catch (err) {
      mirrorStatus = { type: 'error', message: err instanceof Error ? err.message : 'Failed to create the token' }
    } finally {
      mirrorBusy = false
    }
  }

  async function handleSetActive(id: string, isActive: boolean) {
    mirrorStatus = { type: '', message: '' }
    try {
      await api.setBackupTokenActive(id, isActive)
      await loadTokens()
    } catch (err) {
      mirrorStatus = { type: 'error', message: err instanceof Error ? err.message : 'Failed to update the token' }
    }
  }

  async function handleDeleteToken(id: string) {
    mirrorStatus = { type: '', message: '' }
    try {
      await api.deleteBackupToken(id)
      mirrorStatus = { type: 'success', message: 'Token deleted' }
      await loadTokens()
    } catch (err) {
      mirrorStatus = { type: 'error', message: err instanceof Error ? err.message : 'Failed to delete the token' }
    }
  }
</script>

<Card padding="md" class="space-y-5">
  <div class="flex items-center justify-between pb-2 border-b border-border">
    <div class="flex items-center gap-2">
      <span class="material-symbols-outlined text-brand-500 text-[18px]">cloud_upload</span>
      <h2 class="text-sm font-bold text-text-main">Automatic Backup</h2>
    </div>
    <span
      class="text-[10px] font-mono font-bold px-2 py-0.5 rounded border {enabled
        ? 'bg-success/10 text-success border-success/20'
        : 'bg-surface-2 text-text-muted border-border'}"
    >
      {enabled ? `every ${intervalHours}h` : 'off'}
    </span>
  </div>

  <!-- SECTION: the schedule itself. Off by default: nothing is pushed to GitHub
       until an operator turns it on and supplies a token. -->
  <div class="flex items-center justify-between gap-3 p-3 rounded-xl bg-bg border border-border">
    <div class="min-w-0">
      <p class="text-xs font-semibold text-text-main">Daily backup to GitHub</p>
      <p class="text-[11px] text-text-muted pt-0.5">
        This server pushes a snapshot of the database to a private repository on its own, using the
        token below. The repository must be private — a public one would publish every credential.
        Nothing else needs to touch GitHub.
      </p>
    </div>
    <Toggle
      checked={enabled}
      disabled={saving}
      label="Automatic backup"
      onChange={(value) => (enabled = value)}
    />
  </div>

  <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
    <div class="flex flex-col gap-1.5">
      <label class="text-xs font-medium text-text-main" for="backup-interval">Backup interval</label>
      <select
        id="backup-interval"
        bind:value={intervalHours}
        disabled={saving}
        class="w-full h-9 px-3 rounded-[10px] bg-bg border border-border text-xs text-text-main focus:outline-none focus:border-brand-500"
      >
        {#each BACKUP_INTERVALS as option (option.hours)}
          <option value={option.hours}>{option.label}</option>
        {/each}
      </select>
      <p class="text-xs text-text-muted">
        The snapshot is a full copy of the database, so the shortest sensible interval is hourly.
      </p>
    </div>

    <Input
      id="backup-github-user"
      label="GitHub username"
      bind:value={githubUser}
      placeholder="jhopan"
      disabled={saving}
    />

    <Input
      id="backup-github-repo"
      label="Repository"
      bind:value={githubRepo}
      placeholder="https://github.com/jhopan12/AutoBuy.git"
      hint="Paste the repository URL straight from GitHub. A plain name also works."
      disabled={saving}
    />
  </div>

  <Input
    id="backup-github-token"
    type="password"
    label="GitHub token"
    bind:value={githubToken}
    disabled={saving}
    placeholder={tokenConfigured ? '•••••••• (saved — leave blank to keep)' : 'ghp_...'}
    hint="A fine-grained token with write access to the repository. It is stored on the server and never shown again."
  />

  <div class="flex flex-col sm:flex-row gap-2">
    <Button variant="secondary" icon="save" loading={saving} onclick={handleSave}>Save</Button>
    <Button
      variant="outline"
      icon="cloud_upload"
      loading={backingUp}
      disabled={saving}
      onclick={handleRunNow}
    >
      Back up now
    </Button>
  </div>

  {#if status.message}
    <p class="text-xs {status.type === 'error' ? 'text-danger' : 'text-success'}">{status.message}</p>
  {/if}

  <!-- SECTION: the one-off download command. The token is a placeholder until a
       real one is minted below, so a copied command is never mistaken for a
       working credential. -->
  <div class="p-3 rounded-xl bg-bg border border-border space-y-2">
    <p class="text-xs font-semibold text-text-main">Download the database once</p>
    <p class="text-[11px] text-text-muted">
      Run this on the machine that keeps a copy off this box. It writes the raw SQLite file straight
      into the current directory.
    </p>
    <div class="relative">
      <pre class="text-[11px] font-mono text-text-main bg-surface-2 border border-border rounded-lg p-2.5 pr-10 overflow-x-auto whitespace-pre">{backupDownloadCommand(
          resolvedOrigin,
        )}</pre>
      <button
        type="button"
        title="Copy"
        class="absolute top-1.5 right-1.5 p-1 rounded text-text-muted hover:text-text-main hover:bg-surface-3 cursor-pointer"
        onclick={() => copyToClipboard(backupDownloadCommand(resolvedOrigin))}
      >
        <span class="material-symbols-outlined text-[16px]">content_copy</span>
      </button>
    </div>
  </div>

  <!-- SECTION: the mirror tokens. Each machine gets its own, so one can be
       revoked without touching the others. -->
  <div class="p-3 rounded-xl bg-bg border border-border space-y-3">
    <div>
      <p class="text-xs font-semibold text-text-main">Mirror the database on a schedule</p>
      <p class="text-[11px] text-text-muted pt-0.5">
        A machine pulls the database straight from this server with its own backup token. The GitHub
        copy is separate — the server pushes that itself.
      </p>
    </div>

    <div class="flex flex-col sm:flex-row gap-2 sm:items-end">
      <Input
        id="backup-token-name"
        class="flex-1"
        label="Token name"
        bind:value={tokenName}
        placeholder="debian-mirror"
        disabled={mirrorBusy}
      />
      <Button variant="secondary" icon="add" loading={mirrorBusy} onclick={handleCreateToken}>
        Create token
      </Button>
    </div>

    {#if newToken}
      <div class="p-3 rounded-xl bg-warning/10 border border-warning/30 space-y-2">
        <p class="text-xs font-semibold text-text-main">New token — copy it now</p>
        <p class="text-[11px] text-text-muted">
          It is shown only once. The server keeps just the key id and a checksum, so a database copy
          never carries mirror access with it.
        </p>
        <div class="flex items-center gap-2">
          <code class="flex-1 text-[11px] font-mono break-all p-2 rounded bg-surface-2 border border-border">{newToken}</code>
          <button
            type="button"
            title="Copy"
            class="p-1 rounded text-text-muted hover:text-text-main hover:bg-surface-3 cursor-pointer"
            onclick={() => copyToClipboard(newToken)}
          >
            <span class="material-symbols-outlined text-[16px]">content_copy</span>
          </button>
        </div>
      </div>
    {/if}

    {#if tokens.length > 0}
      <div class="space-y-2">
        {#each tokens as t (t.id)}
          <div class="flex flex-col sm:flex-row sm:items-center gap-2 p-3 rounded-xl bg-surface border border-border">
            <div class="min-w-0 flex-1">
              <p class="text-xs font-semibold text-text-main truncate">{t.name}</p>
              <p class="text-[11px] text-text-muted font-mono truncate">
                bk-...-{t.keyId}{t.machineId ? ` · ${t.machineId}` : ''}
              </p>
              <p class="text-[10px] text-text-subtle">
                created {String(t.createdAt || '').slice(0, 10)}{t.lastUsedAt
                  ? ` · last used ${String(t.lastUsedAt).slice(0, 16).replace('T', ' ')}`
                  : ' · never used'}
              </p>
            </div>
            <div class="flex items-center gap-2">
              <span
                class="rounded-full px-2 py-0.5 text-[10px] font-semibold border {t.isActive
                  ? 'bg-success/10 text-success border-success/20'
                  : 'bg-surface-2 text-text-muted border-border'}"
              >
                {t.isActive ? 'active' : 'revoked'}
              </span>
              <Button variant="outline" size="sm" onclick={() => handleSetActive(t.id, !t.isActive)}>
                {t.isActive ? 'Revoke' : 'Re-enable'}
              </Button>
              <Button variant="ghost" size="sm" onclick={() => handleDeleteToken(t.id)}>Delete</Button>
            </div>
          </div>
        {/each}
      </div>
    {/if}

    {#if mirrorStatus.message}
      <p class="text-xs {mirrorStatus.type === 'error' ? 'text-danger' : 'text-success'}">{mirrorStatus.message}</p>
    {/if}

    <div class="space-y-3 pt-1">
      <div>
        <p class="text-xs font-semibold text-text-main">Set it up on the mirror machine</p>
        <p class="text-[11px] text-text-muted pt-0.5">
          All this machine needs is curl and a cron entry. It never touches GitHub.
        </p>
      </div>

      <div class="space-y-1">
        <p class="text-[11px] font-medium text-text-muted">2 — Run it every N hours</p>
        <div class="relative">
          <pre class="text-[11px] font-mono text-text-main bg-surface-2 border border-border rounded-lg p-2.5 pr-10 overflow-x-auto whitespace-pre">{backupMirrorScript(
              resolvedOrigin,
            )}</pre>
          <button
            type="button"
            title="Copy"
            class="absolute top-1.5 right-1.5 p-1 rounded text-text-muted hover:text-text-main hover:bg-surface-3 cursor-pointer"
            onclick={() => copyToClipboard(backupMirrorScript(resolvedOrigin))}
          >
            <span class="material-symbols-outlined text-[16px]">content_copy</span>
          </button>
        </div>
        <p class="text-[11px] text-text-subtle">
          Save it as mirror.sh, then make it executable with: chmod +x mirror.sh
        </p>
      </div>

      <div class="space-y-1">
        <p class="text-[11px] font-medium text-text-muted">3 — Schedule it (cron)</p>
        <div class="relative">
          <pre class="text-[11px] font-mono text-text-main bg-surface-2 border border-border rounded-lg p-2.5 pr-10 overflow-x-auto whitespace-pre">{backupCronLine()}</pre>
          <button
            type="button"
            title="Copy"
            class="absolute top-1.5 right-1.5 p-1 rounded text-text-muted hover:text-text-main hover:bg-surface-3 cursor-pointer"
            onclick={() => copyToClipboard(backupCronLine())}
          >
            <span class="material-symbols-outlined text-[16px]">content_copy</span>
          </button>
        </div>
        <p class="text-[11px] text-text-subtle">
          Run crontab -e and paste this line. Adjust the path and the schedule.
        </p>
      </div>
    </div>
  </div>

  <p class="text-[11px] text-text-subtle">
    The token in the snippets above is the literal placeholder {BACKUP_TOKEN_PLACEHOLDER} — replace it
    with one of the mirror tokens minted here.
  </p>
</Card>
