# Contributing to Kuberture

## Reporting Bugs

Open a [GitHub issue](https://github.com/lexfrei/kuberture/issues/new?template=bug_report.md) with a clear description, steps to reproduce, and expected vs actual behavior.

## Suggesting Features

Open a [GitHub issue](https://github.com/lexfrei/kuberture/issues/new?template=feature_request.md) describing the feature and the problem it solves.

## Development Setup

```bash
go build ./...
go test --race ./...
golangci-lint run
```

## Pull Request Process

1. Fork the repository and create a feature branch.
2. Write tests first (TDD is required).
3. Ensure all tests pass and linting is clean before submitting.
4. Open a pull request against `master` with a clear description of your changes.
