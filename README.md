# gomedic

Zero-dependency CLI diagnostics for Go services — turn raw `pprof` data into actionable, business-code-level checkup reports directly in your terminal.

> Work in progress (`v0.1`).

## Motivation

`go tool pprof -http=:8080` works well locally, but debugging a Go service over SSH or inside a Kubernetes container introduces a lot of friction:

- No browser or Graphviz on remote servers; port-forwarding drops when high-load pods restart.
- A single snapshot rarely proves a leak — you have to sample twice, run `-diff_base`, and inspect CPU, Heap, Goroutine, Mutex, and Block separately.
- Raw text dumps are filled with `runtime` and standard library frames instead of highlighting your own application code.
- Interactive web flamegraphs cannot be consumed directly by CI gates or automated scripts.

`gomedic` aims to be a read-only, single-binary diagnostic CLI that samples multiple `pprof` dimensions over a short window, computes deltas, strips runtime noise, and prints a concise report pointing to suspect business code lines.

## Planned Usage

```bash
# Run a 30s multi-dimensional checkup on a running Go service
gomedic check http://127.0.0.1:6060 --duration 30s --pkg github.com/myorg/myapp

# Output Markdown or JSON for CI regression checks
gomedic check http://127.0.0.1:6060 -d 30s --format markdown --fail-on high

# Diff two offline pprof files
gomedic diff --type heap base.pb.gz current.pb.gz --pkg github.com/myorg/myapp
```

## License

[MIT](LICENSE)
