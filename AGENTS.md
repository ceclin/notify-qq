- Use `log/slog` for logging.
- Use `github.com/samber/oops` for error handling.
- Use `github.com/spf13/viper` for configuration.

## Coding Style

- Comments explain why code exists and how to use it, rather than what it does.
- Keep logic self-descriptive through clear naming, functional operators, and
  composition where helpful.
- Do not add unit tests unless explicitly requested. Prefer behavior tests that
  verify observable outcomes, including error paths, and regression tests for
  fixed bugs or previously broken edge cases.
