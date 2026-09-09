# Contributing to Bifrost

Thanks for your interest in contributing! Bifrost is a small project — contributions are very welcome, especially around selector maintenance and stability improvements.

## Before You Start

- **Open an issue first** for any non-trivial change so we can discuss the approach before you invest time coding
- For bug fixes and selector updates, feel free to go straight to a PR

## Development Setup

```bash
git clone https://github.com/yourname/bifrost.git
cd bifrost

# Build
go build ./...

# Vet
go vet ./...

# Run tests
go test ./...

# Run locally (needs Chromium in PATH and a logged-in session)
go run . serve
```

## Most Wanted Contributions

### 1. Selector Updates (easiest)

The Gemini UI changes frequently. If something broke:

```bash
# Dump the current DOM to see what changed
go run . inspect > /tmp/dom.json

# Or with Docker:
docker exec bifrost bifrost inspect
```

Find the new selector, update `geminiSelectors` in [`gemini.go`](gemini.go), and open a PR with the inspect output.

### 2. New Model Support

When Google adds new Gemini models:

```bash
# List all modes the UI currently has
go run . modes
```

Update the model registry in `openai.go` with any new entries.

### 3. Bug Reports

Please include:
- Output of `bifrost inspect` (or `docker exec bifrost bifrost inspect`)
- Output of `bifrost test "hello"` if the issue is response-related
- Docker/OS version
- What you expected vs. what happened

### 4. Test Coverage

The project has no automated tests yet — any test is a welcome addition. Most critical areas:

- `parseToolCalls` in `openai.go` — highly branchy, easy to break
- `repairJSON` / `findCodeBlocks` — parsing logic
- `SerializeMessages` — message serialization
- `looksLikeRefusal` / `looksLikeMissedToolCall` — detection heuristics

## Code Style

- Standard `gofmt` formatting (no custom style)
- `go vet` must pass
- Comments in English
- Keep the single-binary / zero-external-dependency philosophy — no new frameworks

## Pull Request Checklist

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] If you touched selectors: include `bifrost inspect` output showing the new selector works
- [ ] If you added a feature: update README.md

## What We Won't Accept

- Changes that require Node.js, Python, or other runtimes
- Adding a database or persistent state beyond the Chrome profile
- Breaking the OpenAI API compatibility
- Changes that require a paid Gemini API key

## License

By contributing, you agree that your contributions will be licensed under the MIT License.
