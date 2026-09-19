# Maintainer constraints

This is a Go + SQLite + embedded WebUI gateway. Preserve these invariants:

1. All management operations use POST /api.json with action/params. Do not add REST management URLs.
2. Runtime business configuration lives in SQLite. No YAML, TOML, or JSON configuration files.
3. Production frontend is embedded through embed.FS. Keep an already-buildable frontend in the repository; no required npm step.
4. Native model protocols preserve vendor fields. Cross-protocol conversion is an explicit supported subset; never silently drop reasoning, state, tool results or signatures.
5. No fallback after streaming starts; no automatic replay of ambiguous network failures or server-side tool execution.
6. Gateway API keys, administrator credentials and upstream API keys are separate. Never return encrypted-secret ciphertext as an API credential or log plaintext secrets.
7. Maintain config version checks, transaction integrity and immutable published snapshots. Do not let the UI or CLI edit SQLite directly.
8. Local rolling budgets are not provider balances. Missing usage is unknown, not zero; retain crash reservations.
9. Do not add multi-account limit evasion, public subscription resale or background quota-burning jobs.
10. Test real handlers with httptest. Run go test ./..., go test -race ./..., go vet ./... before delivery. Keep the README honest about CGo/SQLite build prerequisites and untested cloud integrations.

11. Runtime configuration belongs in SQLite and must be adjustable from the admin UI while the
    process runs. Do not add command-line flags for anything a running gateway should be able to
    change; flags cover the database path, the listening address and bootstrap-only operations.
12. Startup banner output is terminal UI, not log stream. Never route the admin token through slog.
    Management error details may echo submitted configuration, so keep them at DEBUG level.

No third-party Go modules are currently required. Target Go >=1.23. Native Windows is not supported; use WSL2. Tests must not require real provider API keys or the Internet.
