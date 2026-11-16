/**
 * Text the Automatic Backup panel renders, ported from upstream's profile page
 * (`src/app/(dashboard)/dashboard/profile/page.js`).
 *
 * It lives here rather than inline in the component because the shell text is
 * the fragile part: a trailing backslash is a line continuation, so losing one
 * turns a single command into three that each fail, and the token header is the
 * one argument that must never be re-quoted or line-wrapped. Both are pinned by
 * `backupMirror.test.ts`.
 */

/** The intervals the picker offers; the server clamps to [1, 168] anyway. */
export const BACKUP_INTERVALS: { hours: number; label: string }[] = [
  { hours: 1, label: 'Every hour' },
  { hours: 3, label: 'Every 3 hours' },
  { hours: 6, label: 'Every 6 hours' },
  { hours: 12, label: 'Every 12 hours' },
  { hours: 24, label: 'Every 24 hours (daily)' },
  { hours: 48, label: 'Every 48 hours' },
  { hours: 168, label: 'Every 7 days' },
]

/** A backslash, spelled out: a literal one inside a template literal escapes. */
const SHELL_CONT = String.fromCharCode(92)

/** The token placeholder shown until a real one is pasted in its place. */
export const BACKUP_TOKEN_PLACEHOLDER = '<TOKEN>'

/**
 * One-off download, for a human at a terminal.
 *
 * The origin is the dashboard's own, so the command never points at a guessed
 * host: the mirror pulls from the gateway the operator is looking at. A
 * trailing slash is dropped for the same reason — `//api/backup/download` is
 * answered with a 404 or a redirect by some reverse proxies.
 */
export function backupDownloadCommand(origin: string, token = BACKUP_TOKEN_PLACEHOLDER): string {
  return [
    `curl -H "x-backup-token: ${token}" ${SHELL_CONT}`,
    `  "${origin.replace(/\/+$/, '')}/api/backup/download" ${SHELL_CONT}`,
    '  -o data.sqlite',
  ].join('\n')
}

/** The mirror script, written to disk once and then run from cron. */
export function backupMirrorScript(origin: string, token = BACKUP_TOKEN_PLACEHOLDER): string {
  return [
    '#!/bin/sh',
    `curl -fsS -H "x-backup-token: ${token}" ${SHELL_CONT}`,
    `  "${origin.replace(/\/+$/, '')}/api/backup/download" ${SHELL_CONT}`,
    '  -o "data-$(date +%Y%m%d-%H%M).sqlite"',
  ].join('\n')
}

/** The cron entry that runs that script every six hours. */
export function backupCronLine(): string {
  return '0 */6 * * * /home/<user>/panrouter-backups/mirror.sh >> /home/<user>/panrouter-backups/mirror.log 2>&1'
}
