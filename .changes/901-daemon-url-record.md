### fix(cli): `status` and `stop` use the address the daemon actually bound

Found by running the binary rather than reading it. Start a daemon with
`PORT=20990`, then run `panrouter-go status` from a shell that does not export
`PORT`: it reported `http://127.0.0.1:20130`, the default, and `stop` sent its
graceful-shutdown request to that same wrong port. The daemon was alive and
serving the whole time — the commands were answering a different question.

The pid file proves *which* process is the daemon; it never said *where* that
process listens. Both commands were recomputing the port from their own
environment instead of asking the process.

- `internal/daemon`: the daemon's address is recorded next to the pid claim
  (`run/gateway.url`) when `Start` sees it healthy, removed with the claim on
  stop/unregister, and `daemonLocalURL` prefers it over a recomputed one.
- `panrouter-go status` prints the recorded address, falling back to the
  configured one only when there is no record.
- `writeURL` creates the run dir itself, so a restart after a manual cleanup
  cannot lose the record.

Verified by hand, which is the point: `PORT=20990 … start` then `env -u PORT
./bin/panrouter-go.exe status` now prints `http://127.0.0.1:20990`, and
`env -u PORT ./bin/panrouter-go.exe stop` stops the daemon and frees the port
(both were wrong before the change). `TestRecordedURLRecordsWhatTheDaemonBound`
pins the record round-trip, the precedence in `daemonLocalURL`, that a blank
write cannot erase it, and that `clearURL` removes it.

Also smoke-tested end to end on the built binary (v1.9.17-exp.2): `/health`,
`/api/health`, `/api/init`, `/dashboard` 200; `/api/locale` 405 (upstream has
POST only); `/v1/*` and `/v1beta/models` 401 without a key and 200 with one;
`GET /v1beta/models` returns 1837 models in Gemini shape on both spellings;
`POST /v1beta/models/…:generateContent` reaches the engine and answers a
Google-shaped 502 when no provider connection exists.