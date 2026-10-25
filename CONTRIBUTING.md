# Contributing

## Workflow

1. Branch from `main`: `feat/<topic>`, `fix/<topic>`, `docs/<topic>`, or `chore/<topic>`.
2. Make small commits that each do one thing.
3. Before pushing, run `make verify`. It must exit 0: lint, Go and Python tests, the E2E simulation, and
   `verification/report.md` with no FAIL or NO-TEST requirement.
4. Open a merge request (pull request) into `main` and describe what changed and how it was verified.
5. Release by tagging `main` with `vMAJOR.MINOR.PATCH` and running `make package`; keep `VERSION` in the
   Makefile, `version` in `agent/cmd/gpumon-agent/main.go`, and the versions in `central/pyproject.toml` and
   `central/gpumon_central/__init__.py` in step.

## Commit messages

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):

```
<type>(<optional scope>): <summary in the imperative, lower case, no period>

<optional body: what and why, wrapped at 72 columns>

<optional footer: BREAKING CHANGE: ..., Refs: ...>
```

Types: `feat` (new behavior), `fix` (bug fix), `docs`, `test`, `refactor`, `perf`, `build` (packaging,
dependencies), `ci`, `chore` (tooling, repository maintenance), `style` (formatting only).

Scopes: `agent`, `central`, `web`, `schema`, `deploy`, `scripts`, `docs`.

Examples:

```
feat(central): add daily automatic CSV export
fix(deploy): pin the export timer to the dashboard time zone
docs: describe the firewall rules for the VPN pool
```

A change to the payload format is a breaking change for mixed agent and central versions: update
`schema/payload.schema.json`, regenerate the examples with `go test ./internal/contract -update` (in `agent/`),
and mark the commit with `BREAKING CHANGE:` in the footer.

## Rules from the specification

- Code, comments, logs, UI text, and documentation are English and ASCII-only (`scripts/ascii_check.py`).
- Every requirement in `SPEC.md` keeps at least one test that references its ID; never skip, weaken, or
  delete a test to make it pass.
- No runtime network access except agent-to-central; vendor frontend assets.
