# Ply

Task-based source control, written in Go. The spec defines the product; its concepts and behavior are
requirements, its schemas and command spellings are starting points: @docs/spec.md

Choices made where the spec left room: @docs/decisions.md

## Workflow

- Work is tracked in GitHub issues on `blatessa/ply`, grouped by milestone. Comment on an issue before starting so agents don't collide.
- Reference issues in commits (`Closes #12`). New work you find becomes a new issue, not silent scope.
- `go test ./...` and `go vet ./...` must pass before you commit.
