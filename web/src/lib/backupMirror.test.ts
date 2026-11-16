import { describe, expect, it } from 'bun:test'
import {
  BACKUP_INTERVALS,
  backupCronLine,
  backupDownloadCommand,
  backupMirrorScript,
} from './backupMirror'

describe('backup mirror commands', () => {
  it('keeps each command on one shell line through the continuations', () => {
    const script = backupMirrorScript('http://localhost:20130', 'bk-any-abcdef-01234567')
    const lines = script.split('\n')
    expect(lines[0]).toBe('#!/bin/sh')
    // Every line but the last has to end in a backslash, or the shell runs the
    // fragment on its own and the download never happens.
    for (const line of lines.slice(1, -1)) {
      expect(line.endsWith('\\')).toBe(true)
    }
    expect(lines.at(-1)?.endsWith('\\')).toBe(false)
    expect(lines.at(-1)).toContain('date +%Y%m%d-%H%M')
  })

  it('carries the token in the header the download route reads', () => {
    const token = 'bk-any-abcdef-01234567'
    expect(backupDownloadCommand('http://localhost:20130', token)).toContain(`x-backup-token: ${token}`)
    expect(backupMirrorScript('http://localhost:20130', token)).toContain(`x-backup-token: ${token}`)
  })

  it('points at the dashboard origin, without doubling the slash', () => {
    for (const origin of ['http://localhost:20130', 'http://localhost:20130/']) {
      expect(backupDownloadCommand(origin)).toContain('"http://localhost:20130/api/backup/download"')
      expect(backupMirrorScript(origin)).toContain('"http://localhost:20130/api/backup/download"')
    }
  })

  it('shows a placeholder until a token exists', () => {
    // A command the operator can read before minting a token must not look like
    // a working credential.
    expect(backupDownloadCommand('http://localhost:20130')).toContain('x-backup-token: <TOKEN>')
  })

  it('mentions the mirror path and schedule in the cron line', () => {
    const line = backupCronLine()
    expect(line.startsWith('0 */6 * * * ')).toBe(true)
    expect(line).toContain('mirror.sh')
  })
})

describe('backup interval options', () => {
  it('offers hourly to weekly, with daily present', () => {
    const hours = BACKUP_INTERVALS.map((o) => o.hours)
    expect(hours).toEqual([...hours].sort((a, b) => a - b))
    expect(new Set(hours).size).toBe(hours.length)
    expect(hours[0]).toBe(1)
    expect(hours).toContain(24)
    expect(hours.at(-1)).toBe(168)
  })

  it('every option stays inside the band the server accepts', () => {
    // ResolveIntervalHours clamps to [1, 168]; an option outside it would save
    // as a different number than the picker shows.
    for (const option of BACKUP_INTERVALS) {
      expect(option.hours).toBeGreaterThanOrEqual(1)
      expect(option.hours).toBeLessThanOrEqual(168)
    }
  })
})
